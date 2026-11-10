// Package pixiv — not a booru. Uses the site's web AJAX API with the user's
// login cookie (PHPSESSID), which unlocks everything including R-18.
//
//   - Auth is a session cookie, not query params.
//   - Search (/ajax/search/artworks/{word}) returns 60 illusts/page with tags,
//     xRestrict (rating) and userName (pixiv users ARE the authors), but no
//     bookmark count — the score filter is applied client-side by pulling each
//     illust's bookmarkCount from /ajax/illust/{id} (only when a threshold is
//     set, to stay fast otherwise).
//   - The original image URL derives from the thumbnail's date path; the
//     extension (png/jpg) is unknown up front, so DownloadURLs offers both.
//   - Image bytes need a pixiv Referer (the core adds it for pximg hosts).
//   - Multi-page artworks are expanded and every original page downloaded.
//
// The query string is booru-flavoured; this engine parses it into word + mode
// + a client-side bookmark threshold. id:<n> fetches specific artworks;
// user_id:<n> lists an author's gallery (one token backs «работы автора»,
// several back the «Авторы» union).
package pixiv

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"massdownloader/internal/core"
)

var dateRe = regexp.MustCompile(`/img/(\d{4}/\d{2}/\d{2}/\d{2}/\d{2}/\d{2})/`)
var scoreRe = regexp.MustCompile(`score:(>=|<=|>|<|=)?(\d+)`)
var digitsRe = regexp.MustCompile(`\d+`)
var artworksRe = regexp.MustCompile(`/artworks/(\d+)`)

const (
	userWorkBatch = 48 // works per page when browsing an author («работы автора»)
)

// Filters mirrors app/sites/other/pixiv_filters.py: no faves; "score" is the
// bookmark count (filtered client-side); authors are users resolved by id.
func Filters() core.FilterList {
	return core.FilterList{
		// PixivAuthorsFilter: values are numeric user ids; each becomes a
		// user_id:<n> token. Several authors -> the union of their works.
		{Kind: core.KindAuthors, Key: "authors",
			Label:        "Авторы (поиск по пользователям)",
			Placeholder:  "имя автора pixiv… (или ID/ссылка)",
			Autocomplete: "artists",
			Tooltip: "Поиск работ по авторам pixiv.\n" +
				"Начните вводить имя и выберите автора из подсказок,\n" +
				"либо вставьте числовой ID или ссылку на профиль.\n" +
				"Несколько авторов — показываются работы всех (объединение).\n" +
				"Если задано — теги игнорируются."},
		{Kind: core.KindTags, Key: "include", Label: "Теги — обязательные",
			Placeholder: "тег (обязательно)…", Autocomplete: "tags"},
		core.NewExclude(),
		core.NewText("ids", "Поиск по ID (pixiv)",
			"145964909, ссылка… (через запятую)", "id"),
		core.NewThreshold("score", "Закладки (bookmarks)", "score", 500),
		{Kind: core.KindChoice, Key: "rating", Label: "Рейтинг", Token: "rating",
			Options: []core.ChoiceOption{
				{Label: "Любой", Value: ""}, {Label: "Все возрасты", Value: "s"},
				{Label: "R-18", Value: "e"},
			}},
	}
}

// Engine ports PixivEngine.
type Engine struct {
	core.EngineBase
	mu        sync.Mutex
	snapshots map[string]userSnapshot
	metadata  *core.MetadataCache
}

func NewEngine() *Engine {
	return &Engine{metadata: core.NewMetadataCache()}
}

func (e *Engine) Key() string              { return "pixiv" }
func (e *Engine) SupportsAlbums() bool     { return true }
func (e *Engine) UsesCursor() bool         { return false }
func (e *Engine) Filters() core.FilterList { return Filters() }

