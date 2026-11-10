// Package furaffinity — not a booru; scrapes the site's own search/gallery
// pages. FA exposes no public API, so this engine reads the same HTML the
// browser gets (the modern template, the guest default). A search/gallery page
// is a list of <figure id="sid-…" class="r-<rating> t-<type> u-<user>"> blocks
// carrying the id, rating, submission type, author and thumbnail — enough for
// the grid. The file itself only exists on the submission page (/view/<id>/),
// so posts leave the search with an empty File.URL and are resolved lazily via
// ExpandPost (download-time) and PrepareView (the viewer), memoised in an LRU.
package furaffinity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"massdownloader/internal/core"
)

var (
	figRe      = regexp.MustCompile(`(?is)<figure\b.*?</figure>`)
	sidRe      = regexp.MustCompile(`id="sid-(\d+)"`)
	classRe    = regexp.MustCompile(`class="([^"]*)"`)
	thumbRe    = regexp.MustCompile(`src="(//t\.furaffinity\.net/[^"]+)"`)
	userRe     = regexp.MustCompile(`href="/user/([^/"#?]+)`)
	favcountRe = regexp.MustCompile(`favcount:(>=|<=|>|<|=)?(\d+)`)
	// upload epoch baked into every file URL: /art/<user>/<epoch>/<epoch>.<name>
	artEpochRe = regexp.MustCompile(`/art/[^/]+/(?:stories/|music/|poetry/)?(\d{9,11})/`)
	dlRe       = regexp.MustCompile(`(?s)class="download"[^>]*>\s*<a[^>]*href="([^"]+)"`)
	subImgRe   = regexp.MustCompile(`<img[^>]*\bid="submissionImg"[^>]*>`)
	fullviewRe = regexp.MustCompile(`data-fullview-src="([^"]+)"`)
	ratingBox  = regexp.MustCompile(`class="rating-box\s+(\w+)"`)
	favSpanRe  = regexp.MustCompile(`(?s)class="favorites"[^>]*>\s*<span[^>]*>\s*([\d,\s]+?)<`)
	popupDate  = regexp.MustCompile(`(?s)class="popup_date"[^>]*title="([^"]*)"[^>]*>([^<]*)<`)
	tagsRow    = regexp.MustCompile(`(?s)class="tags-row"(.*?)</section>`)
	tagLink    = regexp.MustCompile(`>([^<>]+)</a>`)
	subCont    = regexp.MustCompile(`(?s)submission-id-sub-container(.*?)</div>`)
	atSize     = regexp.MustCompile(`@\d+-`)
)

var ratingByClass = map[string]string{
	"r-general": "s", "r-mature": "q", "r-adult": "e",
}

// figure classes only tell images/flash/audio/text apart — enough for an
// extension guess (the real one arrives with the view page).
var extByClass = map[string]string{
	"t-image": "jpg", "t-flash": "swf", "t-audio": "mp3", "t-text": "txt",
}

var ratingWords = map[string]string{
	"general": "s", "mature": "q", "adult": "e",
}

// ratingsFor lists which form checkboxes a rating choice ticks ("" -> all).
var ratingsFor = map[string][]string{
	"":   {"general", "mature", "adult"},
	"s":  {"general"},
	"q":  {"mature"},
	"e":  {"adult"},
	"qe": {"mature", "adult"},
}

var types = []string{"art", "music", "flash", "story", "photo", "poetry"}

var rangesSet = map[string]bool{
	"1day": true, "3days": true, "7days": true, "30days": true,
	"90days": true, "1year": true, "3years": true, "5years": true,
}

// orders maps sort token -> the form's (order-by, order-direction).
var orders = map[string][2]string{
	"":           {"relevancy", "desc"},
	"date":       {"date", "desc"},
	"date_asc":   {"date", "asc"},
	"popularity": {"popularity", "desc"},
}

const detailCacheMax = 512

