// Package onlyhaven ports app/sites/onlyhaven without changing Kemono's wire contract.
package onlyhaven

import (
	"fmt"
	"massdownloader/internal/core"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type Engine struct {
	core.EngineBase
	mu    sync.Mutex
	names map[[3]string]string
}

func NewEngine() *Engine                 { return &Engine{names: map[[3]string]string{}} }
func (*Engine) Key() string              { return "onlyhaven" }
func (*Engine) SupportsAlbums() bool     { return true }
func (*Engine) UsesCursor() bool         { return false }
func (*Engine) Filters() core.FilterList { return Filters() }
func Filters() core.FilterList {
	duration := core.NewThreshold("min_duration", "Мин. длительность видео, сек.", "min-duration", 60)
	duration.Minimum, duration.Maximum, duration.Ops, duration.OpTokens = 0, 86400, "≥", ">="
	return core.FilterList{
		{Kind: core.KindAuthors, Key: "creator", Label: "Автор (создатель)", Autocomplete: "artists", Placeholder: "имя создателя… (пусто — лента сайта)"},
		core.NewText("q", "Поиск по тексту", "слова в подписи или имени автора…", "q"),
		core.HideTextFilter(),
		{Kind: core.KindChoice, Key: "service", Label: "Сервис", Token: "service", Options: []core.ChoiceOption{{Label: "Все сервисы", Value: ""}, {Label: "OnlyFans", Value: "onlyfans"}, {Label: "Fansly", Value: "fansly"}, {Label: "Patreon", Value: "patreon"}}},
		{Kind: core.KindChoice, Key: "media_type", Label: "Тип содержимого", Token: "media", Options: []core.ChoiceOption{{Label: "Все типы", Value: ""}, {Label: "Фото", Value: "photos"}, {Label: "Видео", Value: "videos"}, {Label: "Аудио", Value: "audio"}, {Label: "Текст", Value: "text"}, {Label: "С медиа", Value: "media"}}},
		{Kind: core.KindChoice, Key: "sort", Label: "Сортировка", Token: "sort", Options: []core.ChoiceOption{{Label: "По умолчанию", Value: ""}, {Label: "Сначала новые", Value: "recent"}, {Label: "Сначала старые", Value: "oldest"}, {Label: "Популярные", Value: "popular"}, {Label: "Больше вложений", Value: "attachments"}, {Label: "Длинные видео", Value: "videos"}, {Label: "Недавно импортированные", Value: "imported"}, {Label: "По релевантности", Value: "relevance"}}},
		duration, core.NewBool("preview_only", "Только превью", "preview-only", false),
		{Kind: core.KindBool, Key: "extract_archives", Label: "Распаковывать архивы", Token: "extract-archives", FalseToken: "no-extract-archives", DefaultBool: true},
		{Kind: core.KindBool, Key: "delete_archives", Label: "Удалять архив после распаковки", Token: "delete-archives", EnabledIf: "extract_archives"},
		{Kind: core.KindBool, Key: "extract_to_artist", Label: "Распаковывать в папку автора", Token: "extract-to-artist", EnabledIf: "extract_archives"},
	}
}

type query struct {
	creators                           [][2]string
	text, service, media, sort         string
	duration                           int64
	preview, extract, delete, toArtist bool
}

func parse(tags string) query {
	q := query{extract: true}
	var words []string
	seen := map[[2]string]bool{}
	for _, raw := range strings.Fields(tags) {
		t := strings.TrimLeft(raw, "~")
		key, val, _ := strings.Cut(t, ":")
		switch key {
		case "creator":
			svc, id, ok := strings.Cut(val, ":")
			pair := [2]string{svc, id}
			if ok && svc != "" && id != "" && !seen[pair] {
				q.creators = append(q.creators, pair)
				seen[pair] = true
			}
		case "q":
			words = append(words, val)
		case "service":
			q.service = val
		case "media":
			q.media = val
		case "sort":
			q.sort = val
		case "min-duration":
			q.duration = max(0, core.AsInt(strings.TrimLeft(val, ">=")))
		case "preview-only":
			q.preview = true
		case "no-extract-archives":
			q.extract = false
		case "extract-archives":
			q.extract = true
		case "delete-archives":
			q.delete = true
		case "extract-to-artist":
			q.toArtist = true
		}
	}
	q.text = strings.Join(words, " ")
	if !q.extract {
		q.delete = false
		q.toArtist = false
	}
	return q
}
func params(q query, offset, limit int) url.Values {
	p := url.Values{"o": {fmt.Sprint(offset)}, "n": {fmt.Sprint(min(50, max(1, limit)))}}
	for k, v := range map[string]string{"q": q.text, "service": q.service, "sort": q.sort, "type": q.media} {
		if v != "" {
			p.Set(k, v)
		}
	}
	if q.media == "videos" && q.duration > 0 {
		p.Set("min_duration", fmt.Sprint(q.duration))
	}
	return p
}
func (e *Engine) FetchPosts(c *core.ApiClient, s *core.Site, tags string, limit int, cursor any) ([]*core.PostInfo, error) {
	r, err := e.FetchPage(c, s, tags, limit, cursor)
	return r.Posts, err
}

func (e *Engine) FetchPage(c *core.ApiClient, site *core.Site, tags string, limit int, page any) (core.PageResult, error) {
	q := parse(tags)
	limit = min(50, max(1, limit))
	offset := (max(1, int(core.AsInt(page))) - 1) * limit
	var rows []map[string]any
	fetch := func(endpoint string, p url.Values, creator *[2]string) error {
		data, err := c.GetJSON(nil, site.Base()+endpoint, p, nil)
		if err != nil {
			return err
		}
		obj, _ := data.(map[string]any)
		list, ok := obj["posts"].([]any)
		if !ok {
			return fmt.Errorf("OnlyHaven: отсутствует список posts")
		}
		for _, item := range list {
			if row, ok := item.(map[string]any); ok {
				if creator != nil {
					if core.Str_(row["service"]) == "" {
						row["service"] = creator[0]
					}
					if core.Str_(row["creatorId"]) == "" {
						row["creatorId"] = creator[1]
					}
					if core.Str_(row["creatorName"]) == "" {
						row["creatorName"] = e.creatorName(c, site, creator[0], creator[1])
					}
				}
				rows = append(rows, row)
			}
		}
		return nil
	}
	if len(q.creators) == 0 {
		if err := fetch("/api/v1/posts", params(q, offset, limit), nil); err != nil {
			return core.PageResult{}, err
		}
	} else {
		for _, pair := range q.creators {
			perCreator := max(1, limit/len(q.creators))
			p := params(q, (max(1, int(core.AsInt(page)))-1)*perCreator, perCreator)
			p.Del("service")
			if err := fetch("/api/v1/"+url.PathEscape(pair[0])+"/user/"+url.PathEscape(pair[1])+"/posts", p, &pair); err != nil {
				return core.PageResult{}, err
			}
		}
	}
	out := []*core.PostInfo{}
	for _, row := range rows {
		if p := build(site, row, q); p != nil {
			out = append(out, p)
		}
	}
	if len(q.creators) > 1 {
		sort.SliceStable(out, func(i, j int) bool {
			if q.sort == "oldest" {
				return out[i].CreatedAt < out[j].CreatedAt
			}
			if q.sort == "popular" {
				return out[i].Score > out[j].Score
			}
			return out[i].CreatedAt > out[j].CreatedAt
		})
	}
	result := core.PageResult{Posts: out, HasMore: len(rows) > 0}
	if result.HasMore {
		result.Next = max(1, int(core.AsInt(page))) + 1
	}
	return result, nil
}
func (e *Engine) creatorName(c *core.ApiClient, site *core.Site, svc, id string) string {
	key := [3]string{site.Key, svc, id}
	e.mu.Lock()
	n, ok := e.names[key]
	e.mu.Unlock()
	if ok {
		return n
	}
	data, err := c.GetJSON(nil, site.Base()+"/api/v1/"+url.PathEscape(svc)+"/user/"+url.PathEscape(id)+"/profile", nil, nil)
	if err == nil {
		if m, ok := data.(map[string]any); ok {
			n = core.Str_(m["displayName"])
			if n == "" {
				n = core.Str_(m["name"])
			}
		}
	}
	e.mu.Lock()
	e.names[key] = n
	e.mu.Unlock()
	return n
}

var mimeExt = map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/gif": "gif", "image/webp": "webp", "image/avif": "avif", "video/mp4": "mp4", "video/webm": "webm", "audio/mpeg": "mp3", "audio/mp4": "m4a", "audio/ogg": "ogg", "application/zip": "zip", "application/x-7z-compressed": "7z", "application/vnd.rar": "rar", "application/x-rar-compressed": "rar"}

func attachment(site *core.Site, a map[string]any, previewOnly bool) map[string]any {
	if a["locked"] == true {
		return nil
	}
	sha := strings.ToLower(core.Str_(a["sha256"]))
	if sha == "" {
		return nil
	}
	kind := core.Str_(a["kind"])
	name := core.Str_(a["originalFilename"])
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	if ext == "" {
		ext = mimeExt[core.Str_(a["mimeType"])]
	}
	if ext == "" {
		ext = map[string]string{"image": "jpg", "gif": "gif", "video": "mp4", "audio": "mp3"}[kind]
	}
	if ext == "" {
		ext = "bin"
	}
	preview := ""
	if a["previewThumbhash"] != nil || kind == "image" || kind == "gif" || kind == "video" {
		preview = "https://img." + site.Domain + "/thumbnail/" + sha + "/preview.webp"
	}
	host := site.StaticDomain
	if host == "" {
		host = "e1." + site.Domain
	}
	u := "https://" + host + "/media/" + sha + "/original." + ext
	if previewOnly {
		if preview == "" {
			return nil
		}
		u, ext, sha = preview, "webp", ""
	}
	return map[string]any{"url": u, "preview": preview, "sha256": sha, "ext": ext, "size": core.AsInt(a["bytes"]), "width": core.AsInt(a["width"]), "height": core.AsInt(a["height"]), "name": name, "kind": kind}
}
func mediaFile(m map[string]any) *core.PostFile {
	return &core.PostFile{URL: core.Str_(m["url"]), MD5: core.Str_(m["sha256"]), Ext: core.Str_(m["ext"]), Size: core.AsInt(m["size"]), Width: int(core.AsInt(m["width"])), Height: int(core.AsInt(m["height"]))}
}
func archive(ext string) bool { return ext == "zip" || ext == "rar" || ext == "7z" }
func build(site *core.Site, row map[string]any, q query) *core.PostInfo {
	id := core.Str_(row["id"])
	if id == "" {
		return nil
	}
	media := []any{}
	locked, count := 0, 0
	list, _ := row["attachments"].([]any)
	for _, item := range list {
		a, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if a["locked"] == true {
			locked++
		}
		if m := attachment(site, a, q.preview); m != nil {
			media = append(media, m)
			if archive(core.Str_(m["ext"])) {
				count++
			}
		}
	}
	svc, cid, name := core.Str_(row["service"]), core.Str_(row["creatorId"]), core.Str_(row["creatorName"])
	p := &core.PostInfo{ID: core.FromIDString(id), Site: site.Key, AuthorID: cid, Rating: "?", Score: core.AsInt(row["bookmarked"]), FavCount: core.AsInt(row["bookmarked"]), PageCount: max(1, len(media)), CreatedAt: core.ParseTime(row["published"]), Extra: map[string]any{"service": svc, "creator_id": cid, "raw_id": id, "media": media, "preview_only": q.preview, "title": core.Str_(row["title"]), "content": core.Str_(row["captionHtml"]), "locked_count": locked, "archive_extract": q.extract, "archive_delete": q.delete, "archive_to_artist": q.toArtist, "archive_count": count}}
	p.AttachmentCount = len(list)
	if p.CreatedAt == 0 {
		p.CreatedAt = core.ParseTime(row["added"])
	}
	if name != "" {
		p.Artists = []string{name}
	} else if cid != "" {
		p.Artists = []string{svc + "/" + cid}
	}
	if len(media) > 0 {
		m := media[0].(map[string]any)
		p.File = mediaFile(m)
		p.PreviewURL = core.Str_(m["preview"])
		p.SampleURL = p.File.URL
	}
	return p
}
func (*Engine) ExpandPost(_ *core.ApiClient, p *core.PostInfo) []*core.PostInfo {
	if p == nil {
		return nil
	}
	if p.Extra["expanded_page"] == true {
		return []*core.PostInfo{p}
	}
	list, _ := p.Extra["media"].([]any)
	if len(list) == 0 {
		return []*core.PostInfo{p}
	}
	out := []*core.PostInfo{}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		page := p.Clone()
		page.File = mediaFile(m)
		page.PreviewURL = core.Str_(m["preview"])
		page.SampleURL = page.File.URL
		page.PageIndex = i
		page.PageCount = len(list)
		page.Extra["download_name"] = m["name"]
		page.Extra["archive"] = archive(page.File.Ext)
		page.Extra["expanded_page"] = true
		out = append(out, page)
	}
	return out
}
func (e *Engine) LocalPages(p *core.PostInfo) []*core.PostInfo { return e.ExpandPost(nil, p) }
func (*Engine) VerifiesHash(p *core.PostInfo) bool             { return p.Extra["preview_only"] != true }
func (*Engine) DeriveArtists(p *core.PostInfo, _ []string) []string {
	return append([]string(nil), p.Artists...)
}
func (*Engine) ArtistQuery(_ string, p *core.PostInfo) string {
	if p == nil {
		return ""
	}
	svc, cid := core.Str_(p.Extra["service"]), core.Str_(p.Extra["creator_id"])
	if svc == "" || cid == "" {
		return ""
	}
	q := "creator:" + svc + ":" + cid
	if p.Extra["preview_only"] == true {
		q += " preview-only"
	}
	if p.Extra["archive_extract"] == false {
		return q + " no-extract-archives"
	}
	q += " extract-archives"
	if p.Extra["archive_delete"] == true {
		q += " delete-archives"
	}
	if p.Extra["archive_to_artist"] == true {
		q += " extract-to-artist"
	}
	return q
}
func (*Engine) Permalink(site *core.Site, p *core.PostInfo) string {
	svc, cid, id := core.Str_(p.Extra["service"]), core.Str_(p.Extra["creator_id"]), core.Str_(p.Extra["raw_id"])
	if svc != "" && cid != "" && id != "" {
		return site.Base() + "/creators/" + svc + "/" + cid + "/post/" + id
	}
	return site.Base()
}