// illustDetail returns the /ajax/illust/{id} body dict, memoised (LRU);
// {} when the response carried no body.
func (e *Engine) illustDetail(c *core.ApiClient, site *core.Site,
	iid int64) (map[string]any, error) {

	if c.Metadata == nil {
		c = c.Clone(c.Context())
		c.Metadata = e.metadata
	}
	data, err := c.GetJSON(nil,
		fmt.Sprintf("%s/ajax/illust/%d", site.Base(), iid), nil,
		e.headers(c, site))
	if err != nil {
		return nil, err
	}
	m, ok := data.(map[string]any)
	if !ok || m["error"] == true {
		return nil, fmt.Errorf("Pixiv: ошибка получения деталей работы")
	}
	body, ok := m["body"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Pixiv: отсутствуют детали работы")
	}
	return body, nil
}

// headers build Referer always; Cookie when logged in. Accepts a bare
// PHPSESSID value or a full "name=value; ..." string.
func (e *Engine) headers(c *core.ApiClient, site *core.Site) map[string]string {
	h := map[string]string{
		"Referer":    "https://www.pixiv.net/",
		"User-Agent": core.BrowserUA,
		"Accept":     "application/json",
	}
	if cookie := c.Cred(site.Key, "cookie"); cookie != "" {
		if strings.Contains(cookie, "=") {
			h["Cookie"] = cookie
		} else {
			h["Cookie"] = "PHPSESSID=" + cookie
		}
	}
	return h
}

type pixivQuery struct {
	word      string
	mode      string
	op        string
	threshold int64
	hasThresh bool
	ids       []int64
	userIDs   []int64
}

// parseQuery translates the booru-style query into pixiv params.
func parseQuery(tags string) pixivQuery {
	var q pixivQuery
	q.mode = "all"
	q.op = "="
	var words []string
	for _, tok := range strings.Fields(tags) {
		switch {
		case strings.HasPrefix(tok, "rating:"):
			r := tok[len("rating:"):]
			switch r {
			case "s":
				q.mode = "safe"
			case "q", "e":
				q.mode = "r18"
			default:
				q.mode = "all"
			}
		case strings.HasPrefix(tok, "score:"):
			if m := scoreRe.FindStringSubmatch(tok); m != nil {
				if m[1] != "" {
					q.op = m[1]
				} else {
					q.op = "="
				}
				n, _ := strconv.ParseInt(m[2], 10, 64)
				q.threshold = n
				q.hasThresh = true
			}
		case strings.HasPrefix(tok, "user_id:"):
			// accept a bare id or a /users/<id> URL (longest digit run)
			if n := longestNum(tok[len("user_id:"):]); n != 0 {
				q.userIDs = append(q.userIDs, n)
			}
		case strings.HasPrefix(tok, "id:"):
			// accept a bare id or a full /artworks/<id> URL
			if n := longestNum(tok[len("id:"):]); n != 0 {
				q.ids = append(q.ids, n)
			}
		case strings.HasPrefix(tok, "favcount:"), strings.HasPrefix(tok, "order:"):
			// not applicable on pixiv
		default:
			words = append(words, strings.TrimLeft(tok, "~"))
		}
	}
	q.word = strings.TrimSpace(strings.Join(words, " "))
	return q
}

// longestNum returns the longest digit run in s (0 when none).
func longestNum(s string) int64 {
	best := ""
	for _, m := range digitsRe.FindAllString(s, -1) {
		if len(m) > len(best) {
			best = m
		}
	}
	n, _ := strconv.ParseInt(best, 10, 64)
	return n
}

func passes(value int64, op string, threshold int64) bool {
	switch op {
	case ">=":
		return value >= threshold
	case ">":
		return value > threshold
	case "<=":
		return value <= threshold
	case "<":
		return value < threshold
	case "=":
		return value == threshold
	}
	return true
}

func ratingOf(xRestrict any) string {
	switch core.AsInt(xRestrict) {
	case 1, 2:
		return "e"
	case 0:
		return "s"
	}
	return "?"
}

