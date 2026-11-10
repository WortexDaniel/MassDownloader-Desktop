// Package twitter — X (Twitter) engine: search the site's images/art via its
// private web GraphQL API.
//
// Auth is the browser session — the user pastes auth_token + ct0; ct0 doubles
// as the x-csrf-token header. The public web Bearer token is a fixed,
// well-known string.
//
// Search is POST /i/api/graphql/<qid>/SearchTimeline carrying a JSON body of
// variables (rawQuery/product/cursor) and a features flag blob. Pagination is
// a keyset cursor pulled from the cursor-bottom entry and threaded through the
// core's cursor seam via the last post's extra (same trick as Sankaku).
//
// The booru-style query translates back to X operators: authors -> from:,
// likes -> min_faves:, excludes -> -word; filter:media -filter:retweets is
// always appended. Rating applies client-side off possibly_sensitive.
package twitter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"massdownloader/internal/core"
)

// The fixed public web bearer token (the one x.com ships to browsers).
const bearer = "AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs=" +
	"1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA"

const gqlBase = "https://x.com/i/api/graphql"

// searchQID is the SearchTimeline operation id baked in as the default; a
// stale one self-heals at runtime (refreshQID).
const searchQID = "yIphfmxUO-hddQHKIOk9tA"

var (
	qidRe    = regexp.MustCompile(`queryId:"([^"]+)",operationName:"SearchTimeline"`)
	bundleRe = regexp.MustCompile(
		`https://abs\.twimg\.com/responsive-web/client-web[^"']+?\.js`)
	bareHexRe = regexp.MustCompile(`[0-9a-f]{8,}`)
	longHexRe = regexp.MustCompile(`[0-9a-f]{16,}`)
	digitsRe  = regexp.MustCompile(`\d+`)
	statusRe  = regexp.MustCompile(`/status/(\d+)`)
)

func fieldTokenRe(name string) *regexp.Regexp {
	return regexp.MustCompile(name + `=([0-9a-f]+)`)
}

// features is the flag blob SearchTimeline expects; a missing flag 400s, so
// this is the union the web client sends.
var features = map[string]any{
	"rweb_video_screen_enabled": false,
	"payments_enabled":          false,
	"rweb_xchat_enabled":        false,
	"profile_label_improvements_pcf_label_in_post_enabled":                    true,
	"rweb_tipjar_consumption_enabled":                                         true,
	"verified_phone_label_enabled":                                            false,
	"creator_subscriptions_tweet_preview_api_enabled":                         true,
	"responsive_web_graphql_timeline_navigation_enabled":                      true,
	"responsive_web_graphql_skip_user_profile_image_extensions_enabled":       false,
	"premium_content_api_read_enabled":                                        false,
	"communities_web_enable_tweet_community_results_fetch":                    true,
	"c9s_tweet_anatomy_moderator_badge_enabled":                               true,
	"responsive_web_grok_analyze_button_fetch_trends_enabled":                 false,
	"responsive_web_grok_analyze_post_followups_enabled":                      true,
	"responsive_web_jetfuel_frame":                                            true,
	"responsive_web_grok_share_attachment_enabled":                            true,
	"articles_preview_enabled":                                                true,
	"responsive_web_edit_tweet_api_enabled":                                   true,
	"graphql_is_translatable_rweb_tweet_is_translatable_enabled":              true,
	"view_counts_everywhere_api_enabled":                                      true,
	"longform_notetweets_consumption_enabled":                                 true,
	"responsive_web_twitter_article_tweet_consumption_enabled":                true,
	"tweet_awards_web_tipping_enabled":                                        false,
	"responsive_web_grok_show_grok_translated_post":                           false,
	"responsive_web_grok_analysis_button_from_backend":                        true,
	"creator_subscriptions_quote_tweet_preview_enabled":                       false,
	"freedom_of_speech_not_reach_fetch_enabled":                               true,
	"standardized_nudges_misinfo":                                             true,
	"tweet_with_visibility_results_prefer_gql_limited_actions_policy_enabled": true,
	"longform_notetweets_rich_text_read_enabled":                              true,
	"longform_notetweets_inline_media_enabled":                                true,
	"responsive_web_grok_image_annotation_enabled":                            true,
	"responsive_web_grok_imagine_annotation_enabled":                          true,
	"responsive_web_grok_community_note_auto_translation_is_enabled":          false,
	"responsive_web_enhance_cards_enabled":                                    false,
	"hidden_profile_subscriptions_enabled":                                    true,
	"subscriptions_feature_can_gift_premium":                                  true,
	"highlights_tweets_tab_ui_enabled":                                        true,
	"responsive_web_twitter_article_notes_tab_enabled":                        true,
}