// abs upgrades FA's protocol-relative CDN links.
func abs(u string) string {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

// faDate parses popup_date text -> epoch (0 when unparsable). The
// attribute/text pair swaps with the account's «full date» setting.
func faDate(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "on ") {
		s = strings.TrimSpace(s[3:])
	}
	for _, layout := range []string{"Jan 2, 2006 3:04 PM", "Jan 2, 2006, 3:04 PM"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

type viewInfo struct {
	DL      string
	Full    string
	Rating  string
	Faves   int64
	Created int64
	Tags    []string
	Author  string
}

// ParseViewPage lifts what the app needs from a /view/<id>/ page: the
// download URL, fullview render, rating, favourites, upload time, tags and
// author. Every field is optional — FA's markup drifts, so each is matched on
// its own and a miss degrades that field, never the whole post.
func ParseViewPage(htmlText string) viewInfo {
	var out viewInfo
	if m := dlRe.FindStringSubmatch(htmlText); m != nil {
		out.DL = abs(m[1])
	}
	if m := subImgRe.FindString(htmlText); m != "" {
		if fm := fullviewRe.FindStringSubmatch(m); fm != nil {
			out.Full = abs(fm[1])
		}
	}
	if m := ratingBox.FindStringSubmatch(htmlText); m != nil {
		if r, ok := ratingWords[strings.ToLower(m[1])]; ok {
			out.Rating = r
		}
	}
	if m := favSpanRe.FindStringSubmatch(htmlText); m != nil {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, m[1])
		out.Faves, _ = strconv.ParseInt(digits, 10, 64)
	}
	em := artEpochRe.FindStringSubmatch(out.DL + " " + out.Full)
	if em != nil {
		out.Created, _ = strconv.ParseInt(em[1], 10, 64)
	} else if m := popupDate.FindStringSubmatch(htmlText); m != nil {
		out.Created = faDate(m[1])
		if out.Created == 0 {
			out.Created = faDate(m[2])
		}
	}
	if m := tagsRow.FindStringSubmatch(htmlText); m != nil {
		for _, t := range tagLink.FindAllStringSubmatch(m[1], -1) {
			if s := strings.TrimSpace(t[1]); s != "" {
				out.Tags = append(out.Tags, s)
			}
		}
	}
	if m := subCont.FindStringSubmatch(htmlText); m != nil {
		if um := userRe.FindStringSubmatch(m[1]); um != nil {
			out.Author = um[1]
		}
	}
	return out
}

// Filters mirrors app/sites/furaffinity/filters.py.
func Filters() core.FilterList {
	return core.FilterList{
		// «Автор» for FA: a username whose gallery is walked page by page;
		// each value becomes a by:<name> token. No autocomplete.
		{Kind: core.KindAuthors, Key: "artists", Label: "Автор (галерея)",
			Placeholder:  "имя автора… (пусто — поиск по словам)",
			Autocomplete: "artists",
			Tooltip: "Показать галерею автора FurAffinity целиком\n" +
				"(страница /gallery/имя/). Введите имя как в URL профиля.\n" +
				"Учитывается только первый автор; если задан — ключевые\n" +
				"слова и настройки поиска (тип, период…) игнорируются."},
		{Kind: core.KindTags, Key: "include",
			Label:        "Ключевые слова / теги",
			Placeholder:  "слово, тег… (пусто — по автору)",
			Autocomplete: "tags"},
		{Kind: core.KindExclude, Key: "exclude", Label: "Исключить слова",
			Placeholder: "исключить тег…", Autocomplete: "tags",
			Tooltip: "Слова добавляются в запрос со знаком «-»\n" +
				"(работает в режиме «расширенный» — по умолчанию)."},
		{Kind: core.KindThreshold, Key: "faves", Label: "Избранное (мин.)",
			Token: "favcount", DefaultValue: 50,
			Minimum: -1000, Maximum: 1_000_000,
			Ops: "≥", OpTokens: ">=",
			Tooltip: "показывать работы с числом добавлений\n" +
				"в избранное не меньше заданного.\n" +
				"Фильтр на стороне приложения: для каждой\n" +
				"работы догружается её страница — медленно"},
		{Kind: core.KindChoice, Key: "rating", Label: "Рейтинг", Token: "rating",
			Options: []core.ChoiceOption{
				{Label: "Любой", Value: ""}, {Label: "General", Value: "s"},
				{Label: "Mature", Value: "q"}, {Label: "Adult", Value: "e"},
				{Label: "Mature + Adult", Value: "qe"},
			},
			Tooltip: "рейтинг FurAffinity (галочки формы поиска).\n" +
				"Mature/Adult видны только с вставленным Cookie\n" +
				"залогиненного аккаунта (окно «Авторизация»)."},
		{Kind: core.KindChoice, Key: "type", Label: "Тип работы", Token: "type",
			Options: []core.ChoiceOption{
				{Label: "Любой", Value: ""}, {Label: "Art", Value: "art"},
				{Label: "Music", Value: "music"}, {Label: "Flash", Value: "flash"},
				{Label: "Story", Value: "story"}, {Label: "Photos", Value: "photo"},
				{Label: "Poetry", Value: "poetry"},
			},
			Tooltip: "тип публикации (параметр поиска FA).\n" +
				"При просмотре галереи автора не применяется."},
		{Kind: core.KindChoice, Key: "range", Label: "Период", Token: "range",
			Options: []core.ChoiceOption{
				{Label: "Всё время", Value: ""}, {Label: "1 день", Value: "1day"},
				{Label: "3 дня", Value: "3days"}, {Label: "Неделя", Value: "7days"},
				{Label: "Месяц", Value: "30days"}, {Label: "3 месяца", Value: "90days"},
				{Label: "Год", Value: "1year"}, {Label: "3 года", Value: "3years"},
				{Label: "5 лет", Value: "5years"},
			},
			Tooltip: "ограничить поиск работами за период\n" +
				"(параметр range формы поиска FA)."},
		{Kind: core.KindChoice, Key: "sort", Label: "Сортировка", Token: "order",
			Options: []core.ChoiceOption{
				{Label: "Релевантность", Value: ""},
				{Label: "Сначала новые", Value: "date"},
				{Label: "Сначала старые", Value: "date_asc"},
				{Label: "Популярность", Value: "popularity"},
			},
			Tooltip: "порядок результатов поиска FA\n(order-by + order-direction)."},
		{Kind: core.KindChoice, Key: "mode", Label: "Совпадение слов",
			Token: "mode",
			Options: []core.ChoiceOption{
				{Label: "Расширенный (по умолчанию)", Value: ""},
				{Label: "Все слова", Value: "all"},
				{Label: "Любое из слов", Value: "any"},
			},
			Tooltip: "как трактовать несколько слов запроса.\n" +
				"«Расширенный» понимает операторы -слово,\n" +
				"| (или), \"фраза\" — синтаксис поиска FA."},
		core.ImagesOnlyFilter("Только изображения (без flash/видео)",
			"Не скачивать видео и анимацию.\nСнимите галочку, чтобы качать и ролики/GIF."),
	}
}

// Engine ports FurAffinityEngine.
type Engine struct {
	core.EngineBase
	mu       sync.Mutex
	metadata *core.MetadataCache
}

func NewEngine() *Engine {
	return &Engine{metadata: core.NewMetadataCache()}
}

func (e *Engine) Key() string              { return "furaffinity" }
func (e *Engine) UsesCursor() bool         { return false } // numeric pages
func (e *Engine) TagPhrase() bool          { return true }  // FA search is full-text
func (e *Engine) Filters() core.FilterList { return Filters() }

// headers builds browser UA + the pasted Cookie when present.
func (e *Engine) headers(c *core.ApiClient, siteKey string) map[string]string {
	h := map[string]string{
		"User-Agent":      core.BrowserUA,
		"Accept-Language": "en-US,en;q=0.9",
		"Accept":          "text/html,application/xhtml+xml",
	}
	if cookie := c.Cred(siteKey, "cookie"); cookie != "" {
		h["Cookie"] = cookie
	}
	return h
}

// parseQuery translates the booru-style query into search ingredients.
type faQuery struct {
	authors   []string
	words     []string
	rating    string
	typeName  string
	rng       string
	order     string
	mode      string
	threshold int64 // -1 = unset
	hasThresh bool
}

func parseQuery(tags string) faQuery {
	var q faQuery
	q.threshold = -1
	for _, tok := range strings.Fields(tags) {
		low := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(low, "by:"):
			name := strings.TrimSpace(tok[3:])
			name = strings.TrimLeft(name, "@")
			if name != "" {
				q.authors = append(q.authors, name)
			}
		case strings.HasPrefix(low, "rating:"):
			v := low[len("rating:"):]
			if _, ok := ratingsFor[v]; ok {
				q.rating = v
			}
		case strings.HasPrefix(low, "type:"):
			v := low[len("type:"):]
			for _, t := range types {
				if t == v {
					q.typeName = v
					break
				}
			}
		case strings.HasPrefix(low, "range:"):
			v := low[len("range:"):]
			if rangesSet[v] {
				q.rng = v
			}
		case strings.HasPrefix(low, "order:"):
			v := low[len("order:"):]
			if _, ok := orders[v]; ok {
				q.order = v
			}
		case strings.HasPrefix(low, "mode:"):
			v := low[len("mode:"):]
			if v == "all" || v == "any" || v == "extended" {
				q.mode = v
			}
		case strings.HasPrefix(low, "favcount:"):
			if m := favcountRe.FindStringSubmatch(low); m != nil {
				n, _ := strconv.ParseInt(m[2], 10, 64)
				q.threshold = n
				q.hasThresh = true
			}
		default:
			q.words = append(q.words, tok)
		}
	}
	return q
}

