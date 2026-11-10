// Package kemono — Kemono/Coomer engine: kemono.cr and coomer.st.
//
// Not a booru: there are no tags, ratings or scores. Content is grouped by
// creator (a (service, id) pair — a Patreon or OnlyFans account) and each post
// carries a file plus attachments. The app browses one creator's feed or the
// site-wide recent feed and downloads every file of each post (mirrored on the
// Pixiv album pattern: one grid item per post, expanded to one job per file).
//
// Two quirks shape this module:
//
//   - Cloudflare rejects the JSON endpoints unless the request sends
//     Accept: text/css plus a same-site Referer (the body is still JSON);
//   - Original files live on backend hosts (nN.<domain>) that are often
//     unreachable, while thumbnails come reliably from img.<domain>. The
//     «только превью» switch routes every download to the thumbnail host and
//     drops non-image files.
package kemono

import (
	"crypto/sha1"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"massdownloader/internal/core"
)

const (
	serviceWindow = 5 // raw 50-post pages swept per logical page when filtering by service
	perPage       = 50
)

func creatorTip() string {
	return "Имя создателя (Patreon/OnlyFans/…). Выберите из подсказок —\n" +
		"загрузится вся лента этого автора. Пусто — свежие посты сайта."
}
func serviceTip() string {
	return "Площадка автора. Помогает выбрать нужного создателя в подсказках\n" +
		"и сузить ленту свежих постов до одного сервиса."
}
func qTip() string {
	return "Поиск по словам в заголовке/тексте поста (по всей ленте сайта)."
}
func previewTip() string {
	return "Качать только превью-картинки (сервер img.…), не обращаясь к бэкенд-\n" +
		"серверам (nN.…), где лежат оригиналы и видео. Включите, если оригиналы\n" +
		"не скачиваются. Видео и не-картинки в этом режиме пропускаются."
}

var kemonoServices = []core.ChoiceOption{
	{Label: "Все сервисы", Value: ""},
	{Label: "Patreon", Value: "patreon"},
	{Label: "Pixiv Fanbox", Value: "fanbox"},
	{Label: "Fantia", Value: "fantia"},
	{Label: "Boosty", Value: "boosty"},
	{Label: "Gumroad", Value: "gumroad"},
	{Label: "SubscribeStar", Value: "subscribestar"},
	{Label: "DLsite", Value: "dlsite"},
	{Label: "Discord", Value: "discord"},
	{Label: "Afdian", Value: "afdian"},
}

var coomerServices = []core.ChoiceOption{
	{Label: "Все сервисы", Value: ""},
	{Label: "OnlyFans", Value: "onlyfans"},
	{Label: "Fansly", Value: "fansly"},
	{Label: "CandFans", Value: "candfans"},
}

func PawchiveFilters() core.FilterList {
	return filtersFor([]core.ChoiceOption{{Label: "Все сервисы", Value: ""}, {Label: "Patreon", Value: "patreon"}, {Label: "Pixiv Fanbox", Value: "fanbox"}, {Label: "Discord", Value: "discord"}})
}

func filtersFor(services []core.ChoiceOption) core.FilterList {
	return core.FilterList{
		{Kind: core.KindAuthors, Key: "creator",
			Label:        "Автор (создатель)",
			Placeholder:  "имя создателя… (пусто — лента сайта)",
			Autocomplete: "artists", Tooltip: creatorTip()},
		core.NewText("q", "Поиск по тексту", "слова в заголовке/тексте…", "q"),
		core.HideTextFilter(),
		{Kind: core.KindChoice, Key: "service", Label: "Сервис",
			Token: "service", Options: services, Tooltip: serviceTip()},
		{Kind: core.KindBool, Key: "preview_only",
			Label: "Только превью (без бэкенда)", Token: "preview-only",
			Tooltip: previewTip()},
		{Kind: core.KindBool, Key: "extract_archives", Label: "Распаковывать архивы", Token: "extract-archives", FalseToken: "no-extract-archives", DefaultBool: true},
		{Kind: core.KindBool, Key: "delete_archives", Label: "Удалять архив после распаковки", Token: "delete-archives", EnabledIf: "extract_archives"},
		{Kind: core.KindBool, Key: "extract_to_artist", Label: "Распаковывать в папку автора", Token: "extract-to-artist", EnabledIf: "extract_archives"},
		core.ImagesOnlyFilter("Только изображения (без видео)",
			"Не скачивать видео и анимацию.\nСнимите галочку, чтобы качать и ролики/GIF."),
	}
}

