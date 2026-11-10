// Package moebooru — konachan and friends.
//
// GET /post.json?tags=...&limit=100&page=N — a flat JSON array. Tags are one
// space-separated string per post (no per-category split), the file URL is
// direct, there is no fav count, and author is the uploader (not the artist).
// Plain numeric pagination. Supports -tag, score:>=N, rating:s and ~a ~b (OR).
// GET /tag.json?name=x*&order=count — suggestions; numeric type.
package moebooru

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"massdownloader/internal/core"
)

// Filters mirrors app/sites/moebooru/filters.py (no favourite count).
func Filters() core.FilterList {
	return core.FilterList{
		core.NewAuthors(),
		core.NewTags(),
		core.NewExclude(),
		core.NewThreshold("score", "Score", "score", 100),
		core.NewRating(),
		// moebooru's order metatag; default newest-first
		core.NewSort(),
		core.ImagesOnlyFilter("Только изображения (без webm/swf)",
			"Не скачивать видео и анимацию.\nСнимите галочку, чтобы качать и ролики/GIF."),
	}
}

// Engine ports MoebooruEngine.
type Engine struct{ core.EngineBase }

func (e *Engine) Key() string              { return "moebooru" }
func (e *Engine) UsesCursor() bool         { return false }
func (e *Engine) Filters() core.FilterList { return Filters() }

// FetchPosts runs one numeric page of /post.json.
func (e *Engine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {

	q := url.Values{}
	q.Set("tags", tags)
	q.Set("limit", fmt.Sprint(limit))
	q.Set("page", pageString(page))
	data, err := c.GetJSON(nil, site.Base()+"/post.json", q, nil)
	if err != nil {
		return nil, err
	}
	list, _ := data.([]any)
	out := make([]*core.PostInfo, 0, len(list))
	for _, item := range list {
		if d, ok := item.(map[string]any); ok {
			out = append(out, e.ParsePost(d, site))
		}
	}
	return out, nil
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

// ParsePost converts one flat payload; the uploader (author) is kept as the
// fallback author; a searched artist tag still takes priority.
func (e *Engine) ParsePost(d map[string]any, site *core.Site) *core.PostInfo {
	urlStr := core.Str_(d["file_url"])
	md5 := core.Str_(d["md5"])
	ext := ""
	if i := strings.LastIndex(urlStr, "."); i >= 0 {
		ext = strings.ToLower(urlStr[i+1:])
	}
	var file *core.PostFile
	if urlStr != "" && md5 != "" {
		file = &core.PostFile{URL: urlStr, MD5: md5, Ext: ext,
			Size:   core.AsInt(d["file_size"]),
			Width:  int(core.AsInt(d["width"])),
			Height: int(core.AsInt(d["height"]))}
	}
	sample := core.Str_(d["sample_url"])
	if sample == "" {
		sample = urlStr
	}
	author := strings.TrimSpace(core.Str_(d["author"]))
	var artists []string
	if author != "" {
		artists = []string{author}
	}
	return &core.PostInfo{
		ID:         core.I(core.AsInt(d["id"])),
		Site:       site.Key,
		File:       file,
		PreviewURL: core.Str_(d["preview_url"]),
		SampleURL:  sample,
		Score:      core.AsInt(d["score"]),
		FavCount:   0,
		Rating:     core.StrDflt_(d["rating"], "?"),
		Artists:    artists,
		Tags:       splitWords(core.Str_(d["tags"])),
		CreatedAt:  core.ParseTime(d["created_at"]), // unix epoch
	}
}

func splitWords(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Fields(s)
}

// DeriveArtists: one flat tag string, no categories -> recover from hints,
// falling back to the stashed uploader.
func (e *Engine) DeriveArtists(post *core.PostInfo, hintTags []string) []string {
	return core.HintedOrUploader(post, hintTags)
}

// ArtistQuery: a real artist tag searches directly; an uploader handle via
// user:<name>.
func (e *Engine) ArtistQuery(artist string, post *core.PostInfo) string {
	return core.TagOrUserQuery(artist, post)
}

var showRe = regexp.MustCompile(`/post/(?:show/)?(\d+)`)

// IdFromURL: moebooru permalinks are /post/show/<n> (the /show/ optional).
func (e *Engine) IdFromURL(_ *core.Site, urlStr string) core.PostId {
	if m := showRe.FindStringSubmatch(urlStr); m != nil {
		n := int64(0)
		for _, c := range m[1] {
			n = n*10 + int64(c-'0')
		}
		return core.I(n)
	}
	return core.I(0)
}

// Autocomplete suggests tags from /tag.json (prefix match).
func (e *Engine) Autocomplete(c *core.ApiClient, site *core.Site,
	query string) ([]core.TagSuggestion, error) {

	return e.suggestTags(c, site, query, 2, false, 20)
}

// SearchArtists filters type==1 suggestions from /tag.json substring search.
func (e *Engine) SearchArtists(c *core.ApiClient, site *core.Site, query string,
	limit int) ([]core.TagSuggestion, error) {

	if query == "" {
		return nil, nil
	}
	tags, err := e.suggestTags(c, site, query, 1, true, 80)
	if err != nil {
		return nil, err
	}
	artists := make([]core.TagSuggestion, 0, len(tags))
	for _, t := range tags {
		if t.Category == 1 {
			artists = append(artists, t)
			if len(artists) >= limit {
				break
			}
		}
	}
	return artists, nil
}

func (e *Engine) suggestTags(c *core.ApiClient, site *core.Site, query string,
	minLen int, substring bool, limit int) ([]core.TagSuggestion, error) {

	if len(query) < minLen {
		return nil, nil
	}
	pattern := query + "*"
	if substring {
		pattern = "*" + query + "*"
	}
	q := url.Values{}
	q.Set("name", pattern)
	q.Set("order", "count")
	q.Set("limit", fmt.Sprint(limit))
	data, err := c.GetJSON(nil, site.Base()+"/tag.json", q, nil)
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
		out = append(out, core.TagSuggestion{
			Name:      core.Str_(d["name"]),
			PostCount: core.AsInt(d["count"]),
			Category:  int(core.AsInt(d["type"])),
		})
	}
	return out, nil
}

// Register installs the engine and the konachan site.
func Register() {
	core.RegisterEngine(&Engine{})
	core.RegisterSite(&core.Site{
		Key:         "konachan",
		Label:       "konachan.com",
		Domain:      "konachan.com",
		Engine:      "moebooru",
		PageLimit:   100,
		MinInterval: 0.4, // Moebooru asks for politeness; still > e621's pace
	})
}