func (q *faQuery) searchParams(page int) url.Values {
	ord := orders[q.order]
	params := url.Values{}
	params.Set("q", strings.Join(q.words, " "))
	params.Set("page", strconv.Itoa(page))
	params.Set("perpage", "72")
	params.Set("order-by", ord[0])
	params.Set("order-direction", ord[1])
	if q.rng != "" {
		params.Set("range", q.rng)
	} else {
		params.Set("range", "all")
	}
	if q.mode != "" {
		params.Set("mode", q.mode)
	} else {
		params.Set("mode", "extended")
	}
	for _, r := range ratingsFor[q.rating] {
		params.Set("rating-"+r, "1")
	}
	typeList := types
	if q.typeName != "" {
		typeList = []string{q.typeName}
	}
	for _, t := range typeList {
		params.Set("type-"+t, "1")
	}
	return params
}

// ValidateQuery: FA needs at least one word or an author.
func (e *Engine) ValidateQuery(tags string) error {
	q := parseQuery(tags)
	if len(q.authors) == 0 && len(q.words) == 0 {
		return &core.ApiError{
			Msg: "FurAffinity: укажите ключевые слова или автора (by:)"}
	}
	return nil
}

// FetchPosts scrapes one search/gallery page.
func (e *Engine) FetchPosts(c *core.ApiClient, s *core.Site, tags string, limit int, cursor any) ([]*core.PostInfo, error) {
	r, err := e.FetchPage(c, s, tags, limit, cursor)
	if limit > 0 && len(r.Posts) > limit {
		r.Posts = r.Posts[:limit]
	}
	return r.Posts, err
}

