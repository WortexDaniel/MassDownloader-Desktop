// Package deviantart — not a booru; scrapes the site's server-rendered pages.
//
// DA's internal JSON API (/_puppy/…) rejects requests whose CSRF token was not
// minted for a real browser session, so this engine reads the
// window.__INITIAL_STATE__ JSON every DA page embeds for its React client.
// That state carries complete deviation entities, so one HTML page = one fully
// parsed result page.
//
//   - Search is /search?q=… (24 results/page). Pagination is a keyset cursor —
//     the full next-page URL threaded through the core's cursor seam, which
//     also lets author galleries (/<name>/gallery/all?page=N) ride the same seam.
//   - Authors aren't searchable operators: an author query browses their
//     gallery directly (by:<name>; only the first author is used). Rating and
//     favourites threshold apply client-side.
//   - Auth is optional for safe content. Guest pages carry mature entities but
//     mark them isBlocked with force-blurred media tokens, so build drops them;
//     18+ needs a pasted Cookie. The Cookie also unlocks /download originals.
//   - Watchers-only works come back blurred; PrepareView temporarily watches
//     the author to unblur (undone in Release).
package deviantart

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"massdownloader/internal/core"
)

var (
	stateRe = regexp.MustCompile(
		`window\.__INITIAL_STATE__\s*=\s*JSON\.parse\("((?:[^"\\]|\\.)*)"\)`)
	scoreRe = regexp.MustCompile(`score:(>=|<=|>|<|=)?(\d+)`)
	csrfRe  = regexp.MustCompile(`__CSRF_TOKEN__\s*=\s*'([^']+)'`)
	artIDRe = regexp.MustCompile(`/art/[^/?#]*-(\d+)(?:[/?#]|$)`)
	deviaRe = regexp.MustCompile(`/deviation/(\d+)`)
)

// thumbTypes is the preview render preference: big enough for the grid.
var thumbTypes = []string{"350T", "400T", "300W", "250T", "preview"}

var jsEsc = map[rune]rune{
	'\\': '\\', '"': '"', '\'': '\'', '/': '/', 'n': '\n', 'r': '\r',
	't': '\t', 'b': '\b', 'f': '\f', 'v': '\v', '0': 0,
}