// FetchPosts runs one search page, an id lookup, or a merged author browse.
func (e *Engine) FetchPosts(c *core.ApiClient, s *core.Site, tags string, limit int, cursor any) ([]*core.PostInfo, error) {
	r, err := e.FetchPage(c, s, tags, limit, cursor)
	return r.Posts, err
}

func (e *Engine) fetchSearchPage(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) (core.PageResult, error) {

	q := parseQuery(tags)
	pageNo := int64(1)
	switch t := page.(type) {
	case int:
		pageNo = int64(t)
	case int64:
		pageNo = t
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil &&
			n > 0 {
			pageNo = n
		}
	}
	if len(q.ids) > 0 {
		posts, err := e.fetchByIDs(c, site, q.ids, q.op, q.threshold, pageNo)
		return core.PageResult{Posts: posts}, err
	}
	if q.word == "" {
		return core.PageResult{}, &core.ApiError{Msg: "Pixiv: укажите тег или ID для поиска"}
	}
	params := url.Values{}
	params.Set("word", q.word)
	params.Set("order", "date_d")
	params.Set("mode", q.mode)
	params.Set("p", strconv.FormatInt(pageNo, 10))
	params.Set("s_mode", "s_tag")
	params.Set("type", "all")
	data, err := c.GetJSON(nil,
		fmt.Sprintf("%s/ajax/search/artworks/%s", site.Base(),
			url.QueryEscape(q.word)),
		params, e.headers(c, site))
	if err != nil {
		return core.PageResult{}, err
	}
	m, ok := data.(map[string]any)
	if !ok {
		return core.PageResult{}, fmt.Errorf("Pixiv: некорректный ответ поиска")
	}
	if errFlag, _ := m["error"].(bool); errFlag {
		msg := core.Str_(m["message"])
		if msg == "" {
			msg = "Pixiv: ошибка запроса"
		}
		return core.PageResult{}, &core.ApiError{Msg: msg}
	}
	body, _ := m["body"].(map[string]any)
	im, _ := body["illustManga"].(map[string]any)
	items, ok := im["data"].([]any)
	if !ok {
		return core.PageResult{}, fmt.Errorf("Pixiv: отсутствует список результатов")
	}
	out := make([]*core.PostInfo, 0, len(items))
	for _, item := range items {
		it, ok := item.(map[string]any)
		if !ok {
			continue
		}
		p, err := e.build(c, site, it, q.op, q.threshold, q.hasThresh)
		if err != nil {
			return core.PageResult{}, err
		}
		if p != nil {
			out = append(out, p)
		}
	}
	result := core.PageResult{Posts: out, HasMore: len(items) > 0}
	if last := core.AsInt(im["lastPage"]); last > 0 && pageNo >= last {
		result.HasMore = false
	}
	if result.HasMore {
		result.Next = pageNo + 1
	}
	return result, nil
}

