// Package e621 — the Danbooru-derived engine and its two sites.
//
// GET /posts.json?tags=...&limit=320&page=... -> {"posts":[...]}.
// Cursor pagination via page=b<lowest_id> walks past the page-750 wall.
// Posts hidden by the anonymous blacklist have file.url=null; the URL is
// reconstructed from the always-present md5.
// GET /tags/autocomplete.json?search[name_matches]=... — tag suggestions.
// GET /tags.json?search[name_matches]=*x*&search[category]=1 — artists.
package e621

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"massdownloader/internal/core"
)

// Filters mirrors app/sites/e621/filters.py FILTERS.
func Filters() core.FilterList {
	return core.FilterList{
		core.NewAuthors(),
		core.NewTags(),
		core.NewExclude(),
		core.NewThreshold("score", "Score", "score", 100),
		core.NewThreshold("faves", "Избранное (faves)", "favcount", 50),
		core.NewRating(), // rating, default s/q/e options
		// sort by upload time: default is newest-first (e621 adds
		// order:id_desc); «Сначала старые» searches ascending id order
		core.NewSort(),
		core.NewBool("hot", "Горячее", "order:hot", false),
		{Kind: core.KindBool, Key: "popular", Label: "Популярное", Token: "order:popular",
			Tooltip: "Подборка /popular за текущий день сайта, по убыванию score."},
		core.ImagesOnlyFilter("Только изображения (без webm/swf)",
			"Не скачивать видео и анимацию.\nСнимите галочку, чтобы качать и ролики/GIF."),
	}
}

// Engine implements the e621 protocol. Port of E621Engine.
type Engine struct {
	core.EngineBase
}

func (e *Engine) Key() string      { return "e621" }
func (e *Engine) UsesCursor() bool { return true }
func (e *Engine) AuthorScanDescending(query string) bool {
	for _, token := range strings.Fields(query) {
		if strings.HasPrefix(token, "order:") && token != "order:id_desc" {
			return false
		}
		// Mutable thresholds and explicit historical boundaries need reconciliation.
		if strings.Contains(token, ":") && token != "order:id_desc" {
			return false
		}
	}
	return true
}

// AuthorBatchSize: e621 allows 40 tags per search; a batch of 30 authors
// leaves room for id:> and order:id_desc.
func (e *Engine) AuthorBatchSize() int     { return 30 }
func (e *Engine) Filters() core.FilterList { return Filters() }

// FetchPosts runs one cursor page of a post search.
func (e *Engine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {
	if discoveryQuery(tags).popular {
		result, err := e.FetchPage(c, site, tags, limit, page)
		return result.Posts, err
	}
	tags = discoveryQuery(tags).query
	return FetchPostsWithParser(c, site, tags, limit, page, e.ParsePost)
}

// FetchPostsWithParser shares the search protocol while retaining the site's
// payload parser. Go method delegation does not dispatch overridden methods
// like Python's self.parse_post does.
func FetchPostsWithParser(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any, parse func(map[string]any, *core.Site) *core.PostInfo) ([]*core.PostInfo, error) {

	// cursor pagination (page=b<id>) needs a stable id order
	if !strings.Contains(tags, "order:") {
		tags = strings.TrimSpace(tags + " order:id_desc")
	}
	q := url.Values{}
	q.Set("tags", tags)
	q.Set("limit", fmt.Sprint(limit))
	q.Set("page", pageString(page))
	data, err := c.GetJSON(nil, site.Base()+"/posts.json", q, nil)
	if err != nil {
		return nil, err
	}
	return parsePosts(data, site, parse)
}

func parsePosts(data any, site *core.Site, parse func(map[string]any, *core.Site) *core.PostInfo) ([]*core.PostInfo, error) {
	var list []any
	if m, ok := data.(map[string]any); ok {
		var valid bool
		list, valid = m["posts"].([]any)
		if !valid {
			return nil, fmt.Errorf("API: отсутствует список posts")
		}
	} else if l, ok := data.([]any); ok {
		list = l
	} else {
		return nil, fmt.Errorf("API: некорректный ответ списка постов")
	}
	out := make([]*core.PostInfo, 0, len(list))
	for _, item := range list {
		d, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("API: некорректный пост")
		}
		p := parse(d, site)
		if p == nil || p.ID.String() == "" || p.ID.IsNu && p.ID.Num <= 0 {
			return nil, fmt.Errorf("API: некорректный ID поста")
		}
		out = append(out, p)
	}
	return out, nil
}

// pageString renders the Python str(page) form for query params.
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

// NextCursor: oldest-first (order:id, ascending) walks *forward* past the
// highest id seen with a<id>; default newest-first walks back with b<id>.
// Ranked orders must use numeric pages: an ID boundary loses ranked results.
func (e *Engine) NextCursor(tags string, batch []*core.PostInfo, cursor any) any {
	if len(batch) == 0 {
		return nil
	}
	q := discoveryQuery(tags)
	if q.popular {
		return nil
	} else if q.order != "" && q.order != "id" && q.order != "id_asc" && q.order != "id_desc" {
		if page, err := strconv.Atoi(pageString(cursor)); err == nil && page > 0 {
			return page + 1
		}
		return e.EngineBase.NextCursor(tags, batch, cursor)
	}
	if q.order == "id" || q.order == "id_asc" {
		return fmt.Sprintf("a%s", batch[len(batch)-1].ID.String())
	}
	return fmt.Sprintf("b%s", batch[len(batch)-1].ID.String())
}