func (e *Engine) FetchPage(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) (core.PageResult, error) {

	if err := e.ValidateQuery(tags); err != nil {
		return core.PageResult{}, err
	}
	q := parseQuery(tags)
	// id:<n> words ride through as the reverse-search seam
	var ids []int64
	var realWords []string
	for _, w := range q.words {
		if strings.HasPrefix(strings.ToLower(w), "id:") {
			if n, err := strconv.ParseInt(w[3:], 10, 64); err == nil && n > 0 {
				ids = append(ids, n)
				continue
			}
		}
		realWords = append(realWords, w)
	}
	q.words = realWords
	if len(ids) > 0 {
		var posts []*core.PostInfo
		for _, id := range ids {
			p, err := e.PostByID(c, site, core.I(id))
			if err != nil {
				return core.PageResult{}, err
			}
			if p != nil {
				posts = append(posts, p)
			}
		}
		return core.PageResult{Posts: posts}, nil
	}
	pageNo := int64(1)
	switch t := page.(type) {
	case int:
		pageNo = int64(t)
	case int64:
		pageNo = t
	case string:
		if n, err := strconv.ParseInt(t, 10, 64); err == nil && n > 0 {
			pageNo = n
		}
	}
	var htmlText string
	switch {
	case len(q.authors) > 0:
		text, err := c.GetText(nil,
			fmt.Sprintf("%s/gallery/%s/%d/", site.Base(),
				url.QueryEscape(q.authors[0]), pageNo),
			nil, e.headers(c, site.Key))
		if err != nil {
			return core.PageResult{}, err
		}
		htmlText = text
	default:
		text, err := c.GetText(nil, site.Base()+"/search/",
			q.searchParams(int(pageNo)), e.headers(c, site.Key))
		if err != nil {
			return core.PageResult{}, err
		}
		htmlText = text
	}
	var posts []*core.PostInfo
	for _, block := range figRe.FindAllString(htmlText, -1) {
		if p := e.fromFigure(site, block); p != nil {
			posts = append(posts, p)
		}
	}
	rawCount := len(posts)
	if len(q.authors) > 0 && q.rating != "" {
		// a gallery page has no rating checkboxes — the figure classes do
		allowed := map[string]bool{}
		for _, r := range ratingsFor[q.rating] {
			allowed[ratingWords[r]] = true
		}
		filtered := posts[:0]
		for _, p := range posts {
			if allowed[p.Rating] {
				filtered = append(filtered, p)
			}
		}
		posts = filtered
	}
	if q.hasThresh {
		var err error
		posts, err = e.filterByFaves(c, site, posts, q.threshold)
		if err != nil {
			return core.PageResult{}, err
		}
	}
	result := core.PageResult{Posts: posts, HasMore: rawCount > 0}
	if result.HasMore {
		result.Next = pageNo + 1
	}
	return result, nil
}

