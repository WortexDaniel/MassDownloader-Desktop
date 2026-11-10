// Package sankaku — www.sankakucomplex.com (Sankaku Channel) and
// idolcomplex.com. A protocol of its own; the two sites share this engine but
// talk to different API hosts carried on each Site's StaticDomain (channel ->
// sankakuapi.com, idol -> i.sankakuapi.com).
//
// Posts: GET https://<api>/v2/posts/keyset?lang=en&limit=40&tags=… returns
// {"meta":{"next":…},"data":[…]}. Pagination is keyset, not numeric: each
// response hands back an opaque meta.next token passed as next= for the
// following page. The token is threaded through the core's cursor seam by
// stashing it on the last post's Extra.
//
// Auth: most content needs a logged-in token. The user supplies login +
// password; the engine exchanges them once at POST <api>/auth/token for a
// Bearer access_token (cached in-memory, re-fetched on 401).
//
// Tags are categorised (numeric type), so the artist tags are known exactly.
package sankaku

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"massdownloader/internal/core"
)

// Filters mirrors app/sites/sankaku/filters.py — the site's own metatags.
func Filters() core.FilterList {
	return core.FilterList{
		core.NewAuthors(),
		core.NewTags(),
		core.NewExclude(),
		// the site writes ratings as full words; map our canonical s/q/e
		{Kind: core.KindChoice, Key: "rating", Label: "Рейтинг", Token: "rating",
			Options: []core.ChoiceOption{
				{Label: "Любой", Value: ""}, {Label: "Safe", Value: "s"},
				{Label: "Questionable", Value: "q"}, {Label: "Explicit", Value: "e"},
			},
			ValueMap: map[string]string{
				"s": "safe", "q": "questionable", "e": "explicit",
			}},
		{Kind: core.KindChoice, Key: "sort", Label: "Сортировка", Token: "order",
			Options: []core.ChoiceOption{
				{Label: "По умолчанию (дата)", Value: ""},
				{Label: "Популярные", Value: "popular"},
				{Label: "Качество", Value: "quality"},
				{Label: "Случайные", Value: "random"},
			},
			Tooltip: "порядок выдачи — метатег order: сайта"},
		// Sankaku's threshold:<1..5> — minimum quality/score level; off by default
		{Kind: core.KindChoice, Key: "threshold", Label: "Порог (threshold)",
			Token: "threshold",
			Options: []core.ChoiceOption{
				{Label: "Любой", Value: ""}, {Label: "≥ 1", Value: "1"},
				{Label: "≥ 2", Value: "2"}, {Label: "≥ 3", Value: "3"},
				{Label: "≥ 4", Value: "4"}, {Label: "≥ 5", Value: "5"},
			},
			Tooltip: "минимальный порог качества/оценки (threshold: 1–5)"},
		core.ImagesOnlyFilter("Только изображения (без webm/mp4)",
			"Не скачивать видео и анимацию.\nСнимите галочку, чтобы качать и ролики/GIF."),
	}
}

// Sankaku's content-negotiation header its API requires (a plain Accept 403s).
const acceptHdr = "application/vnd.sankaku.api+json;v=2"

var filetypeExt = map[string]string{
	"image/jpeg": "jpg", "image/jpg": "jpg", "image/png": "png",
	"image/gif": "gif", "image/webp": "webp", "image/apng": "png",
	"video/mp4": "mp4", "video/webm": "webm",
}

// maxEmptyHops is how many empty-but-not-exhausted keyset windows one call
// hops over before giving up (each hop is one API round-trip).
const maxEmptyHops = 8

// tagType maps Sankaku tag type -> our shared palette category id.
var tagType = map[int64]int{0: 0, 1: 1, 2: 0, 3: 3, 4: 4, 5: 0, 8: 7, 9: 7}

// Engine ports SankakuEngine.
type Engine struct {
	core.EngineBase
	mu     sync.Mutex
	tokens map[string]tokenEntry // site_key -> ((login,password), bearer)
}