// KemonoFilters/CoomerFilters mirror filters.py's two lists.
func KemonoFilters() core.FilterList { return filtersFor(kemonoServices) }
func CoomerFilters() core.FilterList { return filtersFor(coomerServices) }

// Engine ports KemonoEngine.
type Engine struct {
	core.EngineBase
	mu            sync.Mutex
	creatorsIndex map[string][][3]string          // site -> [(name, svc, id)] by popularity
	creatorsNames map[string]map[[2]string]string // site -> {(svc,id): name}
	profileNames  map[[3]string]string            // (site, svc, id) -> name
}

func NewEngine() *Engine {
	return &Engine{
		creatorsIndex: map[string][][3]string{},
		creatorsNames: map[string]map[[2]string]string{},
		profileNames:  map[[3]string]string{},
	}
}

func (e *Engine) Key() string          { return "kemono" }
func (e *Engine) SupportsAlbums() bool { return true }
func (e *Engine) UsesCursor() bool     { return false } // offset pagination (o = page*50)

// Filters returns the Coomer set as the sane per-site-overridden fallback.
func (e *Engine) Filters() core.FilterList { return CoomerFilters() }

// headers is the Cloudflare handshake: Accept: text/css is the WAF bypass —
// application/json / */* get a 403.
func headers(site *core.Site) map[string]string {
	return map[string]string{"Accept": "text/css", "Referer": site.Base() + "/"}
}

// parseQuery splits the panel's query into creators/q/service/previewOnly.
// Creator tokens may be ~-prefixed when several authors are OR-ed.
type parsedQuery struct {
	creators                  [][2]string // (service, id)
	q                         string
	service                   string
	previewOnly               bool
	extract, delete, toArtist bool
}

func parseQuery(tags string) parsedQuery {
	pq := parsedQuery{extract: true}
	var qwords []string
	for _, tok := range strings.Fields(tags) {
		t := strings.TrimLeft(tok, "~")
		switch {
		case strings.HasPrefix(t, "creator:"):
			rest := t[len("creator:"):]
			if i := strings.Index(rest, ":"); i > 0 {
				svc, cid := rest[:i], rest[i+1:]
				if svc != "" && cid != "" {
					pq.creators = append(pq.creators, [2]string{svc, cid})
				}
			}
		case strings.HasPrefix(t, "service:"):
			pq.service = t[len("service:"):]
		case strings.HasPrefix(t, "q:"):
			qwords = append(qwords, t[2:])
		case t == "preview-only":
			pq.previewOnly = true
		case t == "no-extract-archives":
			pq.extract = false
		case t == "extract-archives":
			pq.extract = true
		case t == "delete-archives":
			pq.delete = true
		case t == "extract-to-artist":
			pq.toArtist = true
		}
	}
	pq.q = strings.Join(qwords, " ")
	if !pq.extract {
		pq.delete = false
		pq.toArtist = false
	}
	return pq
}

// FetchPosts runs one logical page of either feed.
func (e *Engine) FetchPosts(c *core.ApiClient, s *core.Site, tags string, limit int, cursor any) ([]*core.PostInfo, error) {
	r, err := e.FetchPage(c, s, tags, limit, cursor)
	return r.Posts, err
}

func (e *Engine) FetchPage(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) (core.PageResult, error) {

	pq := parseQuery(tags)
	pageNum := 1
	switch t := page.(type) {
	case int:
		pageNum = t
	case int64:
		pageNum = int(t)
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			pageNum = n
		}
	}
	if pageNum < 1 {
		pageNum = 1
	}
	var rows []map[string]any
	var err error
	hasMore := false
	if len(pq.creators) > 0 {
		rows, err = e.creatorFeed(c, site, pq.creators, pageNum)
		hasMore = len(rows) > 0
	} else {
		rows, hasMore, err = e.globalFeedPage(c, site, pq.q, pq.service, pageNum)
	}
	if err != nil {
		return core.PageResult{}, err
	}
	out := make([]*core.PostInfo, 0, len(rows))
	for _, d := range rows {
		if post := e.build(site, d, pq.previewOnly); post != nil {
			post.Extra["archive_extract"], post.Extra["archive_delete"], post.Extra["archive_to_artist"] = pq.extract, pq.delete, pq.toArtist
			out = append(out, post)
		}
	}
	result := core.PageResult{Posts: out, HasMore: hasMore}
	if hasMore {
		result.Next = pageNum + 1
	}
	return result, nil
}