var products = map[string]string{
	"media": "Media", "top": "Top", "latest": "Latest",
}

// Filters mirrors app/sites/twitter/filters.py.
func Filters() core.FilterList {
	return core.FilterList{
		// «Авторы» for X: each handle becomes a from:<handle> operator;
		// several are OR-ed into (from:a OR from:b). No autocomplete.
		{Kind: core.KindAuthors, Key: "artists", Label: "Авторы (from:)",
			Placeholder: "@аккаунт… (пусто — все)", Autocomplete: "artists",
			Tooltip: "Ограничить поиск авторами X (оператор from:).\n" +
				"Вводите @аккаунт (или без @). Несколько — объединение\n" +
				"(from:a OR from:b). Пусто — искать у всех."},
		{Kind: core.KindTags, Key: "include",
			Label:        "Ключевые слова / #хэштеги",
			Placeholder:  "слово, #тег… (пусто — по авторам)",
			Autocomplete: "tags"},
		{Kind: core.KindExclude, Key: "exclude", Label: "Исключить слова",
			Placeholder: "исключить тег…", Autocomplete: "tags"},
		{Kind: core.KindThreshold, Key: "score", Label: "Лайки (мин.)",
			Token: "min_faves", DefaultValue: 100,
			Minimum: -1000, Maximum: 1_000_000,
			Ops: "≥", OpTokens: ">=",
			Tooltip: "оператор min_faves: показывать твиты\n" +
				"с числом лайков не меньше заданного"},
		{Kind: core.KindChoice, Key: "rating", Label: "Рейтинг", Token: "rating",
			Options: []core.ChoiceOption{
				{Label: "Любой", Value: ""}, {Label: "Без 18+", Value: "s"},
				{Label: "Только 18+", Value: "e"},
			},
			Tooltip: "фильтр по метке «деликатный контент»\n" +
				"(применяется на стороне приложения)"},
		{Kind: core.KindChoice, Key: "product", Label: "Раздел",
			Token: "product",
			Options: []core.ChoiceOption{
				{Label: "Новое", Value: "latest"},
				{Label: "Популярное", Value: "top"},
				{Label: "Медиа-сетка", Value: "media"},
			},
			Tooltip: "вкладка поиска X (GraphQL product)"},
		core.ImagesOnlyFilter("Только изображения (без видео/GIF)",
			"Не показывать видео и GIF (X отдаёт их как mp4).\n"+
				"Снимите галочку, чтобы искать и ролики/анимацию."),
	}
}

// Engine ports TwitterEngine.
type Engine struct {
	core.EngineBase
	mu  sync.Mutex
	qid string // live query id; refreshed in-place on a 404
}

func (e *Engine) Key() string              { return "twitter" }
func (e *Engine) SupportsAlbums() bool     { return true }
func (e *Engine) UsesCursor() bool         { return true } // GraphQL cursor via Extra
func (e *Engine) Filters() core.FilterList { return Filters() }

// -- request headers (Bearer always; CSRF + cookies when logged in) ----------

func (e *Engine) headers(c *core.ApiClient, site *core.Site) map[string]string {
	authToken, ct0 := e.credentials(c, site)
	h := map[string]string{
		"Authorization":             "Bearer " + bearer,
		"User-Agent":                core.BrowserUA,
		"Accept":                    "application/json",
		"Referer":                   site.Base() + "/",
		"Origin":                    site.Base(),
		"x-twitter-active-user":     "yes",
		"x-twitter-client-language": "en",
	}
	if ct0 != "" {
		h["x-csrf-token"] = ct0
		h["x-twitter-auth-type"] = "OAuth2Session"
	}
	if authToken != "" || ct0 != "" {
		var parts []string
		if authToken != "" {
			parts = append(parts, "auth_token="+authToken)
		}
		if ct0 != "" {
			parts = append(parts, "ct0="+ct0)
		}
		h["Cookie"] = strings.Join(parts, "; ")
	}
	return h
}

