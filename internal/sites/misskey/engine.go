// Package misskey — reusable Misskey engine using only public API and media
// URLs. Public note endpoints return packed files inline; each file already
// has a public CDN or /files/<access-key> URL, thumbnail and stored-file MD5.
// Public media can be re-encoded, so its format and bytes can differ from
// the stored file described by the API.
// Downloading these public files needs neither credentials nor a Drive API lookup.
// Visibility is enforced by Misskey before packing the note; hidden notes have
// no files and are skipped.
package misskey

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"massdownloader/internal/core"
)

var (
	noteIDRe = regexp.MustCompile(`^[0-9a-zA-Z]+$`)
	noteURL  = regexp.MustCompile(`/notes/([0-9a-zA-Z]+)`)
	scoreRe  = regexp.MustCompile(`^score:(>=|>|=)?(-?\d+)$`)
)

var mimeExt = map[string]string{
	"image/jpeg": "jpg", "image/png": "png", "image/gif": "gif",
	"image/webp": "webp", "image/avif": "avif",
	"video/mp4": "mp4", "video/webm": "webm",
}

// Filters mirrors app/sites/misskey/filters.py.
func Filters() core.FilterList {
	return core.FilterList{
		// Resolve display names to opaque Misskey user IDs via autocomplete;
		// tokens are emitted as user_id:<v>.
		{Kind: core.KindAuthors, Key: "authors",
			Label:       "Авторы (пользователи Misskey)",
			Placeholder: "@имя автора…", Autocomplete: "artists",
			Tooltip: "Начните вводить username и выберите пользователя.\n" +
				"В запрос сохраняется точный Misskey user ID; несколько\n" +
				"авторов объединяются."},
		{Kind: core.KindTags, Key: "include",
			Label:        "Текст / #хэштеги",
			Placeholder:  "слово или #хэштег… (пусто — локальная лента)",
			Autocomplete: "tags",
			Tooltip: "Без автора выполняется полнотекстовый notes/search.\n" +
				"С автором слова дополнительно фильтруют его заметки."},
		{Kind: core.KindExclude, Key: "exclude",
			Label:       "Исключить слова / хэштеги",
			Placeholder: "исключить тег…", Autocomplete: "tags"},
		core.NewText("ids", "Заметки по ID", "ID или ссылка на /notes/…", "id"),
		core.NewThreshold("score", "Реакции (мин.)", "score", 10),
		{Kind: core.KindChoice, Key: "rating", Label: "Рейтинг", Token: "rating",
			Options: []core.ChoiceOption{
				{Label: "Любые", Value: ""}, {Label: "Без Sensitive", Value: "s"},
				{Label: "Только Sensitive", Value: "e"},
			},
			Tooltip: "Фильтр по isSensitive вложенных файлов."},
		core.ImagesOnlyFilter("Только изображения (без видео)",
			"Не скачивать видео и анимацию.\nСнимите галочку, чтобы качать и ролики/GIF."),
	}
}

// misskeyAuthorsTokens renders the AuthorsFilter value as user_id: tokens
// (mirrors MisskeyAuthorsFilter.tokens).
func misskeyAuthorsTokens(value any) []string {
	var out []string
	for _, v := range toStrings(value) {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, "user_id:"+s)
		}
	}
	return out
}

func toStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Engine ports MisskeyEngine.
type Engine struct{ core.EngineBase }

func (e *Engine) Key() string              { return "misskey" }
func (e *Engine) SupportsAlbums() bool     { return true }
func (e *Engine) UsesCursor() bool         { return true }
func (e *Engine) Filters() core.FilterList { return Filters() }

// TokensOverride lets the panel ask this engine for its custom authors token
// rendering (the frontend checks Kind==authors && site engine == misskey and
// uses this shape).
const TokensOverride = "user_id"

func api(c *core.ApiClient, site *core.Site, endpoint string,
	body map[string]any) (any, error) {

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.PostJSON(nil, fmt.Sprintf("%s/api/%s", site.Base(), endpoint), nil,
		strings.NewReader(string(raw)), "application/json", nil)
}

func extractNoteID(raw string) string {
	value := strings.TrimSpace(raw)
	if m := noteURL.FindStringSubmatch(value); m != nil {
		return m[1]
	}
	if noteIDRe.MatchString(value) {
		return value
	}
	return ""
}

type parsedQuery struct {
	words   []string
	exclude []string
	users   []string
	ids     []string
	score   *struct {
		op     string
		wanted int64
	}
	rating string
}