// build converts one search-result entry into PostInfo.
func (e *Engine) build(c *core.ApiClient, site *core.Site, it map[string]any,
	op string, threshold int64, hasThresh bool) (*core.PostInfo, error) {

	if core.AsInt(it["illustType"]) == 2 { // ugoira (animation) — skip
		return nil, nil
	}
	thumb := core.Str_(it["url"])
	m := dateRe.FindStringSubmatch(thumb)
	if m == nil {
		return nil, nil
	}
	iid := core.AsInt(it["id"])
	datepath := m[1]
	score := int64(0)
	var original string
	if hasThresh {
		// need the bookmark count -> pull the illust detail (also gives the
		// exact original URL, so no extension guessing here)
		body, err := e.illustDetail(c, site, iid)
		if err != nil {
			return nil, err
		}
		score = core.AsInt(body["bookmarkCount"])
		if !passes(score, op, threshold) {
			return nil, nil
		}
		if urls, ok := body["urls"].(map[string]any); ok {
			original = core.Str_(urls["original"])
		}
		if post := e.postFromDetail(site, body, op, threshold, hasThresh); post != nil {
			return post, nil
		}
	}
	if original == "" {
		original = fmt.Sprintf(
			"https://i.pximg.net/img-original/img/%s/%d_p0.png", datepath, iid)
	}
	ext := ""
	if i := strings.LastIndex(original, "."); i >= 0 {
		ext = strings.ToLower(original[i+1:])
	}
	w := int(core.AsInt(it["width"]))
	h := int(core.AsInt(it["height"]))
	file := &core.PostFile{URL: original, Ext: ext, Width: w, Height: h}
	master := masterURL(datepath, iid, 0)
	var artists []string
	if un := core.Str_(it["userName"]); un != "" {
		artists = []string{un}
	}
	tags := strSliceOf(it["tags"])
	authorID := ""
	if uid := core.AsInt(it["userId"]); uid != 0 {
		authorID = strconv.FormatInt(uid, 10)
	}
	pageCount := int(maxI64(1, core.AsInt(it["pageCount"])))
	return &core.PostInfo{
		ID:         core.I(iid),
		Site:       site.Key,
		File:       file,
		PreviewURL: thumb,
		SampleURL:  master,
		Score:      score,
		Rating:     ratingOf(it["xRestrict"]),
		Artists:    artists,
		AuthorID:   authorID,
		Tags:       tags,
		PageCount:  pageCount,
		CreatedAt:  core.ParseTime(it["createDate"]),
		Extra:      map[string]any{"title": core.Str_(it["title"])},
	}, nil
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func strSliceOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func masterURL(datepath string, iid int64, page int64) string {
	return fmt.Sprintf("https://i.pximg.net/img-master/img/%s/%d_p%d_master1200.jpg",
		datepath, iid, page)
}

func originalURL(datepath string, iid int64, page int64) string {
	return fmt.Sprintf("https://i.pximg.net/img-original/img/%s/%d_p%d.png",
		datepath, iid, page)
}

// -- browse author galleries --------------------------------------------------

// userWorkIDs parses every illustration+manga id from a profile/all body,
// newest first (pure so it can be tested offline).
func userWorkIDs(profileBody map[string]any) []int64 {
	set := map[int64]bool{}
	for _, cat := range []string{"illusts", "manga"} {
		if d, ok := profileBody[cat].(map[string]any); ok {
			for k := range d {
				if n := core.AsInt(k); n != 0 {
					set[n] = true
				}
			}
		}
	}
	out := make([]int64, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

// modeOK applies the rating filter to a work entry.
func modeOK(mode string, xRestrict any) bool {
	switch mode {
	case "safe":
		return core.AsInt(xRestrict) == 0
	case "r18":
		return core.AsInt(xRestrict) > 0
	}
	return true
}

// -- fetch specific artworks by id --------------------------------------------

func (e *Engine) fetchByIDs(c *core.ApiClient, site *core.Site, ids []int64,
	op string, threshold int64, page int64) ([]*core.PostInfo, error) {

	hasThresh := threshold != 0 || op != "="
	if page > 1 {
		return nil, nil // every requested id is returned on the first page
	}
	var out []*core.PostInfo
	for _, iid := range ids {
		body, err := e.illustDetail(c, site, iid)
		if err != nil {
			return nil, err
		}
		if p := e.postFromDetail(site, body, op, threshold, hasThresh); p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// postFromDetail builds a PostInfo from a detail body (exact urls, page count
// and bookmark count).
func (e *Engine) postFromDetail(site *core.Site, body map[string]any,
	op string, threshold int64, hasThresh bool) *core.PostInfo {

	if core.AsInt(body["illustType"]) == 2 { // ugoira — can't download
		return nil
	}
	urls, _ := body["urls"].(map[string]any)
	original := ""
	if urls != nil {
		original = core.Str_(urls["original"])
	}
	m := dateRe.FindStringSubmatch(original)
	if original == "" || m == nil {
		return nil
	}
	score := core.AsInt(body["bookmarkCount"])
	if hasThresh && !passes(score, op, threshold) {
		return nil
	}
	iid := core.AsInt(body["id"])
	ext := ""
	if i := strings.LastIndex(original, "."); i >= 0 {
		ext = strings.ToLower(original[i+1:])
	}
	file := &core.PostFile{URL: original, Ext: ext,
		Width:  int(core.AsInt(body["width"])),
		Height: int(core.AsInt(body["height"]))}
	master := ""
	if urls != nil {
		master = core.Str_(urls["regular"])
	}
	if master == "" {
		master = masterURL(m[1], iid, 0)
	}
	preview := master
	if urls != nil {
		if s := core.Str_(urls["small"]); s != "" {
			preview = s
		}
	}
	var artists []string
	if un := core.Str_(body["userName"]); un != "" {
		artists = []string{un}
	}
	authorID := ""
	if uid := core.AsInt(body["userId"]); uid != 0 {
		authorID = strconv.FormatInt(uid, 10)
	}
	return &core.PostInfo{
		ID:         core.I(iid),
		Site:       site.Key,
		File:       file,
		PreviewURL: preview,
		SampleURL:  master,
		Score:      score,
		Rating:     ratingOf(body["xRestrict"]),
		Artists:    artists,
		AuthorID:   authorID,
		Tags:       detailTags(body),
		PageCount:  int(maxI64(1, core.AsInt(body["pageCount"]))),
		CreatedAt:  core.ParseTime(body["createDate"]),
		Extra:      pixivContent(body),
	}
}

func pixivContent(body map[string]any) map[string]any {
	title := core.Str_(body["title"])
	if title == "" {
		title = core.Str_(body["illustTitle"])
	}
	content := core.Str_(body["description"])
	if content == "" {
		content = core.Str_(body["illustComment"])
	}
	return map[string]any{"title": title, "content": content}
}

// Search and author listings omit the caption; resolve it only when opened.
func (e *Engine) PrepareView(c *core.ApiClient, post *core.PostInfo) (bool, error) {
	if c == nil || post == nil {
		return false, nil
	}
	site := core.GetSite(post.Site)
	if site == nil {
		return false, fmt.Errorf("Pixiv: неизвестный сайт")
	}
	body, err := e.illustDetail(c, site, post.ID.Int())
	if err != nil {
		return false, err
	}
	detail := e.postFromDetail(site, body, "=", 0, false)
	if detail == nil {
		return false, fmt.Errorf("Pixiv: неполные детали работы")
	}
	if post.Extra == nil {
		post.Extra = map[string]any{}
	}
	for key, value := range detail.Extra {
		post.Extra[key] = value
	}
	post.Score, post.Rating, post.CreatedAt = detail.Score, detail.Rating, detail.CreatedAt
	post.Artists, post.AuthorID, post.Tags = detail.Artists, detail.AuthorID, detail.Tags
	// An expanded attachment must keep its own URLs and page index.
	if post.Extra["expanded_page"] != true && post.Extra["standalone_attachment"] != true {
		post.File, post.PreviewURL, post.SampleURL = detail.File, detail.PreviewURL, detail.SampleURL
		post.PageCount = detail.PageCount
	}
	return true, nil
}

func detailTags(body map[string]any) []string {
	tagsObj, _ := body["tags"].(map[string]any)
	raw, _ := tagsObj["tags"].([]any)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		var name string
		switch tv := t.(type) {
		case map[string]any:
			name = core.Str_(tv["tag"])
		case string:
			name = tv
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// ExpandPost yields one PostInfo per album image; with a client the exact
// per-page URLs come from /ajax/illust/{id}/pages, else they derive from
// page 0's date path with the .png/.jpg guess resolved at download time.
func (e *Engine) ExpandPost(c *core.ApiClient, post *core.PostInfo) []*core.PostInfo {
	if post == nil {
		return nil
	}
	if post.Extra["expanded_page"] == true || post.Extra["standalone_attachment"] == true {
		return []*core.PostInfo{post}
	}
	n := maxI64(1, int64(post.PageCount))
	if n <= 1 || post.File == nil {
		return []*core.PostInfo{post}
	}
	if c != nil {
		if pages := e.albumFromAPI(c, post); len(pages) > 0 {
			return pages
		}
	}
	return e.albumDerived(post)
}

func (e *Engine) LocalPages(p *core.PostInfo) []*core.PostInfo { return e.ExpandPost(nil, p) }

func (e *Engine) albumFromAPI(c *core.ApiClient, post *core.PostInfo) []*core.PostInfo {
	if c.Metadata == nil {
		c = c.Clone(c.Context())
		c.Metadata = e.metadata
	}
	site := core.GetSite(post.Site)
	if site == nil {
		return nil
	}
	d, err := c.GetJSON(nil,
		fmt.Sprintf("%s/ajax/illust/%s/pages", site.Base(), post.ID.String()),
		nil, e.headers(c, site))
	if err != nil {
		return nil
	}
	m, ok := d.(map[string]any)
	if !ok {
		return nil
	}
	if errFlag, _ := m["error"].(bool); errFlag {
		return nil
	}
	list, _ := m["body"].([]any)
	if len(list) == 0 {
		return nil
	}
	pages := make([]*core.PostInfo, 0, len(list))
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		urls, _ := entry["urls"].(map[string]any)
		original := ""
		if urls != nil {
			original = core.Str_(urls["original"])
		}
		if original == "" {
			return nil
		}
		master := original
		if urls != nil {
			if r := core.Str_(urls["regular"]); r != "" {
				master = r
			} else if s := core.Str_(urls["small"]); s != "" {
				master = s
			}
		}
		ext := ""
		if j := strings.LastIndex(original, "."); j >= 0 {
			ext = strings.ToLower(original[j+1:])
		}
		f := &core.PostFile{URL: original, Ext: ext,
			Width:  int(core.AsInt(entry["width"])),
			Height: int(core.AsInt(entry["height"]))}
		page := post.Clone()
		page.File, page.PreviewURL, page.SampleURL = f, master, master
		page.PageCount, page.PageIndex = len(list), i
		if page.Extra == nil {
			page.Extra = map[string]any{}
		}
		page.Extra["expanded_page"] = true
		pages = append(pages, page)
	}
	return pages
}

func (e *Engine) albumDerived(post *core.PostInfo) []*core.PostInfo {
	m := dateRe.FindStringSubmatch(post.File.URL)
	if m == nil {
		return []*core.PostInfo{post}
	}
	datepath := m[1]
	iid := post.ID.Int()
	n := maxI64(1, int64(post.PageCount))
	pages := make([]*core.PostInfo, 0, n)
	for i := int64(0); i < n; i++ {
		original := originalURL(datepath, iid, i)
		w, h := 0, 0
		if i == 0 {
			w, h = post.File.Width, post.File.Height
		}
		f := &core.PostFile{URL: original, Ext: "png", Width: w, Height: h}
		master := masterURL(datepath, iid, i)
		page := post.Clone()
		page.File, page.PreviewURL, page.SampleURL = f, master, master
		page.PageCount, page.PageIndex = int(n), int(i)
		if page.Extra == nil {
			page.Extra = map[string]any{}
		}
		page.Extra["expanded_page"] = true
		pages = append(pages, page)
	}
	return pages
}

// ArtistQuery browses the numeric user id via the user_id: metatag; without
// one the feature stays off.
func (e *Engine) ArtistQuery(artist string, post *core.PostInfo) string {
	if post != nil && post.AuthorID != "" {
		return "user_id:" + post.AuthorID
	}
	return ""
}

// IdFromURL parses /artworks/<n> permalinks (optionally /en/artworks/<n>).
func (e *Engine) IdFromURL(_ *core.Site, urlStr string) core.PostId {
	if m := artworksRe.FindStringSubmatch(urlStr); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return core.I(n)
	}
	if u, err := url.Parse(urlStr); err == nil {
		if n, err := strconv.ParseInt(u.Query().Get("illust_id"), 10, 64); err == nil && n > 0 {
			return core.I(n)
		}
	}
	return core.I(0)
}

// DownloadURLs offers .png then .jpg when the extension is unknown.
func (e *Engine) DownloadURLs(post *core.PostInfo) []string {
	if post.File == nil {
		return nil
	}
	u := post.File.URL
	if strings.Contains(u, "/img-original/") && len(u) > 4 {
		tail := strings.ToLower(u[len(u)-4:])
		if tail == ".png" || tail == ".jpg" {
			other := u[:len(u)-4]
			if tail == ".png" {
				other += ".jpg"
			} else {
				other += ".png"
			}
			return []string{u, other}
		}
	}
	return []string{u}
}

// SearchArtists resolves a typed nick to real pixiv users (backs the «Авторы»
// autocomplete). Suggestions show the nick but carry the numeric user id as
// their search Value.
func (e *Engine) SearchArtists(c *core.ApiClient, site *core.Site, query string,
	limit int) ([]core.TagSuggestion, error) {

	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	params := url.Values{"nick": {q}, "s_mode": {"s_usr"}}
	data, err := c.GetJSON(nil, site.Base()+"/ajax/search/users", params,
		e.headers(c, site))
	if err != nil {
		return nil, err
	}
	m, ok := data.(map[string]any)
	if !ok {
		return nil, nil
	}
	if errFlag, _ := m["error"].(bool); errFlag {
		return nil, nil
	}
	body, _ := m["body"].(map[string]any)
	users, _ := body["users"].([]any)
	out := make([]core.TagSuggestion, 0, limit)
	for _, item := range users {
		u, ok := item.(map[string]any)
		if !ok {
			continue
		}
		uid := core.AsInt(u["userId"])
		name := core.Str_(u["name"])
		if uid != 0 && name != "" {
			out = append(out, core.TagSuggestion{Name: name, Category: 1,
				Value: strconv.FormatInt(uid, 10)})
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// Autocomplete suggests tags via rpc/cps.php.
func (e *Engine) Autocomplete(c *core.ApiClient, site *core.Site,
	query string) ([]core.TagSuggestion, error) {

	if len(query) < 1 {
		return nil, nil
	}
	params := url.Values{"keyword": {query}, "lang": {"en"}}
	data, err := c.GetJSON(nil, site.Base()+"/rpc/cps.php", params,
		e.headers(c, site))
	if err != nil {
		return nil, err
	}
	m, _ := data.(map[string]any)
	candidates, _ := m["candidates"].([]any)
	out := make([]core.TagSuggestion, 0, len(candidates))
	for _, item := range candidates {
		cand, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := core.Str_(cand["tag_name"])
		if name == "" {
			continue
		}
		ts := core.TagSuggestion{Name: name,
			PostCount: core.AsInt(cand["access_count"]), Category: 0}
		if tr := core.Str_(cand["tag_translation"]); tr != "" {
			ts.Antecedent = tr
		}
		out = append(out, ts)
	}
	return out, nil
}

// DeriveArtists: the userName was captured at parse time.
func (e *Engine) DeriveArtists(post *core.PostInfo, _ []string) []string {
	return append([]string(nil), post.Artists...)
}

// Register installs the engine and the pixiv.net site.
func Register() {
	core.RegisterEngine(NewEngine())
	core.RegisterSite(&core.Site{
		Key:       "pixiv",
		Label:     "pixiv.net",
		Domain:    "www.pixiv.net",
		Engine:    "pixiv",
		PageLimit: 60,
		// a logged-in session behaves like a browser loading a gallery, so a
		// score filter's per-illust detail fetches can run far brisker than
		// e621's cap — this is the main lever on Pixiv scan speed
		MinInterval:  0.2,
		RequiresAuth: true,
		AuthURL: "https://www.pixiv.net/ (войдите, затем DevTools → Application → " +
			"Cookies → PHPSESSID)",
		AuthFie: []core.CredField{
			{Key: "cookie", Label: "Cookie PHPSESSID"},
		},
	})
}