// ParsePost converts one API payload into PostInfo.
func (e *Engine) ParsePost(d map[string]any, site *core.Site) *core.PostInfo {
	f, _ := d["file"].(map[string]any)
	md5 := ""
	ext := ""
	var fURL any
	size, width, height := int64(0), 0, 0
	if f != nil {
		md5 = core.Str_(f["md5"])
		ext = strings.ToLower(core.Str_(f["ext"]))
		fURL = f["url"]
		size = core.AsInt(f["size"])
		width = int(core.AsInt(f["width"]))
		height = int(core.AsInt(f["height"]))
	}
	urlStr, _ := fURL.(string)
	if urlStr == "" && md5 != "" && ext != "" {
		urlStr = site.FileURLFromMd5(md5, ext)
	}
	var file *core.PostFile
	if urlStr != "" && md5 != "" {
		file = &core.PostFile{URL: urlStr, MD5: md5, Ext: ext,
			Size: size, Width: width, Height: height}
	}
	preview, _ := d["preview"].(map[string]any)
	previewURL := ""
	if preview != nil {
		previewURL, _ = preview["url"].(string)
	}
	if previewURL == "" && md5 != "" {
		previewURL = site.PreviewURLFromMd5(md5)
	}
	sampleMap, _ := d["sample"].(map[string]any)
	sampleURL := ""
	if sampleMap != nil {
		sampleURL, _ = sampleMap["url"].(string)
	}
	if sampleURL == "" {
		if core.IMAGE_EXTS[ext] {
			sampleURL = urlStr
		} else {
			sampleURL = previewURL
		}
	}
	allTags := []string{}
	var authorTags []string
	if tagsObj, ok := d["tags"].(map[string]any); ok {
		for _, v := range tagsObj {
			if list, ok := v.([]any); ok {
				for _, t := range list {
					if s, ok := t.(string); ok {
						allTags = append(allTags, s)
					}
				}
			}
		}
		// e621 calls the author category "artist"; e6ai calls it "director"
		for _, key := range []string{"artist", "director"} {
			if list, ok := tagsObj[key].([]any); ok {
				for _, t := range list {
					if s, ok := t.(string); ok {
						authorTags = append(authorTags, s)
					}
				}
			}
		}
	}
	banned := map[string]bool{"conditional_dnp": true, "avoid_posting": true,
		"unknown_artist": true, "third-party_edit": true,
		"unknown_director": true, "anonymous_director": true}
	artists := make([]string, 0, len(authorTags))
	for _, t := range authorTags {
		if !banned[t] {
			artists = append(artists, t)
		}
	}
	score := int64(0)
	if sm, ok := d["score"].(map[string]any); ok {
		score = core.AsInt(sm["total"])
	}
	return &core.PostInfo{
		ID:         core.I(core.AsInt(d["id"])),
		Site:       site.Key,
		File:       file,
		PreviewURL: previewURL,
		SampleURL:  sampleURL,
		Score:      score,
		FavCount:   core.AsInt(d["fav_count"]),
		Rating:     core.StrDflt_(d["rating"], "?"),
		Artists:    artists,
		Tags:       allTags,
		CreatedAt:  core.ParseTime(d["created_at"]),
	}
}

// Autocomplete fetches tag suggestions from /tags/autocomplete.json.
func (e *Engine) Autocomplete(c *core.ApiClient, site *core.Site,
	query string) ([]core.TagSuggestion, error) {

	if len(query) < 3 {
		return nil, nil
	}
	q := url.Values{}
	q.Set("search[name_matches]", query)
	data, err := c.GetJSON(nil, site.Base()+"/tags/autocomplete.json", q, nil)
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
		ts := core.TagSuggestion{
			Name:      core.Str_(d["name"]),
			PostCount: core.AsInt(d["post_count"]),
			Category:  int(core.AsInt(d["category"])),
		}
		if a, ok := d["antecedent_name"].(string); ok {
			ts.Antecedent = a
		}
		out = append(out, ts)
	}
	return out, nil
}

// SearchArtists queries artist tags ordered by post count.
func (e *Engine) SearchArtists(c *core.ApiClient, site *core.Site, query string,
	limit int) ([]core.TagSuggestion, error) {

	if query == "" {
		return nil, nil
	}
	q := url.Values{}
	q.Set("search[name_matches]", "*"+query+"*")
	q.Set("search[category]", "1")
	q.Set("search[order]", "count")
	q.Set("limit", fmt.Sprint(limit))
	data, err := c.GetJSON(nil, site.Base()+"/tags.json", q, nil)
	if err != nil {
		return nil, err
	}
	if m, ok := data.(map[string]any); ok { // empty -> {"tags": []}
		list, _ := m["tags"].([]any)
		data = list
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
			PostCount: core.AsInt(d["post_count"]),
			Category:  1,
		})
	}
	return out, nil
}

// DeriveArtists: engines that categorise tags (e621) fill Artists at parse
// time; this just returns them (falling back to hinted tags like Python).
func (e *Engine) DeriveArtists(post *core.PostInfo, hintTags []string) []string {
	return core.DeriveArtistsDefault(post, hintTags)
}

// Register installs the engine and both sites (called from sites.go init).
func Register() {
	core.RegisterEngine(&Engine{})
	registerE621Net()
	registerE6aiNet()
}