type tokenEntry struct {
	login, password, token string
}

func (e *Engine) Key() string              { return "sankaku" }
func (e *Engine) UsesCursor() bool         { return true } // keyset via post.Extra
func (e *Engine) Filters() core.FilterList { return Filters() }

// -- auth ---------------------------------------------------------------------

// api is the JSON host. It is the Site's APIDomain so the throttle applies the
// site's MinInterval and the user's limit there (StaticDomain is a fallback).
func (e *Engine) api(site *core.Site) string {
	if site.APIDomain != "" {
		return site.APIBase()
	}
	return "https://" + site.StaticDomain
}

func (e *Engine) headers(site *core.Site, token string) map[string]string {
	// NB: do NOT send "Platform: web-app" — with that header the API returns
	// only an .avif preview_url and withholds file_url/sample_url.
	h := map[string]string{
		"Accept":  acceptHdr,
		"Origin":  site.Base(),
		"Referer": site.Base() + "/",
	}
	if core.BrowserUA != "" {
		h["User-Agent"] = core.BrowserUA
	}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	return h
}

// token returns the Bearer token for site, logging in once and memoising.
// "" when the user supplied no credentials (anonymous best-effort).
func (e *Engine) token(c *core.ApiClient, site *core.Site) string {
	login := strings.TrimSpace(c.Cred(site.Key, "login"))
	password := strings.TrimSpace(c.Cred(site.Key, "password"))
	if login == "" || password == "" {
		return ""
	}
	e.mu.Lock()
	if cached, ok := e.tokens[site.Key]; ok &&
		cached.login == login && cached.password == password {
		e.mu.Unlock()
		return cached.token
	}
	e.mu.Unlock()
	tok, err := e.login(c, site, login, password)
	if err != nil {
		return ""
	}
	e.mu.Lock()
	e.tokens[site.Key] = tokenEntry{login, password, tok}
	e.mu.Unlock()
	return tok
}

func (e *Engine) login(c *core.ApiClient, site *core.Site, login,
	password string) (string, error) {

	body, _ := json.Marshal(map[string]string{"login": login, "password": password})
	hdr := e.headers(site, "")
	hdr["Content-Type"] = "application/json"
	data, err := c.PostJSON(nil, e.api(site)+"/auth/token", nil,
		strings.NewReader(string(body)), "application/json", hdr)
	if err != nil {
		return "", err
	}
	m, _ := data.(map[string]any)
	tok := core.Str_(m["access_token"])
	if tok == "" {
		return "", &core.AuthRequiredError{SiteKey: site.Key}
	}
	return tok, nil
}

func (e *Engine) invalidate(site *core.Site) {
	e.mu.Lock()
	delete(e.tokens, site.Key)
	e.mu.Unlock()
}

// apiGet performs a GET against the API host with the Bearer token attached,
// re-authenticating once if the token expired (401).
func (e *Engine) apiGet(c *core.ApiClient, site *core.Site, path string,
	params url.Values) (any, error) {

	tok := e.token(c, site)
	apiURL := e.api(site) + path
	data, err := c.GetJSON(nil, apiURL, params, e.headers(site, tok))
	if err != nil {
		if tok != "" && strings.Contains(err.Error(), "401") {
			e.invalidate(site)
			tok = e.token(c, site)
			return c.GetJSON(nil, apiURL, params, e.headers(site, tok))
		}
		return nil, err
	}
	return data, nil
}

// -- post fetching ------------------------------------------------------------