func parseQuery(query string) parsedQuery {
	var p parsedQuery
	for _, tok := range strings.Fields(query) {
		switch {
		case strings.HasPrefix(tok, "user_id:"):
			if uid := strings.TrimSpace(tok[len("user_id:"):]); uid != "" {
				p.users = append(p.users, uid)
			}
		case strings.HasPrefix(tok, "id:"):
			if id := extractNoteID(tok[len("id:"):]); id != "" {
				p.ids = append(p.ids, id)
			}
		case strings.HasPrefix(tok, "rating:"):
			p.rating = tok[len("rating:"):]
		case scoreRe.MatchString(tok):
			m := scoreRe.FindStringSubmatch(tok)
			op := "="
			if m[1] != "" {
				op = m[1]
			}
			var wanted int64
			fmt.Sscanf(m[2], "%d", &wanted)
			p.score = &struct {
				op     string
				wanted int64
			}{op, wanted}
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			p.exclude = append(p.exclude,
				strings.ToLower(tok[1:]))
		default:
			p.words = append(p.words, tok)
		}
	}
	return p
}

func passesNumber(value int64, threshold *struct {
	op     string
	wanted int64
}) bool {
	if threshold == nil {
		return true
	}
	switch threshold.op {
	case ">=":
		return value >= threshold.wanted
	case ">":
		return value > threshold.wanted
	}
	return value == threshold.wanted
}

func usableFile(f map[string]any) bool {
	if f == nil || core.Str_(f["url"]) == "" {
		return false
	}
	mime := strings.ToLower(core.Str_(f["type"]))
	return strings.HasPrefix(mime, "image/") ||
		strings.HasPrefix(mime, "video/")
}

func extension(file map[string]any) string {
	// Misskey's public URL may point to a WebP derivative while type/name
	// still describe the stored JPEG/PNG. Prefer a recognized media suffix.
	if u, err := url.Parse(core.Str_(file["url"])); err == nil {
		suffix := strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), ".")
		for _, ext := range mimeExt {
			if suffix == ext || suffix == "jpeg" && ext == "jpg" {
				return ext
			}
		}
	}
	mime := strings.ToLower(core.Str_(file["type"]))
	if ext, ok := mimeExt[mime]; ok {
		return ext
	}
	name := core.Str_(file["name"])
	suffix := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	if suffix != "" {
		return suffix
	}
	u, err := url.Parse(core.Str_(file["url"]))
	if err != nil {
		return "bin"
	}
	if suffix = strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), "."); suffix != "" {
		return suffix
	}
	return "bin"
}

type mediaInfo struct {
	URL      string `json:"url"`
	Preview  string `json:"preview"`
	MD5      string `json:"md5"`
	Ext      string `json:"ext"`
	Size     int64  `json:"size"`
	Width    int64  `json:"width"`
	Height   int64  `json:"height"`
	Sensitiv bool   `json:"sensitive"`
}

func mediaOf(file map[string]any) mediaInfo {
	props, _ := file["properties"].(map[string]any)
	u := core.Str_(file["url"])
	preview := core.Str_(file["thumbnailUrl"])
	if preview == "" {
		preview = u
	}
	md5 := strings.ToLower(core.Str_(file["md5"]))
	width, height := int64(0), int64(0)
	if props != nil {
		width = core.AsInt(props["width"])
		height = core.AsInt(props["height"])
	}
	sensitiv, _ := file["isSensitive"].(bool)
	return mediaInfo{URL: u, Preview: preview, MD5: md5, Ext: extension(file),
		Size: core.AsInt(file["size"]), Width: width, Height: height,
		Sensitiv: sensitiv}
}

// ParseNote converts one note payload into PostInfo (nil when unusable).
func ParseNote(site *core.Site, note map[string]any) *core.PostInfo {
	noteID := core.Str_(note["id"])
	if noteID == "" {
		return nil
	}
	if hidden, _ := note["isHidden"].(bool); hidden {
		return nil
	}
	var media []mediaInfo
	if list, ok := note["files"].([]any); ok {
		for _, item := range list {
			f, ok := item.(map[string]any)
			if !ok || !usableFile(f) {
				continue
			}
			media = append(media, mediaOf(f))
		}
	}
	if len(media) == 0 {
		return nil
	}
	user, _ := note["user"].(map[string]any)
	username := ""
	host := ""
	if user != nil {
		username = core.Str_(user["username"])
		host = core.Str_(user["host"])
	}
	handle := username
	if username != "" && host != "" {
		handle = username + "@" + host
	}
	first := media[0]
	rating := "s"
	for _, m := range media {
		if m.Sensitiv {
			rating = "e"
			break
		}
	}
	var tags []string
	if list, ok := note["tags"].([]any); ok {
		for _, t := range list {
			tags = append(tags, core.Str_(t))
		}
	}
	authorID := core.Str_(note["userId"])
	if authorID == "" && user != nil {
		authorID = core.Str_(user["id"])
	}
	return &core.PostInfo{
		ID:   core.S(noteID),
		Site: site.Key,
		File: &core.PostFile{URL: first.URL, MD5: first.MD5, Ext: first.Ext,
			Size: first.Size, Width: int(first.Width), Height: int(first.Height)},
		PreviewURL: first.Preview,
		SampleURL:  first.URL,
		Score:      core.AsInt(note["reactionCount"]),
		Rating:     rating,
		Artists:    orEmpty(handle),
		Tags:       tags,
		AuthorID:   authorID,
		CreatedAt:  core.ParseTime(note["createdAt"]),
		PageCount:  len(media),
		Extra:      map[string]any{"media": mustAny(media)},
	}
}

func orEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func mustAny(media []mediaInfo) any {
	b, _ := json.Marshal(media)
	var out any
	json.Unmarshal(b, &out)
	return out
}

func mediaList(extra map[string]any) []mediaInfo {
	if extra == nil {
		return nil
	}
	b, err := json.Marshal(extra["media"])
	if err != nil {
		return nil
	}
	var out []mediaInfo
	json.Unmarshal(b, &out)
	return out
}

func fetchOne(c *core.ApiClient, site *core.Site, noteID string) map[string]any {
	v, err := api(c, site, "notes/show", map[string]any{"noteId": noteID})
	if err != nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

func passesNote(note map[string]any, p parsedQuery) bool {
	var files []map[string]any
	if list, ok := note["files"].([]any); ok {
		for _, item := range list {
			if f, ok := item.(map[string]any); ok && usableFile(f) {
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 {
		return false
	}
	if hidden, _ := note["isHidden"].(bool); hidden {
		return false
	}
	sensitive := false
	for _, f := range files {
		if v, _ := f["isSensitive"].(bool); v {
			sensitive = true
			break
		}
	}
	if p.rating == "s" && sensitive {
		return false
	}
	if p.rating == "e" && !sensitive {
		return false
	}
	if !passesNumber(core.AsInt(note["reactionCount"]), p.score) {
		return false
	}
	var sb strings.Builder
	sb.WriteString(core.Str_(note["text"]))
	sb.WriteString(" ")
	sb.WriteString(core.Str_(note["cw"]))
	sb.WriteString(" ")
	if tagList, ok := note["tags"].([]any); ok {
		for _, t := range tagList {
			sb.WriteString(core.Str_(t))
			sb.WriteString(" ")
		}
	}
	haystack := strings.ToLower(sb.String())
	for _, word := range p.exclude {
		if strings.Contains(haystack, word) {
			return false
		}
	}
	// notes/search applies the positive query server-side. Author and ID
	// endpoints do not, so keep their additional words as an AND filter.
	if len(p.users) > 0 || len(p.ids) > 0 {
		for _, word := range p.words {
			w := strings.ToLower(strings.TrimLeft(word, "#"))
			if w != "" && !strings.Contains(haystack, w) {
				return false
			}
		}
	}
	return true
}

// fetchSource walks through empty/non-media pages so client filtering does not
// prematurely tell the core pagination is exhausted.
func fetchSource(c *core.ApiClient, site *core.Site, endpoint string,
	body map[string]any, p parsedQuery,
	limit int) ([]*core.PostInfo, string) {

	cursor, _ := body["untilId"].(string)
	for i := 0; i < 5; i++ {
		payload := map[string]any{}
		for k, v := range body {
			payload[k] = v
		}
		if cursor != "" {
			payload["untilId"] = cursor
		}
		delete(payload, "untilId")
		if cursor != "" {
			payload["untilId"] = cursor
		}
		raw, err := api(c, site, endpoint, payload)
		if err != nil {
			return nil, ""
		}
		list, _ := raw.([]any)
		if len(list) == 0 {
			return nil, ""
		}
		last, _ := list[len(list)-1].(map[string]any)
		if last == nil {
			return nil, ""
		}
		cursor = core.Str_(last["id"])
		var visible []*core.PostInfo
		for _, item := range list {
			n, ok := item.(map[string]any)
			if !ok || !passesNote(n, p) {
				continue
			}
			if post := ParseNote(site, n); post != nil {
				visible = append(visible, post)
			}
		}
		if len(visible) > 0 {
			for _, post := range visible {
				if post.Extra == nil {
					post.Extra = map[string]any{}
				}
				post.Extra["_next"] = cursor
			}
			if len(visible) > limit {
				visible = visible[:limit]
			}
			return visible, cursor
		}
	}
	return nil, ""
}

// FetchPosts runs one page of the requested source (IDs/users/search/timeline).
func (e *Engine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {

	p := parseQuery(tags)
	pageStr := pageString(page)
	if len(p.ids) > 0 {
		if pageStr != "1" && pageStr != "" {
			return nil, nil
		}
		var out []*core.PostInfo
		for _, id := range p.ids {
			if n := fetchOne(c, site, id); n != nil && passesNote(n, p) {
				if post := ParseNote(site, n); post != nil {
					out = append(out, post)
				}
			}
		}
		return out, nil
	}

	cursor := ""
	if pageStr != "1" && pageStr != "" {
		cursor = pageStr
	}
	if len(p.users) > 0 {
		const marker = "misskey-users:"
		cursorMap := map[string]string{}
		if strings.HasPrefix(cursor, marker) {
			var value map[string]any
			if err := json.Unmarshal([]byte(cursor[len(marker):]), &value); err == nil &&
				value != nil {
				for k, v := range value {
					cursorMap[k] = core.Str_(v)
				}
			}
		}
		var posts []*core.PostInfo
		seen := map[string]bool{}
		var users []string
		for _, uid := range p.users {
			if !seen[uid] {
				seen[uid] = true
				users = append(users, uid)
			}
		}
		perUser := limit / len(users)
		if perUser < 1 {
			perUser = 1
		}
		nextMap := map[string]string{}
		for _, uid := range users {
			// An explicitly empty saved cursor means this author's feed was
			// exhausted on the previous page.
			if v, inMap := cursorMap[uid]; inMap && v == "" {
				nextMap[uid] = ""
				continue
			}
			body := map[string]any{
				"userId": uid, "withFiles": true,
				"withRenotes": false, "limit": perUser,
			}
			userCursor := ""
			if len(cursorMap) == 0 {
				userCursor = cursor
			} else if cached, ok := cursorMap[uid]; ok {
				userCursor = cached
			}
			if userCursor != "" {
				body["untilId"] = userCursor
			}
			batch, nextValue := fetchSource(c, site, "users/notes", body,
				p, perUser)
			nextMap[uid] = nextValue
			posts = append(posts, batch...)
		}
		sort.SliceStable(posts, func(i, j int) bool {
			return posts[i].ID.Less(posts[j].ID)
		})
		// newest-first like Python's reverse sort by id
		reverseSlice(posts)
		rawNext, _ := json.Marshal(nextMap)
		nextCursor := marker + string(rawNext)
		for _, post := range posts {
			if post.Extra == nil {
				post.Extra = map[string]any{}
			}
			post.Extra["_next"] = nextCursor
		}
		return posts, nil
	}

	var endpoint string
	var body map[string]any
	if len(p.words) > 0 {
		endpoint = "notes/search"
		body = map[string]any{"query": strings.Join(p.words, " "), "limit": limit}
	} else {
		endpoint = "notes/local-timeline"
		body = map[string]any{"withFiles": true, "withRenotes": false,
			"limit": limit}
	}
	if cursor != "" {
		body["untilId"] = cursor
	}
	posts, _ := fetchSource(c, site, endpoint, body, p, limit)
	return posts, nil
}

func reverseSlice[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func pageString(page any) string {
	switch t := page.(type) {
	case int:
		return fmt.Sprint(t)
	case int64:
		return fmt.Sprint(t)
	case string:
		return t
	}
	return fmt.Sprint(page)
}

// NextCursor reads the stashed token, falling back to the boundary id.
func (e *Engine) NextCursor(_ string, batch []*core.PostInfo, cursor any) any {
	if len(batch) == 0 {
		return cursor
	}
	last := batch[len(batch)-1]
	if last.Extra != nil {
		if nxt, ok := last.Extra["_next"].(string); ok && nxt != "" {
			return nxt
		}
	}
	return last.ID.String()
}

// Autocomplete suggests hashtags from hashtags/search.
func (e *Engine) Autocomplete(c *core.ApiClient, site *core.Site,
	query string) ([]core.TagSuggestion, error) {

	q := strings.TrimLeft(strings.TrimSpace(query), "#")
	if q == "" {
		return nil, nil
	}
	data, err := api(c, site, "hashtags/search",
		map[string]any{"query": q, "limit": 20, "offset": 0})
	if err != nil {
		return nil, err
	}
	list, _ := data.([]any)
	out := make([]core.TagSuggestion, 0, len(list))
	for _, tag := range list {
		s := core.Str_(tag)
		if s != "" {
			out = append(out, core.TagSuggestion{Name: "#" + s})
		}
	}
	return out, nil
}

// SearchArtists resolves usernames via users/search; each suggestion commits
// the opaque user ID as its search value.
func (e *Engine) SearchArtists(c *core.ApiClient, site *core.Site, query string,
	limit int) ([]core.TagSuggestion, error) {

	q := strings.TrimLeft(strings.TrimSpace(query), "@")
	if q == "" {
		return nil, nil
	}
	data, err := api(c, site, "users/search",
		map[string]any{"query": q, "limit": limit, "origin": "combined",
			"detail": false})
	if err != nil {
		return nil, err
	}
	list, _ := data.([]any)
	out := make([]core.TagSuggestion, 0, len(list))
	for _, item := range list {
		user, ok := item.(map[string]any)
		if !ok || core.Str_(user["id"]) == "" {
			continue
		}
		username := core.Str_(user["username"])
		host := core.Str_(user["host"])
		label := "@" + username
		if host != "" {
			label += "@" + host
		}
		out = append(out, core.TagSuggestion{Name: label, Category: 1,
			Value: core.Str_(user["id"])})
	}
	return out, nil
}

// ExpandPost splits a multi-file note into one PostInfo per media entry.
func (e *Engine) ExpandPost(_ *core.ApiClient, post *core.PostInfo) []*core.PostInfo {
	media := mediaList(post.Extra)
	if len(media) <= 1 {
		return []*core.PostInfo{post}
	}
	pages := make([]*core.PostInfo, 0, len(media))
	for index, item := range media {
		rating := "s"
		if item.Sensitiv {
			rating = "e"
		}
		pages = append(pages, &core.PostInfo{
			ID: post.ID, Site: post.Site,
			File: &core.PostFile{URL: item.URL, MD5: item.MD5, Ext: item.Ext,
				Size: item.Size, Width: int(item.Width), Height: int(item.Height)},
			PreviewURL: item.Preview, SampleURL: item.URL,
			Score: post.Score, FavCount: post.FavCount, Rating: rating,
			Artists:  append([]string(nil), post.Artists...),
			Tags:     append([]string(nil), post.Tags...),
			AuthorID: post.AuthorID, CreatedAt: post.CreatedAt,
			PageCount: len(media), PageIndex: index,
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

// VerifiesHash mirrors the Python engine: MD5 identifies the stored file,
// while public media URLs can return optimized/transcoded bytes. Retain the
// hash for deduplication, but do not compare it to the CDN response body.
func (*Engine) VerifiesHash(*core.PostInfo) bool { return false }

// ArtistQuery builds the user_id:<uid> query for «работы автора».
func (e *Engine) ArtistQuery(artist string, post *core.PostInfo) string {
	if post != nil && post.AuthorID != "" {
		return "user_id:" + post.AuthorID
	}
	return ""
}

// Permalink is <base>/notes/<id>.
func (e *Engine) Permalink(site *core.Site, post *core.PostInfo) string {
	return fmt.Sprintf("%s/notes/%s", site.Base(), post.ID.String())
}

var noteURLExtract = noteURL

// IdFromURL parses /notes/<id> permalinks (string ids ride the seam).
func (e *Engine) IdFromURL(_ *core.Site, urlStr string) core.PostId {
	if m := noteURLExtract.FindStringSubmatch(urlStr); m != nil {
		return core.FromIDString(m[1])
	}
	return core.I(0)
}

// PostByID resolves a single note via notes/show.
func (e *Engine) PostByID(c *core.ApiClient, site *core.Site,
	id core.PostId) (*core.PostInfo, error) {

	noteID := extractNoteID(id.String())
	if noteID == "" {
		return nil, nil
	}
	note := fetchOne(c, site, noteID)
	if note == nil {
		return nil, nil
	}
	return ParseNote(site, note), nil
}

// DeriveArtists: the author handle was resolved at parse time.
func (e *Engine) DeriveArtists(post *core.PostInfo, _ []string) []string {
	return append([]string(nil), post.Artists...)
}

// Register installs the engine and the misskey.io site.
func Register() {
	core.RegisterEngine(&Engine{})
	core.RegisterSite(&core.Site{
		Key:         "misskey",
		Label:       "Misskey.io",
		Domain:      "misskey.io",
		Engine:      "misskey",
		PageLimit:   100,
		MinInterval: 0.35,
		PostURLTm:   "https://misskey.io/notes/{id}",
	})
}