// fromFigure converts one <figure …> card into a PostInfo stub.
func (e *Engine) fromFigure(site *core.Site, block string) *core.PostInfo {
	sid := sidRe.FindStringSubmatch(block)
	if sid == nil {
		return nil
	}
	idNum, _ := strconv.ParseInt(sid[1], 10, 64)
	classes := map[string]bool{}
	if cm := classRe.FindStringSubmatch(block); cm != nil {
		for _, cls := range strings.Fields(cm[1]) {
			classes[cls] = true
		}
	}
	thumb := ""
	if tm := thumbRe.FindStringSubmatch(block); tm != nil {
		thumb = abs(tm[1])
	}
	// the grid decodes @300; the viewer starts from the @600 render until
	// prepare_view swaps in the real file
	sample := ""
	if thumb != "" {
		sample = atSize.ReplaceAllString(thumb, "@600-")
	}
	author := ""
	if um := userRe.FindStringSubmatch(block); um != nil {
		author = um[1]
	}
	rating := "?"
	for c := range classes {
		if r, ok := ratingByClass[c]; ok {
			rating = r
			break
		}
	}
	ext := "jpg"
	for c := range classes {
		if x, ok := extByClass[c]; ok {
			ext = x
			break
		}
	}
	var artists []string
	if author != "" {
		artists = []string{author}
	}
	return &core.PostInfo{
		ID:         core.I(idNum),
		Site:       site.Key,
		File:       &core.PostFile{Ext: ext},
		PreviewURL: thumb,
		SampleURL:  orDefault(sample, thumb),
		Rating:     rating,
		Artists:    artists,
		AuthorID:   author,
		Extra:      map[string]any{},
	}
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

func (e *Engine) filterByFaves(c *core.ApiClient, site *core.Site,
	posts []*core.PostInfo, threshold int64) ([]*core.PostInfo, error) {

	var kept []*core.PostInfo
	for _, p := range posts {
		if err := e.fillDetail(c, site, p); err != nil {
			return nil, err // failed metadata must never turn into a nonmatching post
		}
		if p.FavCount >= threshold {
			kept = append(kept, p)
		}
	}
	return kept, nil
}

// detail returns the parsed /view/<id>/ page, memoised (LRU).
func (e *Engine) detail(c *core.ApiClient, site *core.Site,
	postID int64) (viewInfo, error) {

	cache := c.Metadata
	if cache == nil {
		e.mu.Lock()
		if e.metadata == nil {
			e.metadata = core.NewMetadataCache()
		}
		cache = e.metadata
		e.mu.Unlock()
	}
	load := func(ctx context.Context) ([]byte, error) {
		shared := c.Clone(ctx)
		shared.SetStopCheck(nil)
		text, err := shared.GetText(ctx, fmt.Sprintf("%s/view/%d/", site.Base(), postID), nil, e.headers(shared, site.Key))
		if err != nil {
			return nil, err
		}
		d := ParseViewPage(text)
		if d.DL == "" && d.Full == "" {
			return nil, fmt.Errorf("FurAffinity: страница /view/%d/ без файла — работа удалена, изменилась разметка или нужен Cookie", postID)
		}
		return json.Marshal(d)
	}
	var data []byte
	var err error
	if c.FreshMetadata {
		data, err = load(c.Context())
	} else {
		data, err = cache.Fetch(c.Context(), fmt.Sprintf("fa:%s:%s:%d", site.Key, c.AuthIdentity(), postID), 5*time.Minute, load)
	}
	if err != nil {
		return viewInfo{}, err
	}
	var d viewInfo
	err = json.Unmarshal(data, &d)
	return d, err
}

// fillDetail resolves post in place from its view page: the real file URL and
// extension, the fullview render for the viewer, upload time, favourites,
// rating and tags.
func (e *Engine) fillDetail(c *core.ApiClient, site *core.Site,
	post *core.PostInfo) error {

	d, err := e.detail(c, site, post.ID.Int())
	if err != nil {
		return err
	}
	u := d.DL
	if u == "" {
		u = d.Full
	}
	if u != "" && post.File != nil {
		post.File.URL = u
		ext := ""
		if i := strings.LastIndex(u, "."); i >= 0 {
			ext = strings.ToLower(u[i+1:])
		}
		if len(ext) >= 1 && len(ext) <= 5 {
			if ext == "jpeg" {
				ext = "jpg"
			}
			post.File.Ext = ext
		}
	}
	if d.Full != "" {
		post.SampleURL = d.Full
	}
	if d.Created != 0 {
		post.CreatedAt = d.Created
	}
	post.FavCount = d.Faves
	post.Score = d.Faves
	if d.Rating != "" {
		post.Rating = d.Rating
	}
	if len(d.Tags) > 0 {
		post.Tags = d.Tags
	}
	if d.Author != "" && len(post.Artists) == 0 {
		post.Artists = []string{d.Author}
		post.AuthorID = d.Author
	}
	if post.Extra == nil {
		post.Extra = map[string]any{}
	}
	post.Extra["resolved"] = true
	return nil
}

// ExpandPost resolves the empty search-page stub to the real file URL at
// download time. A failed page keeps the stub; the job fails visibly.
func (e *Engine) ExpandPost(c *core.ApiClient,
	post *core.PostInfo) []*core.PostInfo {

	if c == nil || post.File == nil {
		return []*core.PostInfo{post}
	}
	if resolved, _ := post.Extra["resolved"].(bool); resolved {
		return []*core.PostInfo{post}
	}
	site := core.GetSite(post.Site)
	_ = e.fillDetail(c, site, post) // errors keep the stub
	return []*core.PostInfo{post}
}

// PrepareView resolves the post in place for the large viewer; true when it
// changed (so the viewer reloads).
func (e *Engine) PrepareView(c *core.ApiClient, post *core.PostInfo) (bool, error) {
	if c == nil || post.File == nil {
		return false, nil
	}
	if resolved, _ := post.Extra["resolved"].(bool); resolved {
		return false, nil
	}
	if err := e.fillDetail(c, core.GetSite(post.Site), post); err != nil {
		return false, nil // the @600 thumb stays on screen
	}
	return true, nil
}

// DownloadURLs prefers the download link; for images the fullview render IS
// the file; for music/stories it is a rendered preview — fallback only.
func (e *Engine) DownloadURLs(post *core.PostInfo) []string {
	var urls []string
	if post.File != nil && post.File.URL != "" {
		urls = append(urls, post.File.URL)
	}
	s := post.SampleURL
	if strings.Contains(s, "//d.furaffinity.net/") {
		dup := false
		for _, u := range urls {
			if u == s {
				dup = true
				break
			}
		}
		if !dup {
			urls = append(urls, s)
		}
	}
	return urls
}

// VerifiesHash: FA exposes no content hash.
func (e *Engine) VerifiesHash(*core.PostInfo) bool { return false }

// ArtistQuery builds by:<name>.
func (e *Engine) ArtistQuery(artist string, _ *core.PostInfo) string {
	a := strings.TrimLeft(strings.TrimLeft(strings.TrimSpace(artist), "@"), "~")
	if a == "" {
		return ""
	}
	return "by:" + a
}

var viewURLRe = regexp.MustCompile(`/(?:view|full)/(\d+)`)

// IdFromURL parses /view/<id> and /full/<id> permalinks.
func (e *Engine) IdFromURL(_ *core.Site, urlStr string) core.PostId {
	if m := viewURLRe.FindStringSubmatch(urlStr); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return core.I(n)
	}
	return core.I(0)
}

