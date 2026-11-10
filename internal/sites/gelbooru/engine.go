// Package gelbooru — rule34.xxx and friends.
//
// GET /index.php?page=dapi&s=post&q=index&json=1&tags=...&limit=...&pid=N —
// a flat list (or {"post":[...]}); 0-based pid pagination. The content hash is
// in hash (md5=32 or sha1=40 hex); the tag string is HTML-escaped; rating is a
// full word; there is no fav count. Posts REQUIRE auth (api_key+user_id).
// Tag suggestions use the site's own /autocomplete.php?q= (prefix match,
// popularity-ordered, carries a word type) — needs a Referer header or 403s.
package gelbooru

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"massdownloader/internal/core"
)

// Filters mirrors app/sites/gelbooru/filters.py (rating uses full words).
func Filters() core.FilterList {
	return core.FilterList{
		core.NewAuthors(),
		core.NewTags(),
		core.NewExclude(),
		core.NewThreshold("score", "Score", "score", 100),
		{Kind: core.KindChoice, Key: "rating", Label: "Рейтинг", Token: "rating",
			Options: []core.ChoiceOption{
				{Label: "Любой", Value: ""}, {Label: "Safe", Value: "s"},
				{Label: "Questionable", Value: "q"}, {Label: "Explicit", Value: "e"},
			},
			// canonical s/q/e -> the words Gelbooru expects
			ValueMap: map[string]string{
				"s": "safe", "q": "questionable", "e": "explicit",
			}},
		// gelbooru's sort metatag is sort:<field>:<dir>; default newest-first
		{Kind: core.KindChoice, Key: "sort", Label: "Сортировка", Token: "sort",
			Options: []core.ChoiceOption{
				{Label: "Сначала новые", Value: ""},
				{Label: "Сначала старые", Value: "id:asc"},
			},
			Tooltip: "порядок по времени загрузки"},
		core.ImagesOnlyFilter("Только изображения (без webm/mp4)",
			"Не скачивать видео и анимацию.\nСнимите галочку, чтобы качать и ролики/GIF."),
	}
}

// Engine ports GelbooruEngine.
type Engine struct{ core.EngineBase }

func (e *Engine) Key() string              { return "gelbooru" }
func (e *Engine) UsesCursor() bool         { return false }
func (e *Engine) Filters() core.FilterList { return Filters() }

// tagTypeWord maps the autocomplete word type to our category id.
var tagTypeWord = map[string]int{
	"general": 0, "artist": 1, "copyright": 3, "character": 4,
	"metadata": 7, "deprecated": 6,
}

var countRe = regexp.MustCompile(`\(([\d,]+)\)\s*$`)