func (e *Engine) rawPosts(c *core.ApiClient, site *core.Site, q string,
	offset int) ([]map[string]any, error) {

	params := url.Values{}
	params.Set("o", fmt.Sprint(offset))
	if q != "" {
		params.Set("q", q)
	}
	data, err := c.GetJSON(nil, site.Base()+"/api/v1/posts", params,
		headers(site))
	if err != nil {
		return nil, err
	}
	return postRows(data)
}

func postRows(data any) ([]map[string]any, error) {
	if m, ok := data.(map[string]any); ok {
		data = m["posts"]
		if data == nil {
			return nil, fmt.Errorf("Некорректный ответ ленты: отсутствует список posts")
		}
	}
	list, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("Некорректный ответ ленты: ожидался список постов")
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if d, ok := item.(map[string]any); ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func (e *Engine) globalFeedPage(c *core.ApiClient, site *core.Site, q,
	service string, page int) ([]map[string]any, bool, error) {

	if service == "" {
		rows, err := e.rawPosts(c, site, q, (page-1)*perPage)
		return rows, len(rows) > 0, err
	}
	// the API can't filter the recent feed by service, so sweep a window of
	// raw pages and keep the matches (a sparse service may end early — fine
	// for a "what's new" browse)
	base := (page - 1) * serviceWindow
	var out []map[string]any
	hasMore := true
	for k := 0; k < serviceWindow; k++ {
		rows, err := e.rawPosts(c, site, q, (base+k)*perPage)
		if err != nil {
			return nil, false, err
		}
		if len(rows) == 0 {
			hasMore = false
			break
		}
		for _, r := range rows {
			if core.Str_(r["service"]) == service {
				out = append(out, r)
			}
		}
	}
	return out, hasMore, nil
}

func (e *Engine) creatorFeed(c *core.ApiClient, site *core.Site,
	creators [][2]string, page int) ([]map[string]any, error) {

	offset := (page - 1) * perPage
	var out []map[string]any
	for _, pair := range creators {
		e.creatorName(c, site, pair[0], pair[1]) // warm the name cache
		data, err := c.GetJSON(nil,
			fmt.Sprintf("%s/api/v1/%s/user/%s/posts", site.Base(), pair[0], pair[1]),
			url.Values{"o": {fmt.Sprint(offset)}}, headers(site))
		if err != nil {
			return nil, err
		}
		rows, err := postRows(data)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// build converts one payload into PostInfo (nil when unusable).
func (e *Engine) build(site *core.Site, d map[string]any,
	previewOnly bool) *core.PostInfo {

	files := postFiles(d)
	var images [][2]string
	for _, f := range files {
		if extOf(f[1]) != "" && core.IMAGE_EXTS[extOf(f[1])] {
			images = append(images, f)
		}
	}
	usable := files
	if previewOnly {
		usable = images
	}
	svc := core.Str_(d["service"])
	uid := core.Str_(d["user"])
	rawID := core.Str_(d["id"])
	thumbPath := ""
	if len(images) > 0 {
		thumbPath = images[0][1]
	}
	preview := ""
	if thumbPath != "" {
		preview = thumbURL(site, thumbPath)
	}
	var file *core.PostFile
	if len(usable) > 0 {
		name0, path0 := usable[0][0], usable[0][1]
		var url0, ext0 string
		if previewOnly {
			url0, ext0 = thumbURL(site, path0), "jpg"
		} else {
			url0, ext0 = dataURL(site, path0, name0), extOr(path0, "bin")
		}
		file = &core.PostFile{URL: url0, MD5: "", Ext: ext0}
	}
	var artists []string
	if uid != "" {
		artists = []string{e.artistLabel(site, svc, uid)}
	}
	extraFiles := make([]any, 0, len(usable))
	archiveCount := 0
	for _, f := range usable {
		extraFiles = append(extraFiles, []any{f[0], f[1]})
		if isArchive(extOf(f[1])) {
			archiveCount++
		}
	}
	return &core.PostInfo{
		ID:              core.I(intID(rawID)),
		Site:            site.Key,
		File:            file,
		PreviewURL:      preview,
		SampleURL:       preview,
		Rating:          "?", // kemono has no rating
		Artists:         artists,
		Tags:            postTags(d["tags"]),
		CreatedAt:       firstParsed(d["published"], d["added"]),
		PageCount:       maxInt(1, len(usable)),
		AttachmentCount: len(files),
		Extra: map[string]any{
			"service": svc, "user": uid, "raw_id": rawID,
			"preview_only":  previewOnly,
			"files":         extraFiles,
			"archive_count": archiveCount,
			"title":         core.Str_(d["title"]), "content": core.Str_(d["content"]),
			"embed": d["embed"], "poll": d["poll"], "attachments": d["attachments"],
		},
	}
}

func postTags(v any) []string {
	var values []string
	if list, ok := v.([]any); ok {
		for _, item := range list {
			values = append(values, core.Str_(item))
		}
	} else {
		values = strings.Split(strings.Trim(strings.TrimSpace(core.Str_(v)), "{}"), ",")
	}
	out := []string{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// postFiles lists a post's downloadable (name, path): the main file followed
// by attachments, de-duplicated by path (the main file is often repeated).
func postFiles(d map[string]any) [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	if f, ok := d["file"].(map[string]any); ok {
		if p := core.Str_(f["path"]); p != "" {
			out = append(out, [2]string{core.Str_(f["name"]), p})
			seen[p] = true
		}
	}
	if list, ok := d["attachments"].([]any); ok {
		for _, item := range list {
			a, ok := item.(map[string]any)
			if !ok {
				continue
			}
			p := core.Str_(a["path"])
			if p != "" && !seen[p] {
				out = append(out, [2]string{core.Str_(a["name"]), p})
				seen[p] = true
			}
		}
	}
	return out
}

func extOf(path string) string {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return ""
	}
	return strings.ToLower(path[i+1:])
}

func extOr(path, dflt string) string {
	if e := extOf(path); e != "" {
		return e
	}
	return dflt
}

// thumbURL is the dedicated thumbnail CDN — reliable image/jpeg even when the
// backend originals are down.
func thumbURL(site *core.Site, path string) string {
	return "https://img." + site.Domain + "/thumbnail/data" + path
}

func dataURL(site *core.Site, path, name string) string {
	u := site.Base() + "/data" + path
	if site.StaticDomain != "" {
		u = "https://" + site.StaticDomain + "/data" + path
	}
	if name != "" {
		u += "?f=" + url.QueryEscape(name)
	}
	return u
}

// intID derives a stable positive int id for dedup/file-naming. Post ids are
// numeric strings for common services; the rare non-numeric id is hashed.
func intID(raw string) int64 {
	if allDigits(raw) {
		n, _ := strconv.ParseInt(raw, 10, 64)
		return n
	}
	if raw == "" {
		return 0
	}
	h := sha1.Sum([]byte(raw))
	v, _ := strconv.ParseInt(fmt.Sprintf("%x", h[:6]), 16, 64)
	if v == 0 {
		v = 1
	}
	return v
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstParsed(a, b any) int64 {
	if t := core.ParseTime(a); t != 0 {
		return t
	}
	return core.ParseTime(b)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ExpandPost yields one downloadable PostInfo per stashed file; single-file
// posts return unchanged (no re-fetch needed).
func (e *Engine) ExpandPost(_ *core.ApiClient, post *core.PostInfo) []*core.PostInfo {
	if post == nil {
		return nil
	}
	if post.Extra["expanded_page"] == true {
		return []*core.PostInfo{post}
	}
	var files [][2]string
	if post.Extra != nil {
		if list, ok := post.Extra["files"].([]any); ok {
			for _, item := range list {
				if pair, ok := item.([]any); ok && len(pair) >= 2 {
					files = append(files,
						[2]string{core.Str_(pair[0]), core.Str_(pair[1])})
				}
			}
		}
	}
	if len(files) <= 1 || post.File == nil {
		if len(files) == 1 && post.Extra != nil {
			post.Extra["download_name"] = files[0][0]
			post.Extra["archive"] = isArchive(extOf(files[0][1]))
		}
		return []*core.PostInfo{post}
	}
	previewOnly := false
	if post.Extra != nil {
		previewOnly, _ = post.Extra["preview_only"].(bool)
	}
	site := core.GetSite(post.Site)
	pages := make([]*core.PostInfo, 0, len(files))
	for i, entry := range files {
		name, path := entry[0], entry[1]
		var u, ext string
		if previewOnly {
			u, ext = thumbURL(site, path), "jpg"
		} else {
			u, ext = dataURL(site, path, name), extOr(path, "bin")
		}
		f := &core.PostFile{URL: u, MD5: "", Ext: ext}
		thumb := thumbURL(site, path)
		if !core.IMAGE_EXTS[extOf(path)] && !core.ANIM_EXTS[extOf(path)] {
			thumb = ""
		}
		extra := cloneExtra(post.Extra)
		extra["download_name"], extra["archive"], extra["expanded_page"] = name, isArchive(ext), true
		pages = append(pages, &core.PostInfo{
			ID: post.ID, Site: post.Site, File: f,
			PreviewURL: thumb, SampleURL: thumb,
			Rating:    post.Rating,
			Score:     post.Score,
			FavCount:  post.FavCount,
			Tags:      append([]string(nil), post.Tags...),
			AuthorID:  post.AuthorID,
			Artists:   append([]string(nil), post.Artists...),
			CreatedAt: post.CreatedAt,
			PageCount: len(files), PageIndex: i,
			Extra: extra,
		})
	}
	return pages
}

func (e *Engine) LocalPages(p *core.PostInfo) []*core.PostInfo { return e.ExpandPost(nil, p) }

func cloneExtra(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// VerifiesHash: no content hash exposed and «только превью» serves a
// downscaled derivative — nothing to verify against.
func (e *Engine) VerifiesHash(*core.PostInfo) bool { return false }

// Permalink builds the human-facing post URL from the stashed identity.
func (e *Engine) Permalink(site *core.Site, post *core.PostInfo) string {
	if post.Extra != nil {
		svc, _ := post.Extra["service"].(string)
		uid, _ := post.Extra["user"].(string)
		rid, _ := post.Extra["raw_id"].(string)
		if svc != "" && uid != "" && rid != "" {
			return fmt.Sprintf("%s/%s/user/%s/post/%s", site.Base(), svc, uid, rid)
		}
	}
	return site.Base()
}

// DeriveArtists: the artist label was resolved at parse time.
func (e *Engine) DeriveArtists(post *core.PostInfo, _ []string) []string {
	return append([]string(nil), post.Artists...)
}

// ArtistQuery browses a creator's whole feed — needs the (service, id) carried
// on the post, not the display name. Carries the «только превью» flag forward:
// if the post came preview-only, the author browser must stay preview-only.
func (e *Engine) ArtistQuery(artist string, post *core.PostInfo) string {
	if post == nil || post.Extra == nil {
		return ""
	}
	svc, _ := post.Extra["service"].(string)
	uid, _ := post.Extra["user"].(string)
	if svc == "" || uid == "" {
		return ""
	}
	query := "creator:" + svc + ":" + uid
	if pv, _ := post.Extra["preview_only"].(bool); pv {
		query += " preview-only"
	}
	if post.Extra["archive_extract"] == false {
		query += " no-extract-archives"
	} else {
		query += " extract-archives"
		if post.Extra["archive_delete"] == true {
			query += " delete-archives"
		}
		if post.Extra["archive_to_artist"] == true {
			query += " extract-to-artist"
		}
	}
	return query
}

// artistLabel picks the best display/folder name from caches we already have
// (no extra request); falls back to service/id.
func (e *Engine) artistLabel(site *core.Site, svc, uid string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if nm := e.profileNames[[3]string{site.Key, svc, uid}]; nm != "" {
		return nm
	}
	if names := e.creatorsNames[site.Key]; names != nil {
		if nm := names[[2]string{svc, uid}]; nm != "" {
			return nm
		}
	}
	if svc != "" {
		return svc + "/" + uid
	}
	return uid
}

// creatorName resolves one creator's name via the cheap /profile endpoint,
// memoised; used to label a creator feed's posts.
func (e *Engine) creatorName(c *core.ApiClient, site *core.Site,
	svc, uid string) string {

	key := [3]string{site.Key, svc, uid}
	e.mu.Lock()
	if cached, ok := e.profileNames[key]; ok {
		e.mu.Unlock()
		return cached
	}
	e.mu.Unlock()
	name := ""
	d, err := c.GetJSON(nil,
		fmt.Sprintf("%s/api/v1/%s/user/%s/profile", site.Base(), svc, uid),
		nil, headers(site))
	if err == nil {
		if m, ok := d.(map[string]any); ok {
			name = core.Str_(m["name"])
		}
	}
	if err == nil {
		e.mu.Lock()
		e.profileNames[key] = name
		e.mu.Unlock()
	}
	return name
}

// SearchArtists resolves a typed creator name against the full creators list.
// Each suggestion shows "name · service" (so duplicates across services are
// distinguishable) and commits a creator:<service>:<id> token.
func (e *Engine) SearchArtists(c *core.ApiClient, site *core.Site, query string,
	limit int) ([]core.TagSuggestion, error) {

	q := strings.ToLower(strings.TrimSpace(query))
	if len(q) < 2 {
		return nil, nil
	}
	index, err := e.loadCreators(c, site)
	if err != nil {
		return nil, err
	}
	out := make([]core.TagSuggestion, 0, limit)
	for _, row := range index {
		if strings.Contains(strings.ToLower(row[0]), q) {
			out = append(out, core.TagSuggestion{
				Name: row[0] + " · " + row[1], Category: 1,
				Value: "creator:" + row[1] + ":" + row[2],
			})
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// loadCreators fetches the full creator list once and caches it sorted by
// popularity (it powers author autocomplete and opportunistic name labels).
func (e *Engine) loadCreators(c *core.ApiClient, site *core.Site) ([][3]string, error) {
	e.mu.Lock()
	cached := e.creatorsIndex[site.Key]
	e.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	data, err := c.GetJSON(nil, site.Base()+"/api/v1/creators", nil, headers(site))
	if err != nil {
		return nil, err
	}
	type row struct {
		name, svc, cid string
		favs           int64
	}
	var rows []row
	names := map[[2]string]string{}
	list, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("Некорректный ответ API авторов: ожидался список создателей")
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		cid := core.Str_(m["id"])
		name := core.Str_(m["name"])
		svc := core.Str_(m["service"])
		if cid == "" || name == "" {
			continue
		}
		rows = append(rows, row{name, svc, cid, core.AsInt(m["favorited"])})
		names[[2]string{svc, cid}] = name
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].favs > rows[j].favs })
	index := make([][3]string, 0, len(rows))
	for _, r := range rows {
		index = append(index, [3]string{r.name, r.svc, r.cid})
	}
	e.mu.Lock()
	e.creatorsIndex[site.Key] = index
	e.creatorsNames[site.Key] = names
	e.mu.Unlock()
	return index, nil
}

// Register installs the engine and both sites.
func Register() {
	engine := NewEngine()
	core.RegisterEngine(engine)
	core.RegisterSite(&core.Site{
		Key:            "kemono",
		Label:          "kemono.cr",
		Domain:         "kemono.cr",
		CfProtected:    true,
		CfExtraDomains: []string{"img.kemono.cr"},
		AuthFie:        []core.CredField{},
		AuthURL:        "https://kemono.cr/",
		Engine:         "kemono",
		PageLimit:      50,
		MinInterval:    0.5, // polite pacing behind Cloudflare
		MediaInterval:  0.2, // /data and nN.kemono.cr share the per-IP limit
		Filters:        KemonoFilters(),
	})
	core.RegisterSite(&core.Site{
		Key:            "coomer",
		Label:          "coomer.st",
		Domain:         "coomer.st",
		CfProtected:    true,
		CfExtraDomains: []string{"img.coomer.st"},
		AuthFie:        []core.CredField{},
		AuthURL:        "https://coomer.st/",
		Engine:         "kemono",
		PageLimit:      50,
		MinInterval:    0.5,
		MediaInterval:  0.2,
		Filters:        CoomerFilters(),
	})
	core.RegisterSite(&core.Site{Key: "pawchive", Label: "pawchive.pw", Domain: "pawchive.pw", StaticDomain: "file.pawchive.pw", Engine: "kemono", PageLimit: 50, MinInterval: 0.5, MediaInterval: 0.5 /* file.pawchive.pw answers 429 to bursts */, Filters: PawchiveFilters()})
}

func isArchive(ext string) bool { return ext == "zip" || ext == "rar" || ext == "7z" }