// credentials reads (auth_token, ct0) from the two dedicated fields with a
// fallback for the old single cookie blob so older saved installs keep working.
func (e *Engine) credentials(c *core.ApiClient, site *core.Site) (string, string) {
	authToken := fieldToken(c.Cred(site.Key, "auth_token"), "auth_token")
	ct0 := fieldToken(c.Cred(site.Key, "ct0"), "ct0")
	if authToken == "" || ct0 == "" { // legacy single "cookie" blob
		at, ct := parseCookie(c.Cred(site.Key, "cookie"))
		if authToken == "" {
			authToken = at
		}
		if ct0 == "" {
			ct0 = ct
		}
	}
	return authToken, ct0
}

// fieldToken extracts one token from a single field that may hold a bare
// value, a name=value pair, or a whole cookie line pasted in by mistake.
func fieldToken(raw, name string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if m := fieldTokenRe(name).FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := bareHexRe.FindString(s); m != "" {
		return m
	}
	return ""
}

// parseCookie pulls auth_token + ct0 from a pasted cookie blob. Accepts the
// whole document.cookie, just the two name=value pairs, or two bare hex
// values separated by ;/whitespace (auth_token first).
func parseCookie(raw string) (string, string) {
	at := fieldTokenRe("auth_token").FindStringSubmatch(raw)
	ct := fieldTokenRe("ct0").FindStringSubmatch(raw)
	authToken, ct0 := "", ""
	if at != nil {
		authToken = at[1]
	}
	if ct != nil {
		ct0 = ct[1]
	}
	if authToken != "" && ct0 != "" {
		return authToken, ct0
	}
	// no usable name= prefixes — fall back to bare hex runs in paste order
	// (auth_token is exactly 40 hex chars on x.com; ct0 is longer)
	var bare []string
	for _, h := range longHexRe.FindAllString(raw, -1) {
		if h != authToken && h != ct0 {
			bare = append(bare, h)
		}
	}
	if authToken == "" && len(bare) > 0 {
		authToken = bare[0]
		for _, h := range bare {
			if len(h) == 40 {
				authToken = h
				break
			}
		}
		var rest []string
		for _, h := range bare {
			if h != authToken {
				rest = append(rest, h)
			}
		}
		bare = rest
	}
	if ct0 == "" && len(bare) > 0 {
		ct0 = bare[0]
	}
	return authToken, ct0
}

// -- query translation --------------------------------------------------------

type parsedXQuery struct {
	froms    []string
	words    []string
	product  string
	rating   string
	minFaves int64
}

// parse splits the panel's booru-style query into X search ingredients.
func parse(tags string) parsedXQuery {
	p := parsedXQuery{product: "Latest"}
	for _, tok := range strings.Fields(tags) {
		low := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(low, "from:"):
			h := strings.TrimLeft(tok[5:], "@")
			if h != "" {
				p.froms = append(p.froms, h)
			}
		case strings.HasPrefix(low, "product:"):
			if v, ok := products[low[8:]]; ok {
				p.product = v
			} else {
				p.product = "Latest"
			}
		case strings.HasPrefix(low, "rating:"):
			p.rating = low[len("rating:"):]
			if len(p.rating) > 1 {
				p.rating = p.rating[:1]
			}
		case strings.HasPrefix(low, "min_faves:"),
			strings.HasPrefix(low, "favcount:"), strings.HasPrefix(low, "score:"):
			if m := digitsRe.FindString(tok); m != "" {
				p.minFaves, _ = strconv.ParseInt(m, 10, 64)
			}
		case low == "filter:images" || low == "-filter:retweets":
			// always re-appended by rawQuery
		default:
			p.words = append(p.words, tok)
		}
	}
	return p
}