// PostByID fetches one work straight from its view page (reverse search /
// id: query). The list thumbnail is reconstructed from the file URL's upload
// epoch — FA thumbnails are t.…/<id>@<size>-<epoch>.jpg.
func (e *Engine) PostByID(c *core.ApiClient, site *core.Site,
	id core.PostId) (*core.PostInfo, error) {

	if !id.IsNu || id.Num <= 0 {
		return nil, nil
	}
	d, err := e.detail(c, site, id.Num)
	if err != nil {
		return nil, nil
	}
	u := d.DL
	if u == "" {
		u = d.Full
	}
	ext := "jpg"
	if i := strings.LastIndex(u, "."); i >= 0 {
		ext = strings.ToLower(u[i+1:])
	}
	if ext == "jpeg" {
		ext = "jpg"
	}
	thumb := d.Full
	if thumb == "" {
		thumb = u
	}
	if d.Created != 0 {
		thumb = fmt.Sprintf("https://t.furaffinity.net/%d@300-%d.jpg",
			id.Num, d.Created)
	}
	author := d.Author
	var artists []string
	if author != "" {
		artists = []string{author}
	}
	return &core.PostInfo{
		ID:         id,
		Site:       site.Key,
		File:       &core.PostFile{URL: u, Ext: ext},
		PreviewURL: thumb,
		SampleURL:  orDefault(d.Full, u),
		Score:      d.Faves,
		FavCount:   d.Faves,
		Rating:     orDefaultS(d.Rating, "?"),
		Artists:    artists,
		AuthorID:   author,
		CreatedAt:  d.Created,
		Tags:       append([]string(nil), d.Tags...),
		Extra:      map[string]any{"resolved": true},
	}, nil
}