// FetchPosts runs one keyset window, hopping empty-but-live windows.
func (e *Engine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {

	// next_cursor returns "" once the keyset is exhausted -> no more pages
	if s, ok := page.(string); ok && s == "" {
		return nil, nil
	}
	// the API answers a purely negative query (only -tag exclusions) with
	// endless 429s — fail with an actionable message instead of retrying
	parts := strings.Fields(tags)
	if len(parts) > 0 {
		allNegative := true
		for _, p := range parts {
			if !strings.HasPrefix(p, "-") {
				allNegative = false
				break
			}
		}
		if allNegative {
			return nil, &core.ApiError{Msg: "Sankaku не ищет по одним " +
				"исключениям — добавьте хотя бы один обычный тег к минус-тегам."}
		}
	}
	params := url.Values{}
	params.Set("lang", "en")
	params.Set("limit", fmt.Sprint(limit))
	params.Set("tags", tags)
	// page is the int 1 on the first request, otherwise the opaque keyset
	// token handed back by the previous response
	if s, ok := page.(string); ok && s != "" && s != "1" {
		params.Set("next", s)
	}
	var rows []any
	nxt := ""
	for hop := 0; hop < maxEmptyHops; hop++ {
		data, err := e.apiGet(c, site, "/v2/posts/keyset", params)
		if err != nil {
			return nil, err
		}
		if m, ok := data.(map[string]any); ok {
			rows, _ = m["data"].([]any)
			meta, _ := m["meta"].(map[string]any)
			nxt = core.Str_(meta["next"])
		} else {
			rows, _ = data.([]any)
			nxt = ""
		}
		if len(rows) > 0 || nxt == "" {
			break
		}
		params.Set("next", nxt)
	}
	out := make([]*core.PostInfo, 0, len(rows))
	for _, item := range rows {
		if d, ok := item.(map[string]any); ok {
			out = append(out, e.ParsePost(d, site))
		}
	}
	if len(out) > 0 {
		// thread the next-page token to NextCursor without a side channel
		if out[len(out)-1].Extra == nil {
			out[len(out)-1].Extra = map[string]any{}
		}
		out[len(out)-1].Extra["_next"] = nxt
	}
	return out, nil
}

// NextCursor reads the token stashed on the batch's last post.
func (e *Engine) NextCursor(_ string, batch []*core.PostInfo, _ any) any {
	if len(batch) == 0 {
		return ""
	}
	if batch[len(batch)-1].Extra != nil {
		if nxt, ok := batch[len(batch)-1].Extra["_next"].(string); ok {
			return nxt
		}
	}
	return ""
}

// ParsePost converts one Sankaku payload. The new Sankaku post id is an
// opaque alphanumeric *string* — keep the raw id for the permalink and derive
// a stable int for dedup/file-naming (same trick as the Kemono engine).
func (e *Engine) ParsePost(d map[string]any, site *core.Site) *core.PostInfo {
	rawID := core.Str_(d["id"])
	md5 := core.Str_(d["md5"])
	fileURL := core.Str_(d["file_url"])
	ext := strings.ToLower(core.Str_(d["file_ext"]))
	if ext == "" {
		ext = e.extOf(fileURL, core.Str_(d["file_type"]))
	}
	var file *core.PostFile
	if fileURL != "" {
		file = &core.PostFile{URL: fileURL, MD5: md5, Ext: ext,
			Size:   core.AsInt(d["file_size"]),
			Width:  int(core.AsInt(d["width"])),
			Height: int(core.AsInt(d["height"]))}
	}
	var tagNames, artists []string
	if tagList, ok := d["tags"].([]any); ok {
		for _, item := range tagList {
			t, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name := tagName(t)
			if name == "" {
				continue
			}
			tagNames = append(tagNames, name)
			if core.AsInt(t["type"]) == 1 {
				artists = append(artists, name)
			}
		}
	}
	// The API's preview_url is .avif. WebView2 (Chromium) decodes avif
	// natively, so unlike the Qt build the grid thumb can use the light
	// preview directly; the still/sample fallbacks remain for video posts
	// (their sample IS the mp4).
	sampleRaw := core.Str_(d["sample_url"])
	previewRaw := core.Str_(d["preview_url"])
	still := sampleRaw
	if !isImageURL(still) {
		if isImageURL(fileURL) {
			still = fileURL
		} else if previewRaw != "" {
			still = previewRaw
		} else {
			still = sampleRaw
		}
	}
	thumb := previewRaw
	if thumb == "" {
		thumb = still
	}
	score := core.AsInt(d["total_score"])
	if score == 0 {
		score = core.AsInt(d["score"])
	}
	return &core.PostInfo{
		ID:         core.I(stableInt(rawID)),
		Site:       site.Key,
		File:       file,
		PreviewURL: thumb,
		SampleURL:  still,
		Score:      score,
		FavCount:   core.AsInt(d["fav_count"]),
		Rating:     core.NormRating(d["rating"]),
		Artists:    artists,
		Tags:       tagNames,
		CreatedAt:  epoch(d["created_at"]),
		Extra:      map[string]any{"raw_id": rawID},
	}
}

// tagName is the searchable tag slug — Sankaku's tagName (already
// underscored), falling back to a normalised name/name_en.
func tagName(t map[string]any) string {
	raw := core.Str_(t["tagName"])
	if raw == "" {
		raw = core.Str_(t["name"])
	}
	if raw == "" {
		raw = core.Str_(t["name_en"])
	}
	return strings.ToLower(strings.ReplaceAll(raw, " ", "_"))
}

// isImageURL: whether u points at a still image (drops the query string).
func isImageURL(u string) bool {
	if u == "" {
		return false
	}
	path := strings.SplitN(u, "?", 2)[0]
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return false
	}
	return core.IMAGE_EXTS[strings.ToLower(path[i+1:])]
}