// rawQuery builds the X search string; hasContent reports whether the user
// actually asked for something (X requires at least one term).
func rawQuery(p parsedXQuery) (string, bool) {
	parts := append([]string(nil), p.words...)
	if len(p.froms) == 1 {
		parts = append(parts, "from:"+p.froms[0])
	} else if len(p.froms) > 1 {
		var froms []string
		for _, f := range p.froms {
			froms = append(froms, "from:"+f)
		}
		parts = append(parts, "("+strings.Join(froms, " OR ")+")")
	}
	hasContent := len(p.words) > 0 || len(p.froms) > 0
	if p.minFaves > 0 {
		parts = append(parts, fmt.Sprintf("min_faves:%d", p.minFaves))
	}
	// only tweets carrying media (photos or video/GIF); the «только
	// изображения» switch drops videos client-side via the core
	parts = append(parts, "filter:media", "-filter:retweets")
	return strings.TrimSpace(strings.Join(parts, " ")), hasContent
}

// -- post fetching ------------------------------------------------------------

// FetchPosts runs one GraphQL search page.
func (e *Engine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {

	if s, ok := page.(string); ok && s == "" {
		return nil, nil // cursor exhausted
	}
	p := parse(tags)
	raw, hasContent := rawQuery(p)
	if !hasContent {
		return nil, &core.ApiError{
			Msg: "X: укажите ключевые слова или автора (from:)"}
	}
	count := limit
	if count <= 0 || count > 20 {
		count = 20
	}
	variables := map[string]any{
		"rawQuery": raw, "count": count,
		"querySource": "typed_query", "product": p.product,
	}
	if s, ok := page.(string); ok && s != "" && s != "1" {
		variables["cursor"] = s
	}
	data, err := e.search(c, site, variables)
	if err != nil {
		return nil, err
	}
	if m, ok := data.(map[string]any); ok {
		if errs, _ := m["errors"].([]any); len(errs) > 0 && m["data"] == nil {
			msg := ""
			if e0, ok := errs[0].(map[string]any); ok {
				msg = core.Str_(e0["message"])
			}
			if msg == "" {
				msg = "ошибка запроса (нужны свежие cookie?)"
			}
			return nil, &core.ApiError{Msg: "X: " + msg}
		}
	}
	var results []map[string]any
	walkTweets(data, &results)
	out := make([]*core.PostInfo, 0, len(results))
	seen := map[string]bool{}
	for _, r := range results {
		post := e.build(site, r)
		if post == nil || seen[post.ID.Key()] {
			continue
		}
		if p.rating == "s" && post.Rating != "s" {
			continue
		}
		if (p.rating == "e" || p.rating == "q") && post.Rating == "s" {
			continue
		}
		seen[post.ID.Key()] = true
		out = append(out, post)
	}
	if len(out) > 0 {
		if out[len(out)-1].Extra == nil {
			out[len(out)-1].Extra = map[string]any{}
		}
		out[len(out)-1].Extra["_next"] = bottomCursor(data)
	}
	return out, nil
}

// NextCursor reads the stashed GraphQL cursor.
func (e *Engine) NextCursor(_ string, batch []*core.PostInfo, _ any) any {
	if len(batch) == 0 {
		return ""
	}
	if last := batch[len(batch)-1]; last.Extra != nil {
		if nxt, ok := last.Extra["_next"].(string); ok {
			return nxt
		}
	}
	return ""
}

// -- the GraphQL call (POST, self-healing query id) ---------------------------

// search POSTs SearchTimeline; a stale query id 404s with an empty body, so
// on a 404 the live id is re-scraped once and the call retried — the search
// heals itself instead of needing a code update.
func (e *Engine) search(c *core.ApiClient, site *core.Site,
	variables map[string]any) (any, error) {

	data, err := e.postSearch(c, site, variables)
	if err != nil {
		var apiErr *core.ApiError
		if ae, ok := err.(*core.ApiError); ok {
			apiErr = ae
		}
		if apiErr != nil && apiErr.Status == 404 && e.refreshQID(c, site) {
			return e.postSearch(c, site, variables)
		}
		return nil, err
	}
	return data, nil
}

func (e *Engine) currentQID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.qid == "" {
		return searchQID
	}
	return e.qid
}