var postURL = regexp.MustCompile(`/creators/[^/]+/[^/]+/post/([^/?#]+)`)

func (*Engine) IdFromURL(_ *core.Site, u string) core.PostId {
	if m := postURL.FindStringSubmatch(u); m != nil {
		return core.FromIDString(m[1])
	}
	return core.I(0)
}
func (*Engine) SearchArtists(c *core.ApiClient, site *core.Site, q string, limit int) ([]core.TagSuggestion, error) {
	if len([]rune(strings.TrimSpace(q))) < 2 {
		return nil, nil
	}
	data, err := c.GetJSON(nil, site.Base()+"/api/v1/creators", url.Values{"q": {q}, "o": {"0"}, "n": {fmt.Sprint(min(50, max(1, limit)))}}, nil)
	if err != nil {
		return nil, err
	}
	obj, _ := data.(map[string]any)
	list, _ := obj["creators"].([]any)
	out := []core.TagSuggestion{}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, svc, name := core.Str_(m["id"]), core.Str_(m["service"]), core.Str_(m["displayName"])
		if name == "" {
			name = core.Str_(m["name"])
		}
		if id != "" && svc != "" && name != "" {
			out = append(out, core.TagSuggestion{Name: name + " · " + svc, Value: "creator:" + svc + ":" + id, Category: 1, PostCount: core.AsInt(m["postCount"])})
		}
	}
	return out[:min(max(0, limit), len(out))], nil
}
func Register() {
	core.RegisterEngine(NewEngine())
	core.RegisterSite(&core.Site{Key: "onlyhaven", Label: "cum.st", Domain: "cum.st", StaticDomain: "e1.cum.st", Engine: "onlyhaven", PageLimit: 50, MinInterval: 0.5, Filters: Filters()})
}