// jsUnescape decodes the body of a JS double-quoted string literal; json's
// decoder chokes on JS-only escapes (\', \x2F …) that DA's serialiser emits.
func jsUnescape(lit string) string {
	var out []rune
	r := []rune(lit)
	for i := 0; i < len(r); i++ {
		c := r[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(r) {
			break
		}
		e := r[i]
		switch e {
		case 'u':
			if i+4 < len(r) {
				if n, err := strconv.ParseUint(string(r[i+1:i+5]), 16, 32); err == nil {
					out = append(out, rune(n))
				}
				i += 4
			}
		case 'x':
			if i+2 < len(r) {
				if n, err := strconv.ParseUint(string(r[i+1:i+3]), 16, 32); err == nil {
					out = append(out, rune(n))
				}
				i += 2
			}
		default:
			if v, ok := jsEsc[e]; ok {
				out = append(out, v)
			} else {
				out = append(out, e)
			}
		}
	}
	return string(out)
}

// withParam sets/replaces rawURL's key query parameter with value.
func withParam(rawURL, key, value string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Del(key)
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// Filters mirrors app/sites/other/deviantart_filters.py.
func Filters() core.FilterList {
	return core.FilterList{
		{Kind: core.KindAuthors, Key: "artists", Label: "Автор (галерея)",
			Placeholder:  "имя автора… (пусто — поиск по словам)",
			Autocomplete: "artists",
			Tooltip: "Показать галерею автора DeviantArt целиком\n" +
				"(страница /имя/gallery/all). Введите имя как в URL\n" +
				"профиля. Учитывается только первый автор; если задан —\n" +
				"ключевые слова игнорируются."},
		{Kind: core.KindTags, Key: "include",
			Label:        "Ключевые слова / теги",
			Placeholder:  "слово, тег… (пусто — по автору)",
			Autocomplete: "tags"},
		{Kind: core.KindExclude, Key: "exclude", Label: "Исключить слова",
			Placeholder: "исключить тег…", Autocomplete: "tags",
			Tooltip: "Слова добавляются в запрос со знаком «-»\n" +
				"(оператор исключения поиска DeviantArt)."},
		{Kind: core.KindThreshold, Key: "score", Label: "Избранное (мин.)",
			Token: "score", DefaultValue: 100,
			Minimum: -1000, Maximum: 1_000_000,
			Ops: "≥", OpTokens: ">=",
			Tooltip: "показывать работы с числом добавлений\n" +
				"в избранное не меньше заданного\n" +
				"(фильтр на стороне приложения)"},
		{Kind: core.KindChoice, Key: "rating", Label: "Рейтинг", Token: "rating",
			Options: []core.ChoiceOption{
				{Label: "Любой", Value: ""}, {Label: "Без 18+", Value: "s"},
				{Label: "Только 18+", Value: "e"},
			},
			Tooltip: "фильтр по флагу «mature content»\n" +
				"(применяется на стороне приложения)"},
	}
}

type daQuery struct {
	authors   []string
	words     []string
	rating    string
	op        string
	threshold int64
	hasThresh bool
}

func parseQuery(tags string) daQuery {
	var q daQuery
	q.op = ">="
	for _, tok := range strings.Fields(tags) {
		low := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(low, "by:"):
			name := strings.TrimLeft(strings.TrimSpace(tok[3:]), "@")
			if name != "" {
				q.authors = append(q.authors, name)
			}
		case strings.HasPrefix(low, "rating:"):
			r := low[len("rating:"):]
			if len(r) > 1 {
				r = r[:1]
			}
			q.rating = r
		case strings.HasPrefix(low, "score:"), strings.HasPrefix(low, "favcount:"):
			v := tok[strings.Index(tok, ":")+1:]
			if m := scoreRe.FindStringSubmatch("score:" + v); m != nil {
				if m[1] != "" {
					q.op = m[1]
				} else {
					q.op = "="
				}
				n, _ := strconv.ParseInt(m[2], 10, 64)
				q.threshold = n
				q.hasThresh = true
			}
		case strings.HasPrefix(low, "order:"):
			// DA's search ignores sort parameters
		default:
			q.words = append(q.words, tok)
		}
	}
	return q
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

// Engine ports DeviantArtEngine.
type Engine struct {
	core.EngineBase
	mu          sync.Mutex
	tempWatched map[string]bool // transient watches undone in Release
	csrf        string          // cached CSRF from the last page load
}

func NewEngine() *Engine {
	return &Engine{tempWatched: map[string]bool{}}
}

func (e *Engine) Key() string              { return "deviantart" }
func (e *Engine) UsesCursor() bool         { return true } // cursor == next page URL
func (e *Engine) TagPhrase() bool          { return true } // DA search is full-text
func (e *Engine) Filters() core.FilterList { return Filters() }

func cookieOf(c *core.ApiClient, siteKey string) string {
	return c.Cred(siteKey, "cookie")
}

func (e *Engine) headers(c *core.ApiClient, site *core.Site) map[string]string {
	h := map[string]string{
		"User-Agent":      core.BrowserUA,
		"Accept-Language": "en-US,en;q=0.9",
		"Accept":          "text/html,application/xhtml+xml",
	}
	if cookie := cookieOf(c, site.Key); cookie != "" {
		h["Cookie"] = cookie
	}
	return h
}

func (e *Engine) initialURL(site *core.Site, q daQuery) (string, error) {
	if len(q.authors) > 0 {
		return fmt.Sprintf("%s/%s/gallery/all", site.Base(),
			url.PathEscape(q.authors[0])), nil
	}
	if len(q.words) > 0 {
		return fmt.Sprintf("%s/search?q=%s", site.Base(),
			url.QueryEscape(strings.Join(q.words, " "))), nil
	}
	return "", &core.ApiError{
		Msg: "DeviantArt: укажите ключевые слова или автора (by:)"}
}

// fetchPage loads a DA page returning its HTML and parsed __INITIAL_STATE__.
func (e *Engine) fetchPage(c *core.ApiClient, site *core.Site,
	pageURL string) (string, map[string]any, error) {

	htmlText, err := c.GetText(nil, pageURL, nil, e.headers(c, site))
	if err != nil {
		return "", nil, err
	}
	m := stateRe.FindStringSubmatch(htmlText)
	if m == nil {
		return "", nil, &core.ApiError{Msg: "DeviantArt: страница без данных " +
			"(изменилась разметка или доступ ограничен)"}
	}
	if cm := csrfRe.FindStringSubmatch(htmlText); cm != nil {
		e.mu.Lock()
		e.csrf = cm[1]
		e.mu.Unlock()
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(jsUnescape(m[1])), &state); err != nil {
		return htmlText, nil, &core.ApiError{Msg: "DeviantArt: некорректный state"}
	}
	return htmlText, state, nil
}

// entityMap reads state[key] where key is "@@streams"/"@​@entities"-style.
func entityMap(state map[string]any, name string) map[string]any {
	for k, v := range state {
		kk := strings.TrimLeft(k, "@")
		if kk == name {
			m, _ := v.(map[string]any)
			return m
		}
	}
	return nil
}

// pickStream finds the deviation stream: search pages declare
// itemType:deviation; gallery pages use a folder-deviations-… stream.
func pickStream(state map[string]any) map[string]any {
	streams := entityMap(state, "streams")
	for _, st := range streams {
		s, ok := st.(map[string]any)
		if !ok {
			continue
		}
		if sp, ok := s["streamParams"].(map[string]any); ok &&
			core.Str_(sp["itemType"]) == "deviation" {
			return s
		}
	}
	for k, st := range streams {
		s, ok := st.(map[string]any)
		if !ok {
			continue
		}
		if _, hasItems := s["items"]; hasItems && strings.HasPrefix(k, "folder-deviations") {
			return s
		}
	}
	return nil
}

func (e *Engine) collect(site *core.Site, state map[string]any,
	stream map[string]any, pageURL string, q daQuery,
	limit int) ([]*core.PostInfo, error) {

	ents := entityMap(state, "entities")
	devs := map[string]any{}
	users := map[string]any{}
	if ents != nil {
		if d, ok := ents["deviation"].(map[string]any); ok {
			devs = d
		}
		if u, ok := ents["user"].(map[string]any); ok {
			users = u
		}
	}
	items, _ := stream["items"].([]any)
	out := make([]*core.PostInfo, 0, len(items))
	for _, item := range items {
		key := ""
		switch t := item.(type) {
		case string:
			key = t
		default:
			key = fmt.Sprint(item)
		}
		d, _ := devs[key].(map[string]any)
		if d == nil {
			continue
		}
		p := e.build(site, d, users)
		if p == nil {
			continue
		}
		if q.rating == "s" && p.Rating != "s" {
			continue
		}
		if (q.rating == "e" || q.rating == "q") && p.Rating == "s" {
			continue
		}
		if q.hasThresh && !passes(p.Score, q.op, q.threshold) {
			continue
		}
		out = append(out, p)
	}
	if len(out) > 0 {
		next := nextURL(pageURL, stream, len(q.authors) > 0)
		if out[len(out)-1].Extra == nil {
			out[len(out)-1].Extra = map[string]any{}
		}
		out[len(out)-1].Extra["_next"] = next
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FetchPosts runs one search/gallery page.
func (e *Engine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {

	if s, ok := page.(string); ok && s == "" {
		return nil, nil // cursor exhausted
	}
	q := parseQuery(tags)
	pageURL := ""
	if s, ok := page.(string); ok && strings.HasPrefix(s, "http") {
		pageURL = s
	} else {
		var err error
		pageURL, err = e.initialURL(site, q)
		if err != nil {
			return nil, err
		}
	}
	_, state, err := e.fetchPage(c, site, pageURL)
	if err != nil {
		return nil, err
	}
	stream := pickStream(state)
	if stream == nil {
		stream = map[string]any{}
	}
	return e.collect(site, state, stream, pageURL, q, limit)
}

// nextURL builds the URL of the page after pageURL: search pages carry a
// keyset cursor in their stream, gallery pages use plain ?page=N. An empty
// string ends pagination.
func nextURL(pageURL string, stream map[string]any, gallery bool) string {
	if more, ok := stream["hasMore"].(bool); ok && !more {
		return ""
	}
	if !gallery {
		cur := core.Str_(stream["cursor"])
		if cur != "" {
			return withParam(pageURL, "cursor", cur)
		}
		return ""
	}
	u, err := url.Parse(pageURL)
	if err != nil {
		return withParam(pageURL, "page", "2")
	}
	n := core.AsInt(u.Query().Get("page"))
	if n == 0 {
		n = 1
	}
	return withParam(pageURL, "page", strconv.FormatInt(n+1, 10))
}

// NextCursor reads the stashed next-page URL.
func (e *Engine) NextCursor(_ string, batch []*core.PostInfo, _ any) any {
	if len(batch) == 0 {
		return ""
	}
	last := batch[len(batch)-1]
	if last.Extra != nil {
		if nxt, ok := last.Extra["_next"].(string); ok {
			return nxt
		}
	}
	return ""
}

// -- post building ------------------------------------------------------------

type mediaURL struct {
	URL         string
	W, H, FSize int64
}

// tokenBlurred reports whether a deviation's wixmp media token forces a blur
// — the signal that the viewer lacks access. DA bakes a "blur":">=N"
// constraint into the signed JWT it issues to non-subscribers, so every render
// comes back blurred.
func tokenBlurred(media map[string]any) bool {
	toks, _ := media["token"].([]any)
	for _, t := range toks {
		tok, ok := t.(string)
		if !ok || !strings.Contains(tok, ".") {
			continue
		}
		parts := strings.Split(tok, ".")
		if len(parts) < 2 {
			continue
		}
		payload := parts[1]
		if pad := len(payload) % 4; pad != 0 {
			payload += strings.Repeat("=", 4-pad)
		}
		body, err := base64.URLEncoding.DecodeString(payload)
		if err != nil {
			continue // malformed token -> assume not blurred
		}
		var parsed struct {
			Obj [][]map[string]any `json:"obj"`
		}
		if json.Unmarshal(body, &parsed) != nil {
			continue
		}
		for _, grp := range parsed.Obj {
			for _, cons := range grp {
				if _, has := cons["blur"]; has {
					return true
				}
			}
		}
	}
	return false
}

// isGated: locked behind following/subscribing (premiumFolderData.hasAccess is
// False) or simply blurred. Such posts download as useless blurred
// placeholders unless the viewer gains access.
func isGated(d map[string]any) bool {
	if pfd, ok := d["premiumFolderData"].(map[string]any); ok {
		if acc, has := pfd["hasAccess"].(bool); has && !acc {
			return true
		}
	}
	media, _ := d["media"].(map[string]any)
	if media == nil {
		return false
	}
	return tokenBlurred(media)
}

// mediaURLs maps type-name -> render info for a deviation's wixmp media
// descriptor. A type without a transform path (c) is the base file itself;
// every URL carries the media token when present.
func mediaURLs(media map[string]any) map[string]mediaURL {
	base := core.Str_(media["baseUri"])
	pretty := core.Str_(media["prettyName"])
	toks, _ := media["token"].([]any)
	tok := ""
	if len(toks) > 0 {
		if s, ok := toks[0].(string); ok && s != "" {
			tok = "?token=" + url.QueryEscape(s)
		}
	}
	out := map[string]mediaURL{}
	types, _ := media["types"].([]any)
	for _, item := range types {
		t, ok := item.(map[string]any)
		if !ok || core.Str_(t["t"]) == "" {
			continue
		}
		c := core.Str_(t["c"])
		name := strings.ReplaceAll(c, "<prettyName>", pretty)
		u := base + name
		if c == "" {
			u = base
		}
		out[core.Str_(t["t"])] = mediaURL{
			URL:   u + tok,
			W:     core.AsInt(t["w"]),
			H:     core.AsInt(t["h"]),
			FSize: core.AsInt(t["f"]),
		}
	}
	return out
}

// build converts one deviation entity into PostInfo (nil when unusable).
func (e *Engine) build(site *core.Site, d, users map[string]any) *core.PostInfo {
	// journals/literature/video are not downloadable images; tier teasers and
	// deleted works carry no usable media either
	if core.Str_(d["type"]) != "image" {
		return nil
	}
	if blocked, _ := d["isBlocked"].(bool); blocked {
		return nil
	}
	if deleted, _ := d["isDeleted"].(bool); deleted {
		return nil
	}
	rid := core.AsInt(d["deviationId"])
	media, _ := d["media"].(map[string]any)
	if media == nil {
		media = map[string]any{}
	}
	mu := mediaURLs(media)
	full, ok := mu["fullview"]
	if !ok {
		full, ok = mu["preview"]
	}
	if rid == 0 || !ok {
		return nil
	}
	ext := "jpg"
	if u, err := url.Parse(full.URL); err == nil {
		p := u.Path
		if i := strings.LastIndex(p, "."); i >= 0 && i > strings.LastIndex(p, "/") {
			ext = strings.ToLower(p[i+1:])
		}
	}
	furl := full.URL
	thumb := furl
	for _, tt := range thumbTypes {
		if m, has := mu[tt]; has {
			thumb = m.URL
			break
		}
	}
	username := ""
	if authorKey := fmt.Sprint(d["author"]); authorKey != "" {
		if author, ok := users[authorKey].(map[string]any); ok {
			username = core.Str_(author["username"])
		}
	}
	stats, _ := d["stats"].(map[string]any)
	faves := int64(0)
	if stats != nil {
		faves = core.AsInt(stats["favourites"])
	}
	rating := "s"
	if mature, _ := d["isMature"].(bool); mature {
		rating = "e"
	}
	var artists []string
	if username != "" {
		artists = []string{username}
	}
	pageURL := core.Str_(d["url"])
	dlOK, _ := d["isDownloadable"].(bool)
	return &core.PostInfo{
		ID:   core.I(rid),
		Site: site.Key,
		File: &core.PostFile{URL: furl, Ext: ext, Size: full.FSize,
			Width: int(full.W), Height: int(full.H)},
		PreviewURL: thumb,
		SampleURL:  furl,
		Score:      faves,
		FavCount:   faves,
		Rating:     rating,
		Artists:    artists,
		AuthorID:   username,
		CreatedAt:  core.ParseTime(d["publishedTime"]),
		Extra: map[string]any{
			"url":   pageURL,
			"dl_ok": dlOK,
			"gated": isGated(d),
		},
	}
}

// -- original / unblurred resolution ------------------------------------------

// ExpandPost never multiplies jobs (DA works are single images) — it is the
// download-time hook that fetches the original file and unblurs gated works.
func (e *Engine) ExpandPost(c *core.ApiClient, post *core.PostInfo) []*core.PostInfo {
	e.resolve(c, post, true)
	return []*core.PostInfo{post}
}

// PrepareView unblurs a gated work for the viewer: temporarily watch the
// author (if needed) and rewrite URLs to fresh unblurred renders. Returns
// whether the post changed. Doesn't chase the heavy /download original.
func (e *Engine) PrepareView(c *core.ApiClient, post *core.PostInfo) (bool, error) {
	if post == nil || post.Extra == nil {
		return false, nil
	}
	gated, _ := post.Extra["gated"].(bool)
	if !gated {
		return false, nil
	}
	return e.resolve(c, post, false), nil
}

// resolve loads the art page and, for a still-gated work whose author the user
// doesn't already follow, temporarily watches them and re-loads so DA hands
// back unblurred media. Then refreshes the post's URLs from that state.
func (e *Engine) resolve(c *core.ApiClient, post *core.PostInfo,
	wantOriginal bool) bool {

	if c == nil || post.File == nil {
		return false
	}
	extra := post.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	pageURL, _ := extra["url"].(string)
	cookie := cookieOf(c, post.Site)
	gated, _ := extra["gated"].(bool)
	dlOK, _ := extra["dl_ok"].(bool)
	wanted := gated || (wantOriginal && dlOK)
	if !(cookie != "" && pageURL != "" && wanted) {
		return false
	}
	site := core.GetSite(post.Site)
	htmlText, state, err := e.fetchPage(c, site, pageURL)
	if err != nil {
		return false // degrade to the blurred render
	}
	if gated && stillGated(state, post.ID.Int()) &&
		e.unlockViaWatch(c, site, state, htmlText, post.ID.Int()) {
		if _, state2, err2 := e.fetchPage(c, site, pageURL); err2 == nil {
			state = state2
		}
	}
	e.applyMedia(state, post, wantOriginal)
	return true
}

// applyMedia refreshes post's URLs from an art-page state: the unblurred
// renders always, plus (for downloads) the /download original first.
func (e *Engine) applyMedia(state map[string]any, post *core.PostInfo,
	wantOriginal bool) {

	ents := entityMap(state, "entities")
	if ents == nil {
		return
	}
	devs, _ := ents["deviation"].(map[string]any)
	if d, ok := devs[post.ID.String()].(map[string]any); ok && d != nil {
		media, _ := d["media"].(map[string]any)
		if media != nil {
			mu := mediaURLs(media)
			fresh, hasFull := mu["fullview"]
			if !hasFull {
				fresh, hasFull = mu["preview"]
			}
			if hasFull {
				post.File.URL = fresh.URL
				post.SampleURL = fresh.URL
			}
			for _, tt := range thumbTypes {
				if m, has := mu[tt]; has {
					post.PreviewURL = m.URL
					break
				}
			}
		}
	}
	if !wantOriginal {
		return
	}
	dxRaw, _ := ents["deviationExtended"].(map[string]any)
	dx, _ := dxRaw[post.ID.String()].(map[string]any)
	if dx == nil {
		return
	}
	dlObj, _ := dx["download"].(map[string]any)
	dl := ""
	if dlObj != nil {
		dl = strings.TrimSpace(core.Str_(dlObj["url"]))
	}
	if dl != "" {
		post.Extra["dl"] = []any{dl}
		orig, _ := dx["originalFile"].(map[string]any)
		if orig != nil {
			if t := core.Str_(orig["type"]); t != "" {
				tl := strings.ToLower(t)
				if tl == "jpeg" {
					tl = "jpg"
				}
				post.File.Ext = tl
			}
		}
	}
}

// -- transient watch to unlock watchers-only works ----------------------------

func stillGated(state map[string]any, postID int64) bool {
	ents := entityMap(state, "entities")
	if ents == nil {
		return false
	}
	devs, _ := ents["deviation"].(map[string]any)
	d, _ := devs[strconv.FormatInt(postID, 10)].(map[string]any)
	return d != nil && isGated(d)
}

// unlockViaWatch follows the work's author so DA lifts the blur — but only if
// the user doesn't already follow them (never unwatches a genuine follow).
// Records the follow so Release can undo it; returns true when a NEW follow
// was made. A paid-subscription gate can't be opened and is left.
func (e *Engine) unlockViaWatch(c *core.ApiClient, site *core.Site,
	state map[string]any, htmlText string, postID int64) bool {

	ents := entityMap(state, "entities")
	if ents == nil {
		return false
	}
	devs, _ := ents["deviation"].(map[string]any)
	d, _ := devs[strconv.FormatInt(postID, 10)].(map[string]any)
	if d == nil {
		return false
	}
	users, _ := ents["user"].(map[string]any)
	author, _ := users[fmt.Sprint(d["author"])].(map[string]any)
	if author == nil {
		return false
	}
	username := core.Str_(author["username"])
	if username == "" {
		return false
	}
	if watching, _ := author["isWatching"].(bool); watching {
		return false
	}
	e.mu.Lock()
	if e.tempWatched[username] {
		e.mu.Unlock()
		return false
	}
	e.mu.Unlock()
	cm := csrfRe.FindStringSubmatch(htmlText)
	csrf := e.csrf
	if cm != nil {
		csrf = cm[1]
	}
	if !e.watchCall(c, site, "watch", username, csrf) {
		return false
	}
	e.mu.Lock()
	e.tempWatched[username] = true
	e.mu.Unlock()
	return true
}

// watchCall POSTs DA's dashared/user/{watch,unwatch} for username. True on
// {"success":true}; never raises.
func (e *Engine) watchCall(c *core.ApiClient, site *core.Site, action,
	username, csrf string) bool {

	if username == "" || csrf == "" {
		return false
	}
	hdr := e.headers(c, site)
	hdr["Accept"] = "application/json"
	hdr["Origin"] = site.Base()
	hdr["Referer"] = site.Base() + "/"
	hdr["X-Requested-With"] = "XMLHttpRequest"
	form := url.Values{"username": {username}, "csrf_token": {csrf}}
	resp, err := c.PostJSON(nil,
		fmt.Sprintf("%s/_puppy/dashared/user/%s", site.Base(), action), nil,
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded", hdr)
	if err != nil {
		return false
	}
	m, ok := resp.(map[string]any)
	if !ok {
		return false
	}
	success, _ := m["success"].(bool)
	return success
}

// Release unwatches every author this engine temporarily followed during the
// run, restoring the user's follow list. Safe when nothing pending.
func (e *Engine) Release(c *core.ApiClient) {
	e.mu.Lock()
	names := make([]string, 0, len(e.tempWatched))
	for name := range e.tempWatched {
		names = append(names, name)
	}
	e.tempWatched = map[string]bool{}
	csrf := e.csrf
	e.mu.Unlock()
	if len(names) == 0 || c == nil {
		return
	}
	site := core.GetSite("deviantart")
	if site == nil {
		return
	}
	if csrf == "" { // need a token to POST the unwatch
		if _, _, err := e.fetchPage(c, site, site.Base()+"/"); err == nil {
			e.mu.Lock()
			csrf = e.csrf
			e.mu.Unlock()
		}
	}
	for _, username := range names {
		e.watchCall(c, site, "unwatch", username, csrf)
	}
}

// DownloadURLs puts the /download original first (404 falls through to the
// always-available fullview render).
func (e *Engine) DownloadURLs(post *core.PostInfo) []string {
	var urls []string
	if post.Extra != nil {
		if list, ok := post.Extra["dl"].([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok && s != "" {
					urls = append(urls, s)
				}
			}
		}
	}
	if post.File != nil && post.File.URL != "" {
		dup := false
		for _, u := range urls {
			if u == post.File.URL {
				dup = true
				break
			}
		}
		if !dup {
			urls = append(urls, post.File.URL)
		}
	}
	return urls
}

// VerifiesHash: DA exposes no content hash.
func (e *Engine) VerifiesHash(*core.PostInfo) bool { return false }

// ArtistQuery builds by:<name>.
func (e *Engine) ArtistQuery(artist string, _ *core.PostInfo) string {
	a := strings.TrimLeft(strings.TrimSpace(artist), "@")
	if a == "" {
		return ""
	}
	return "by:" + a
}

// Permalink prefers the deviation's real /art/... URL stashed on Extra.
func (e *Engine) Permalink(site *core.Site, post *core.PostInfo) string {
	if post.Extra != nil {
		if u, ok := post.Extra["url"].(string); ok && u != "" {
			return u
		}
	}
	return site.PostURL(post.ID)
}

// DeriveArtists: the username was captured at parse time.
func (e *Engine) DeriveArtists(post *core.PostInfo, _ []string) []string {
	return append([]string(nil), post.Artists...)
}

// IdFromURL parses the digits at the END of the /art/ slug (a title may carry
// its own dash-digit runs) or /deviation/<id>.
func (e *Engine) IdFromURL(_ *core.Site, urlStr string) core.PostId {
	m := artIDRe.FindStringSubmatch(urlStr)
	if m == nil {
		m = deviaRe.FindStringSubmatch(urlStr)
	}
	if m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return core.I(n)
	}
	return core.I(0)
}

// PostByID fetches one work via its id-only redirect URL; the art page's state
// also carries the tags that list pages lack.
func (e *Engine) PostByID(c *core.ApiClient, site *core.Site,
	id core.PostId) (*core.PostInfo, error) {

	if !id.IsNu || id.Num <= 0 {
		return nil, nil
	}
	_, state, err := e.fetchPage(c, site,
		fmt.Sprintf("%s/deviation/%d", site.Base(), id.Num))
	if err != nil {
		return nil, nil
	}
	ents := entityMap(state, "entities")
	if ents == nil {
		return nil, nil
	}
	devs, _ := ents["deviation"].(map[string]any)
	d, _ := devs[id.String()].(map[string]any)
	if d == nil {
		return nil, nil
	}
	users, _ := ents["user"].(map[string]any)
	p := e.build(site, d, users)
	if p == nil {
		return nil, nil
	}
	dxRaw, _ := ents["deviationExtended"].(map[string]any)
	dx, _ := dxRaw[id.String()].(map[string]any)
	if dx != nil {
		tags, _ := dx["tags"].([]any)
		var names []string
		for _, t := range tags {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if name := core.Str_(tm["name"]); name != "" {
				names = append(names, name)
			}
		}
		p.Tags = names
	}
	return p, nil
}

// Register installs the engine and the DeviantArt site.
func Register() {
	core.RegisterEngine(NewEngine())
	core.RegisterSite(&core.Site{
		Key:          "deviantart",
		Label:        "DeviantArt",
		Domain:       "www.deviantart.com",
		Engine:       "deviantart",
		PageLimit:    24,   // one SSR page = 24 (gallery: 10) results
		MinInterval:  1.0,  // HTML pages are ~1 MB; stay polite
		OptionalAuth: true, // Cookie unlocks originals + account access
		AuthFie: []core.CredField{
			{Key: "cookie", Label: "Cookie (строка из DevTools)"},
		},
		AuthURL: "https://www.deviantart.com/  (войдите → DevTools → Network → " +
			"любой запрос к deviantart.com → Request Headers → скопируйте " +
			"значение заголовка Cookie целиком)",
		PostURLTm: "https://www.deviantart.com/deviation/{id}",
	})
}