func (e *Engine) postSearch(c *core.ApiClient, site *core.Site,
	variables map[string]any) (any, error) {

	qid := e.currentQID()
	// variables/features/queryId ride in a JSON body (it's a POST op now)
	body := map[string]any{"variables": variables, "features": features,
		"queryId": qid}
	raw, _ := json.Marshal(body)
	hdr := e.headers(c, site)
	hdr["Content-Type"] = "application/json"
	return c.PostJSON(nil, fmt.Sprintf("%s/%s/SearchTimeline", gqlBase, qid),
		nil, strings.NewReader(string(raw)), "application/json", hdr)
}

// refreshQID re-scrapes X's web client for the current SearchTimeline query
// id: load the homepage, walk its abs.twimg.com bundles (main.* first) and
// grab queryId:"…",operationName:"SearchTimeline". Returns true on success.
func (e *Engine) refreshQID(c *core.ApiClient, site *core.Site) bool {
	htmlText, err := c.GetText(nil, site.Base()+"/", nil,
		map[string]string{"User-Agent": core.BrowserUA, "Accept": "text/html"})
	if err != nil {
		return false
	}
	var urls []string
	seen := map[string]bool{}
	for _, u := range bundleRe.FindAllString(htmlText, -1) {
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	sort.SliceStable(urls, func(i, j int) bool {
		// main.* first
		return strings.Contains(urls[i], "/main.") &&
			!strings.Contains(urls[j], "/main.")
	})
	for _, u := range urls {
		js, err := c.GetText(nil, u, nil,
			map[string]string{"User-Agent": core.BrowserUA})
		if err != nil {
			continue
		}
		if m := qidRe.FindStringSubmatch(js); m != nil {
			e.mu.Lock()
			e.qid = m[1]
			e.mu.Unlock()
			return true
		}
	}
	return false
}

// -- response walking ---------------------------------------------------------

// walkTweets collects every tweet_results.result dict anywhere in the
// response — robust to the differing entry/module shapes of the Latest/Top/
// Media products without hard-coding the instruction path.
func walkTweets(node any, out *[]map[string]any) {
	switch t := node.(type) {
	case map[string]any:
		if tr, ok := t["tweet_results"].(map[string]any); ok {
			if result, ok := tr["result"].(map[string]any); ok {
				*out = append(*out, result)
			}
		}
		for _, v := range t {
			walkTweets(v, out)
		}
	case []any:
		for _, v := range t {
			walkTweets(v, out)
		}
	}
}

// bottomCursor finds the cursor-bottom entry's value anywhere in the payload.
func bottomCursor(node any) string {
	var found string
	findCursor(node, &found)
	return found
}

func findCursor(node any, found *string) {
	switch t := node.(type) {
	case map[string]any:
		if eid, ok := t["entryId"].(string); ok &&
			strings.HasPrefix(eid, "cursor-bottom") {
			if content, ok := t["content"].(map[string]any); ok {
				if val := core.Str_(content["value"]); val != "" {
					*found = val
				}
			}
		}
		for _, v := range t {
			findCursor(v, found)
		}
	case []any:
		for _, v := range t {
			findCursor(v, found)
		}
	}
}

// -- post building ------------------------------------------------------------

type mediaEntry [3]string // [download_url, preview_url, ext]

// build converts one tweet result into PostInfo (nil when no media).
func (e *Engine) build(site *core.Site, result map[string]any) *core.PostInfo {
	// TweetWithVisibilityResults wraps the real tweet under "tweet"
	tweet := result
	if inner, ok := result["tweet"].(map[string]any); ok {
		tweet = inner
	}
	legacy, _ := tweet["legacy"].(map[string]any)
	var media []any
	if legacy != nil {
		if ee, ok := legacy["extended_entities"].(map[string]any); ok {
			media, _ = ee["media"].([]any)
		}
		if len(media) == 0 {
			if ents, ok := legacy["entities"].(map[string]any); ok {
				media, _ = ents["media"].([]any)
			}
		}
	}
	// Photos upgrade to ?name=orig; video/animated_gif download the best mp4
	// with the poster as thumb.
	var entries []mediaEntry
	for _, item := range media {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind := core.Str_(m["type"])
		switch kind {
		case "photo":
			if u := core.Str_(m["media_url_https"]); u != "" {
				entries = append(entries, imageURLs(u))
			}
		case "video", "animated_gif":
			vi, _ := m["video_info"].(map[string]any)
			if vi == nil {
				vi = map[string]any{}
			}
			if mp4 := bestMp4(vi); mp4 != "" {
				poster := core.Str_(m["media_url_https"])
				if poster == "" {
					poster = mp4
				}
				entries = append(entries, mediaEntry{mp4, poster, "mp4"})
			}
		}
	}
	if len(entries) == 0 {
		return nil // tweet carries no downloadable media
	}
	rid := core.AsInt(tweet["rest_id"])
	if rid == 0 && legacy != nil {
		rid = core.AsInt(legacy["id_str"])
	}
	if rid == 0 {
		return nil
	}
	handle := tweetHandle(tweet)
	url0, prev0, ext0 := entries[0][0], entries[0][1], entries[0][2]
	file := &core.PostFile{URL: url0, Ext: ext0}
	rating := "s"
	if legacy != nil {
		if ps, _ := legacy["possibly_sensitive"].(bool); ps {
			rating = "e"
		}
	}
	score := int64(0)
	if legacy != nil {
		score = core.AsInt(legacy["favorite_count"])
	}
	createdAt := int64(0)
	if legacy != nil {
		createdAt = core.ParseTime(legacy["created_at"])
	}
	return &core.PostInfo{
		ID:         core.I(rid),
		Site:       site.Key,
		File:       file,
		PreviewURL: prev0,
		SampleURL:  prev0,
		Score:      score,
		Rating:     rating,
		Artists:    orEmptySlice(handle),
		Tags:       []string{},
		CreatedAt:  createdAt,
		PageCount:  maxInt(1, len(entries)),
		Extra: map[string]any{
			"handle": handle,
			"media":  entriesToAny(entries),
		},
	}
}

func entriesToAny(entries []mediaEntry) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, []string{e[0], e[1], e[2]})
	}
	return out
}