func orDefaultS(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

// DeriveArtists: the author was parsed from the figure card.
func (e *Engine) DeriveArtists(post *core.PostInfo, _ []string) []string {
	return append([]string(nil), post.Artists...)
}

// Register installs the engine and the FurAffinity site.
func Register() {
	core.RegisterEngine(NewEngine())
	core.RegisterSite(&core.Site{
		Key:            "furaffinity",
		Label:          "FurAffinity",
		Domain:         "www.furaffinity.net",
		Engine:         "furaffinity",
		PageLimit:      72,   // the search form's largest perpage
		MinInterval:    1.0,  // HTML pages, полноценный скрейпинг — вежливо
		OptionalAuth:   true, // Cookie открывает Mature/Adult
		CfProtected:    true, // весь сайт за Cloudflare
		CfExtraDomains: []string{"t.furaffinity.net", "d.furaffinity.net"},
		AuthFie: []core.CredField{
			{Key: "cookie", Label: "Cookie (строка из DevTools)"},
		},
		AuthURL: "https://www.furaffinity.net/  (войдите → DevTools → Network → " +
			"любой запрос к furaffinity.net → Request Headers → скопируйте " +
			"значение заголовка Cookie целиком; нужны куки «a» и «b»)",
		PostURLTm: "https://www.furaffinity.net/view/{id}/",
	})
}