// stableInt derives a stable positive int from the (often non-numeric) post
// id, for dedup and <id>_<md5> file naming. Numeric ids pass through; opaque
// string ids are hashed.
func stableInt(raw string) int64 {
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

// Permalink addresses the post by its real (string) id kept on Extra —
// post.ID is the derived int and would 404 for hashed ids.
func (e *Engine) Permalink(site *core.Site, post *core.PostInfo) string {
	raw := ""
	if post.Extra != nil {
		raw, _ = post.Extra["raw_id"].(string)
	}
	if raw == "" {
		raw = post.ID.String()
	}
	return fmt.Sprintf("%s/posts/%s", site.Base(), raw)
}

// epoch handles Sankaku's {"json_class":"Time","s":<epoch>,…} shape.
func epoch(v any) int64 {
	if m, ok := v.(map[string]any); ok {
		return core.AsInt(m["s"])
	}
	return core.ParseTime(v)
}

func (e *Engine) extOf(fileURL, fileType string) string {
	if fileURL != "" {
		path := strings.SplitN(fileURL, "?", 2)[0]
		if i := strings.LastIndex(path, "."); i >= 0 {
			ext := strings.ToLower(path[i+1:])
			if len(ext) >= 1 && len(ext) <= 5 {
				return ext
			}
		}
	}
	return filetypeExt[strings.ToLower(fileType)]
}

// VerifiesHash: images are served verbatim (md5 checks out); video may be
// transcoded.
func (e *Engine) VerifiesHash(post *core.PostInfo) bool {
	return !(post.File != nil && core.ANIM_EXTS[post.File.Ext])
}

var sankakuIDRe = regexp.MustCompile(`/post(?:s|/show)/(\d+)(?:$|[/?#])`)

// IdFromURL: permalinks /posts/<id>, /post/show/<id>, optionally /<lang>/
// prefixed. Only legacy *numeric* ids are readable — reverse-search providers
// index old chan URLs which carry them.
func (e *Engine) IdFromURL(_ *core.Site, urlStr string) core.PostId {
	if m := sankakuIDRe.FindStringSubmatch(urlStr); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return core.I(n)
	}
	return core.I(0)
}

// PostByID: the modern API no longer resolves legacy numeric ids (an id:<n>
// search is a 400) — degrade to "not found" instead of surfacing an HTTP
// error in the reverse-search dialog.
func (e *Engine) PostByID(c *core.ApiClient, site *core.Site,
	id core.PostId) (*core.PostInfo, error) {

	post, err := e.EngineBase.PostByID(c, site, id)
	if err != nil {
		var apiErr *core.ApiError
		if asApiErr(err, &apiErr) {
			return nil, nil
		}
		return nil, err
	}
	return post, nil
}