func entriesFromAny(v any) []mediaEntry {
	list, _ := v.([]any)
	out := make([]mediaEntry, 0, len(list))
	for _, item := range list {
		switch t := item.(type) {
		case []any:
			if len(t) >= 3 {
				out = append(out, mediaEntry{core.Str_(t[0]), core.Str_(t[1]),
					core.Str_(t[2])})
			}
		case []string:
			if len(t) >= 3 {
				out = append(out, mediaEntry{t[0], t[1], t[2]})
			}
		}
	}
	return out
}

// bestMp4 picks the highest-bitrate mp4 in a tweet's video_info (an
// animated_gif has one bitrate-less mp4; a video has several — take the
// largest). The m3u8 HLS variant is ignored (not a single file).
func bestMp4(videoInfo map[string]any) string {
	best := ""
	bestBR := int64(-1)
	variants, _ := videoInfo["variants"].([]any)
	for _, item := range variants {
		v, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if core.Str_(v["content_type"]) == "video/mp4" && core.Str_(v["url"]) != "" {
			br := core.AsInt(v["bitrate"])
			if br >= bestBR {
				bestBR = br
				best = core.Str_(v["url"])
			}
		}
	}
	return best
}

// tweetHandle digs screen_name out of the user result (it lived in legacy,
// then moved to result.core — try both).
func tweetHandle(tweet map[string]any) string {
	ur := map[string]any{}
	if cr, ok := tweet["core"].(map[string]any); ok {
		if uresults, ok := cr["user_results"].(map[string]any); ok {
			if r, ok := uresults["result"].(map[string]any); ok {
				ur = r
			}
		}
	}
	if inner, ok := ur["core"].(map[string]any); ok {
		if sn := core.Str_(inner["screen_name"]); sn != "" {
			return sn
		}
	}
	if lg, ok := ur["legacy"].(map[string]any); ok {
		if sn := core.Str_(lg["screen_name"]); sn != "" {
			return sn
		}
	}
	return ""
}