// FetchPosts runs one dapi page. Our pages are 1-based; Gelbooru pid is 0-based.
func (e *Engine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {
	return fetchPostsWithParser(c, site, tags, limit, page, e.ParsePost)
}

// Embedded Go methods do not dispatch the derived engine's ParsePost.
func fetchPostsWithParser(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any, parse func(map[string]any, *core.Site) *core.PostInfo) ([]*core.PostInfo, error) {

	pid := 0
	switch t := page.(type) {
	case int:
		pid = t - 1
	case int64:
		pid = int(t) - 1
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			pid = n - 1
		}
	}
	if pid < 0 {
		pid = 0
	}
	q := url.Values{}
	q.Set("page", "dapi")
	q.Set("s", "post")
	q.Set("q", "index")
	q.Set("json", "1")
	q.Set("tags", tags)
	q.Set("limit", fmt.Sprint(limit))
	q.Set("pid", fmt.Sprint(pid))
	for k, v := range c.AuthParams(site) {
		q.Set(k, v)
	}
	_, body, err := c.Request(nil, "GET", site.APIBase()+"/index.php", q, nil, "", nil)
	if authErrorText(string(bytes.TrimSpace(body))) {
		return nil, &core.AuthRequiredError{SiteKey: site.Key}
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	var data any
	if err := core.DecodeJSON(body, &data); err != nil {
		return nil, &core.ApiError{Msg: fmt.Sprintf("Некорректный JSON: %v", err)}
	}
	if s, ok := data.(string); ok {
		if authErrorText(s) {
			return nil, &core.AuthRequiredError{SiteKey: site.Key}
		}
		return nil, &core.ApiError{Msg: "Ошибка API: " + core.RedactSecrets(s)}
	}
	if m, ok := data.(map[string]any); ok {
		message := core.Str_(m["message"])
		if message == "" {
			message = core.Str_(m["error"])
		}
		if authErrorText(message) {
			return nil, &core.AuthRequiredError{SiteKey: site.Key}
		}
		if success, ok := m["success"].(bool); (ok && !success) || m["error"] != nil || (message != "" && m["post"] == nil) {
			return nil, &core.ApiError{Msg: "Ошибка API: " + core.RedactSecrets(message)}
		}
		list, _ := m["post"].([]any)
		data = list
	}
	list, _ := data.([]any)
	out := make([]*core.PostInfo, 0, len(list))
	for _, item := range list {
		if d, ok := item.(map[string]any); ok {
			out = append(out, parse(d, site))
		}
	}
	return out, nil
}

func authErrorText(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, prefix := range []string{"missing authentication", "invalid authentication", "authentication required", "invalid api key", "api key is invalid", "api key not found"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// ParsePost converts one dapi payload.
func (e *Engine) ParsePost(d map[string]any, site *core.Site) *core.PostInfo {
	return parsePostCommon(d, site)
}

func parsePostCommon(d map[string]any, site *core.Site) *core.PostInfo {
	urlStr := core.Str_(d["file_url"])
	h := core.Str_(d["hash"])
	if h == "" {
		h = core.Str_(d["md5"])
	}
	ext := ""
	if u, err := url.Parse(urlStr); err == nil {
		if i := strings.LastIndex(u.Path, "."); i >= 0 {
			ext = strings.ToLower(u.Path[i+1:])
		}
	}
	var file *core.PostFile
	if urlStr != "" && h != "" {
		file = &core.PostFile{URL: urlStr, MD5: h, Ext: ext,
			Size:   core.AsInt(d["file_size"]),
			Width:  int(core.AsInt(d["width"])),
			Height: int(core.AsInt(d["height"]))}
	}
	// the flat tag string hides which tag is the artist; the uploader (owner)
	// is always present, so keep it as the fallback author
	owner := strings.TrimSpace(core.Str_(d["owner"]))
	var artists []string
	if owner != "" {
		artists = []string{owner}
	}
	sample := core.Str_(d["sample_url"])
	if sample == "" {
		sample = urlStr
	}
	createdAt := core.ParseTime(d["created_at"])
	if createdAt == 0 {
		// gelbooru.com sends a Ruby-time created_at; rule34/xbooru only carry
		// change (a unix epoch) — prefer the former when present
		createdAt = core.ParseTime(d["change"])
	}
	post := &core.PostInfo{
		ID:         core.I(core.AsInt(d["id"])),
		Site:       site.Key,
		File:       file,
		PreviewURL: core.Str_(d["preview_url"]),
		SampleURL:  sample,
		Score:      core.AsInt(d["score"]),
		FavCount:   0,
		Rating:     core.NormRating(d["rating"]),
		Artists:    artists,
		Tags:       splitWords(html.UnescapeString(core.Str_(d["tags"]))),
		CreatedAt:  createdAt,
	}
	normalizeXbooruImages(post)
	return post
}

// Xbooru's browser appends the artwork ID to original image URLs. Bare API
// URLs can return a cached HTML access page with HTTP 200 instead of media.
// Preserve the hash: the browser URL serves the original, identical bytes.
func xbooruImageURL(post *core.PostInfo, ref string) string {
	if post == nil || post.Site != "xbooru" || post.ID.Int() <= 0 || ref == "" {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil || u.Hostname() != "img.xbooru.com" || !strings.HasPrefix(strings.TrimLeft(u.Path, "/"), "images/") {
		return ref
	}
	id := post.ID.String()
	if !u.Query().Has(id) {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += id
	}
	return u.String()
}

func normalizeXbooruImages(post *core.PostInfo) bool {
	if post == nil {
		return false
	}
	changed := false
	rewrite := func(ref *string) {
		updated := xbooruImageURL(post, *ref)
		changed = changed || updated != *ref
		*ref = updated
	}
	if post.File != nil {
		rewrite(&post.File.URL)
	}
	rewrite(&post.SampleURL)
	rewrite(&post.PreviewURL)
	return changed
}

func (e *Engine) PrepareView(_ *core.ApiClient, post *core.PostInfo) (bool, error) {
	return normalizeXbooruImages(post), nil
}

func (e *Engine) DownloadURLs(post *core.PostInfo) []string {
	if post == nil || post.File == nil {
		return nil
	}
	// Also repair saved favorites and jobs queued before this fix, on retry.
	return []string{xbooruImageURL(post, post.File.URL)}
}

func splitWords(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Fields(s)
}

// DeriveArtists recovers the author from hints, else the uploader.
func (e *Engine) DeriveArtists(post *core.PostInfo, hintTags []string) []string {
	return core.HintedOrUploader(post, hintTags)
}

// ArtistQuery: a real artist tag searches directly; uploader via user:.
func (e *Engine) ArtistQuery(artist string, post *core.PostInfo) string {
	return core.TagOrUserQuery(artist, post)
}

var idRe = regexp.MustCompile(`[?&]id=(\d+)`)

// IdFromURL: gelbooru permalinks are index.php?page=post&s=view&id=<n>.
func (e *Engine) IdFromURL(_ *core.Site, urlStr string) core.PostId {
	if m := idRe.FindStringSubmatch(urlStr); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return core.I(n)
	}
	return core.I(0)
}

// VerifiesHash: Gelbooru sites (rule34/xbooru) re-encode video to mp4 on a
// separate CDN host; the served bytes no longer match the post's hash, so a
// video download can never pass verification. Images still do.
func (e *Engine) VerifiesHash(post *core.PostInfo) bool {
	return !(post.File != nil && core.ANIM_EXTS[post.File.Ext])
}

// Autocomplete suggests via /autocomplete.php (needs a Referer or 403s).
func (e *Engine) Autocomplete(c *core.ApiClient, site *core.Site,
	query string) ([]core.TagSuggestion, error) {

	return e.suggest(c, site, query, 2)
}

// SearchArtists filters artist-type suggestions; sites omitting tag types
// (xbooru) fall back to every match as a best effort.
func (e *Engine) SearchArtists(c *core.ApiClient, site *core.Site, query string,
	limit int) ([]core.TagSuggestion, error) {
	return searchArtistsWithSuggester(c, site, query, limit, e.suggest)
}

func searchArtistsWithSuggester(c *core.ApiClient, site *core.Site, query string,
	limit int, suggest func(*core.ApiClient, *core.Site, string, int) ([]core.TagSuggestion, error)) ([]core.TagSuggestion, error) {

	if query == "" {
		return nil, nil
	}
	tags, err := suggest(c, site, query, 1)
	if err != nil {
		return nil, err
	}
	var artists []core.TagSuggestion
	for _, t := range tags {
		if t.Category == 1 {
			artists = append(artists, t)
		}
	}
	if len(artists) == 0 {
		artists = tags
	}
	if len(artists) > limit {
		artists = artists[:limit]
	}
	return artists, nil
}

func (e *Engine) suggest(c *core.ApiClient, site *core.Site, query string,
	minLen int) ([]core.TagSuggestion, error) {

	if len(query) < minLen {
		return nil, nil
	}
	q := url.Values{}
	q.Set("q", query)
	data, err := c.GetJSON(nil, site.APIBase()+"/autocomplete.php", q,
		map[string]string{"Referer": site.Base() + "/"})
	if err != nil {
		return nil, err
	}
	list, _ := data.([]any)
	out := make([]core.TagSuggestion, 0, len(list))
	for _, item := range list {
		d, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := core.Str_(d["value"])
		if name == "" {
			continue
		}
		count := int64(0)
		if m := countRe.FindStringSubmatch(core.Str_(d["label"])); m != nil {
			count, _ = strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
		}
		out = append(out, core.TagSuggestion{Name: name, PostCount: count,
			Category: tagTypeWord[core.Str_(d["type"])]})
	}
	return out, nil
}

// Register installs the gelbooru engines and all four sites.
func Register() {
	core.RegisterEngine(&Engine{})
	core.RegisterEngine(&LegacyEngine{Engine{}})
	core.RegisterEngine(&GelbooruComEngine{Engine{}})
	registerRule34()
	registerXbooru()
	registerTbib()
	registerGelbooruCom()
}

func registerRule34() {
	core.RegisterSite(&core.Site{
		Key:            "rule34",
		Label:          "rule34.xxx",
		Domain:         "rule34.xxx",
		APIDomain:      "api.rule34.xxx",
		Engine:         "gelbooru",
		PageLimit:      200,
		MinInterval:    0.3, // Gelbooru tolerates a brisker pace than e621
		RequiresAuth:   true,
		CfProtected:    true,
		CfExtraDomains: []string{"api.rule34.xxx"},
		AuthURL:        "https://rule34.xxx/index.php?page=account&s=options",
	})
}

func registerXbooru() {
	core.RegisterSite(&core.Site{
		Key:         "xbooru",
		Label:       "xbooru.com",
		Domain:      "xbooru.com",
		Engine:      "gelbooru",
		PageLimit:   200,
		MinInterval: 0.3,
	})
}

func registerTbib() {
	core.RegisterSite(&core.Site{
		Key:         "tbib",
		Label:       "tbib.org",
		Domain:      "tbib.org",
		Engine:      "gelbooru_legacy",
		PageLimit:   200,
		MinInterval: 0.3,
		PostURLTm:   "https://tbib.org/index.php?page=post&s=view&id={id}",
	})
}

func registerGelbooruCom() {
	core.RegisterSite(&core.Site{
		Key:          "gelbooru",
		Label:        "gelbooru.com",
		Domain:       "gelbooru.com",
		Engine:       "gelbooru_com",
		PageLimit:    100, // gelbooru.com caps the dapi response at 100 posts
		MinInterval:  0.3, // same brisk pace rule34 tolerates
		RequiresAuth: true,
		AuthURL:      "https://gelbooru.com/index.php?page=account&s=options",
		// engine gelbooru_com bypasses the models gelbooru permalink branch,
		// so spell the index.php?page=post scheme out here.
		PostURLTm: "https://gelbooru.com/index.php?page=post&s=view&id={id}",
	})
}