func asApiErr(err error, target **core.ApiError) bool {
	if e, ok := err.(*core.ApiError); ok {
		*target = e
		return true
	}
	return false
}

// -- tag suggestions ----------------------------------------------------------

// Autocomplete suggests via /tags/autosuggestCreating.
func (e *Engine) Autocomplete(c *core.ApiClient, site *core.Site,
	query string) ([]core.TagSuggestion, error) {

	if len(query) < 2 {
		return nil, nil
	}
	params := url.Values{}
	params.Set("tag", query)
	params.Set("lang", "en")
	data, err := e.apiGet(c, site, "/tags/autosuggestCreating", params)
	if err != nil {
		return nil, nil // Python swallows ApiError here too
	}
	list, _ := data.([]any)
	out := make([]core.TagSuggestion, 0, len(list))
	for _, item := range list {
		d, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := tagName(d)
		if name == "" {
			continue
		}
		en := core.Str_(d["name_en"])
		ts := core.TagSuggestion{
			Name:      name,
			PostCount: firstNonZero(core.AsInt(d["post_count"]), core.AsInt(d["count"])),
			Category:  tagType[core.AsInt(d["type"])],
		}
		if en != "" && en != name {
			ts.Antecedent = en
		}
		out = append(out, ts)
	}
	return out, nil
}

// SearchArtists filters artist-type suggestions (fallback: all).
func (e *Engine) SearchArtists(c *core.ApiClient, site *core.Site, query string,
	limit int) ([]core.TagSuggestion, error) {

	if query == "" {
		return nil, nil
	}
	sugg, err := e.Autocomplete(c, site, query)
	if err != nil {
		return nil, err
	}
	var artists []core.TagSuggestion
	for _, t := range sugg {
		if t.Category == 1 {
			artists = append(artists, t)
		}
	}
	if len(artists) == 0 {
		artists = sugg
	}
	if len(artists) > limit {
		artists = artists[:limit]
	}
	return artists, nil
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// DeriveArtists: tags are categorised, so artists are filled at parse time.
func (e *Engine) DeriveArtists(post *core.PostInfo, hintTags []string) []string {
	return core.DeriveArtistsDefault(post, hintTags)
}

// Register installs the engine and both sites.
func Register() {
	core.RegisterEngine(&Engine{tokens: map[string]tokenEntry{}})
	core.RegisterSite(&core.Site{
		Key:          "sankaku",
		Label:        "Sankaku Channel",
		Domain:       "www.sankakucomplex.com",
		APIDomain:    "sankakuapi.com", // JSON API: receives MinInterval and user limits
		StaticDomain: "sankakuapi.com", // the API host (not a CDN here)
		Engine:       "sankaku",
		PageLimit:    40,  // the keyset endpoint caps a page at 40
		MinInterval:  1.0, // Sankaku is strict about request rate
		RequiresAuth: true,
		AuthFie: []core.CredField{
			{Key: "login", Label: "Логин (e-mail/имя)"},
			{Key: "password", Label: "Пароль"},
		},
		AuthURL: "https://www.sankakucomplex.com/",
	})
	core.RegisterSite(&core.Site{
		Key:          "idolcomplex",
		Label:        "Idol Complex",
		Domain:       "idolcomplex.com",
		APIDomain:    "i.sankakuapi.com", // idol's own API host (throttled as API)
		StaticDomain: "i.sankakuapi.com",
		Engine:       "sankaku",
		PageLimit:    40,
		MinInterval:  1.0,
		RequiresAuth: true,
		AuthFie: []core.CredField{
			{Key: "login", Label: "Логин (e-mail/имя)"},
			{Key: "password", Label: "Пароль"},
		},
		AuthURL: "https://www.idolcomplex.com/",
	})
}