// imageURLs returns (original, small-thumb, ext) for a pbs.twimg.com image.
// X serves sizes via ?format=<ext>&name=<size>; orig is the full upload.
func imageURLs(murl string) mediaEntry {
	base := murl
	ext := "jpg"
	if i := strings.LastIndex(murl, "."); i >= 0 {
		base = murl[:i]
		ext = strings.ToLower(murl[i+1:])
	}
	return mediaEntry{
		fmt.Sprintf("%s?format=%s&name=orig", base, ext),
		fmt.Sprintf("%s?format=%s&name=small", base, ext),
		ext,
	}
}

// ExpandPost yields one downloadable PostInfo per stashed photo; single-image
// tweets return unchanged.
func (e *Engine) ExpandPost(_ *core.ApiClient, post *core.PostInfo) []*core.PostInfo {
	media := entriesFromAny(post.Extra["media"])
	if len(media) <= 1 || post.File == nil {
		return []*core.PostInfo{post}
	}
	pages := make([]*core.PostInfo, 0, len(media))
	for i, entry := range media {
		ext := entry[2]
		if ext == "" {
			ext = "jpg"
		}
		f := &core.PostFile{URL: entry[0], Ext: ext}
		pages = append(pages, &core.PostInfo{
			ID: post.ID, Site: post.Site, File: f,
			PreviewURL: entry[1], SampleURL: entry[1],
			Score: post.Score, Rating: post.Rating,
			Artists:   append([]string(nil), post.Artists...),
			Tags:      []string{},
			CreatedAt: post.CreatedAt,
			PageCount: len(media), PageIndex: i,
			Extra: cloneExtra(post.Extra),
		})
	}
	return pages
}

func (e *Engine) LocalPages(p *core.PostInfo) []*core.PostInfo { return e.ExpandPost(nil, p) }

func cloneExtra(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// VerifiesHash: X exposes no content hash.
func (e *Engine) VerifiesHash(*core.PostInfo) bool { return false }

// DeriveArtists: the handle was resolved at parse time.
func (e *Engine) DeriveArtists(post *core.PostInfo, _ []string) []string {
	return append([]string(nil), post.Artists...)
}

// ArtistQuery builds the from:<handle> operator query.
func (e *Engine) ArtistQuery(artist string, _ *core.PostInfo) string {
	a := strings.TrimLeft(strings.TrimSpace(artist), "@")
	if a == "" {
		return ""
	}
	return "from:" + a
}

// Permalink is https://x.com/<handle>/status/<id>.
func (e *Engine) Permalink(site *core.Site, post *core.PostInfo) string {
	handle := "i/web"
	if post.Extra != nil {
		if h, ok := post.Extra["handle"].(string); ok && h != "" {
			handle = h
		}
	}
	return fmt.Sprintf("https://x.com/%s/status/%s", handle, post.ID.String())
}

// IdFromURL parses /status/<id> permalinks.
func (e *Engine) IdFromURL(_ *core.Site, urlStr string) core.PostId {
	if m := statusRe.FindStringSubmatch(urlStr); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return core.I(n)
	}
	return core.I(0)
}

// PostByID: a reverse-search hit on X opens in the browser; we don't
// reconstruct a full tweet (that needs another GraphQL op).
func (e *Engine) PostByID(*core.ApiClient, *core.Site,
	core.PostId) (*core.PostInfo, error) {

	return nil, nil
}

func orEmptySlice(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Register installs the engine and the x.com site.
func Register() {
	core.RegisterEngine(&Engine{})
	core.RegisterSite(&core.Site{
		Key:          "twitter",
		Label:        "X (Twitter)",
		Domain:       "x.com",
		Engine:       "twitter",
		PageLimit:    20,
		MinInterval:  2.0, // SearchTimeline is rate-limited; stay polite
		RequiresAuth: true,
		AuthFie: []core.CredField{
			{Key: "auth_token", Label: "auth_token"},
			{Key: "ct0", Label: "ct0"},
		},
		AuthURL: "https://x.com/  (войдите → DevTools → Application → Cookies: " +
			"скопируйте значения auth_token и ct0 в отдельные поля)",
	})
}

var _ = http.StatusOK // keep net/http linked for potential direct use
