// Package danbooru — Danbooru-family engine (Danbooru + AIBooru).
//
// Danbooru shares e621's protocol — the same page=b<id> cursor pagination,
// the same /tags.json artist lookup, the same /posts/<id> permalinks. What
// differs is the payload: everything flat (file_url, tag_string_artist as
// space-separated strings, a plain integer score). The post payload's media
// URLs stay primary; only a failed image byte transfer lazily resolves
// variants from the post's /media_assets/<post-id>.json. Tag suggestions come
// from /autocomplete.json.
package danbooru

import (
	"fmt"
	"net/url"
	"strings"

	"massdownloader/internal/core"
	"massdownloader/internal/sites/e621"
)

// Filters mirrors app/sites/danbooru/filters.py: same set as e621 but
// Danbooru split "safe" into g(eneral) and s(ensitive) in 2022.
func Filters() core.FilterList {
	return core.FilterList{
		core.NewAuthors(),
		core.NewTags(),
		core.NewExclude(),
		core.NewThreshold("score", "Score", "score", 100),
		core.NewThreshold("faves", "Избранное (faves)", "favcount", 50),
		{Kind: core.KindChoice, Key: "rating", Label: "Рейтинг", Token: "rating",
			Options: []core.ChoiceOption{
				{Label: "Любой", Value: ""}, {Label: "General", Value: "g"},
				{Label: "Sensitive", Value: "s"}, {Label: "Questionable", Value: "q"},
				{Label: "Explicit", Value: "e"},
			}},
		core.NewSort(),
		core.ImagesOnlyFilter("Только изображения (без webm/mp4)",
			"Не скачивать видео и анимацию.\nСнимите галочку, чтобы качать и ролики/GIF."),
	}
}

// Engine ports DanbooruEngine (reuses e621's FetchPosts/NextCursor via
// explicit delegation — Go embedding would be ambiguous between the two).
type Engine struct {
	core.EngineBase
	E6 *e621.Engine
}

func (e *Engine) Key() string               { return "danbooru" }
func (e *Engine) UsesCursor() bool          { return true }
func (e *Engine) SupportsMediaAssets() bool { return true }
func (e *Engine) Filters() core.FilterList {
	gold := core.NewBool("use_gold", "Использовать Gold", "", false)
	gold.LocalOnly = true
	gold.Tooltip = "Для постов с любым из тегов Gold сразу использовать media_assets при загрузке изображений."
	tags := core.NewTags()
	tags.Key, tags.Label, tags.Placeholder = "gold_tags", "Теги Gold (любой из)", "добавить тег… (пусто — обычная загрузка)"
	tags.EnabledIf, tags.LocalOnly = "use_gold", true
	return append(Filters(), gold, tags)
}

// Share e621's protocol while explicitly selecting the flat Danbooru parser.
func (e *Engine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {
	posts, err := e621.FetchPostsWithParser(c, site, tags, limit, page, e.ParsePost)
	for _, post := range posts {
		c.ApplyMediaPolicy(post)
	}
	return posts, err
}

func (e *Engine) NextCursor(tags string, batch []*core.PostInfo, cursor any) any {
	return e.E6.NextCursor(tags, batch, cursor)
}
func (e *Engine) AuthorScanDescending(query string) bool { return e.E6.AuthorScanDescending(query) }

func (e *Engine) SearchArtists(c *core.ApiClient, site *core.Site, query string,
	limit int) ([]core.TagSuggestion, error) {
	return e.E6.SearchArtists(c, site, query, limit)
}

// DeriveArtists mirrors the e621 default.
func (e *Engine) DeriveArtists(post *core.PostInfo, hintTags []string) []string {
	return core.DeriveArtistsDefault(post, hintTags)
}

// MediaEndpointID is the ID accepted by this site's media_assets endpoint.
// Danbooru's compatibility route accepts a post ID; AIBooru overrides with
// the nested media_asset.id.
func (e *Engine) MediaEndpointID(postId int64, mediaAsset map[string]any) int64 {
	return postId
}

// mediaRef builds the old URL plus a lazy media_assets fallback reference:
// danbooru-media://<post_id>/<kind>?primary=...&md5=...
// ApiClient tries primary first and requests the owning site's
// media_assets/<endpoint_id>.json only after that URL fails.
func (e *Engine) mediaRef(site *core.Site, postId int64, kind string,
	primary, md5 string, endpointId int64) string {

	params := url.Values{}
	if site.Key != "danbooru" {
		params.Set("site", site.Key)
	}
	if endpointId != 0 && endpointId != postId {
		params.Set("asset", fmt.Sprint(endpointId))
	}
	if md5 != "" {
		params.Set("md5", md5)
	}
	if primary != "" {
		params.Set("primary", primary)
	}
	q := params.Encode()
	suffix := ""
	if q != "" {
		suffix = "?" + q
	}
	return fmt.Sprintf("danbooru-media://%d/%s%s", postId, kind, suffix)
}

// ParsePost converts one flat Danbooru payload into PostInfo.
func (e *Engine) ParsePost(d map[string]any, site *core.Site) *core.PostInfo {
	return e.parsePost(d, site, e.MediaEndpointID)
}

func (e *Engine) parsePost(d map[string]any, site *core.Site,
	mediaEndpointID func(int64, map[string]any) int64) *core.PostInfo {
	postId := core.AsInt(d["id"])
	mediaAsset, _ := d["media_asset"].(map[string]any)
	if mediaAsset == nil {
		mediaAsset = map[string]any{}
	}
	endpointId := mediaEndpointID(postId, mediaAsset)
	md5 := strings.ToLower(core.Str_(d["md5"]))
	if md5 == "" {
		md5 = core.Str_(mediaAsset["md5"])
	}
	ext := strings.ToLower(core.Str_(d["file_ext"]))
	if ext == "" {
		ext = strings.ToLower(core.Str_(mediaAsset["file_ext"]))
	}
	legacyURL := core.Str_(d["file_url"])
	urlStr := legacyURL
	isImage := core.IMAGE_EXTS[ext]
	if isImage && postId > 0 {
		urlStr = e.mediaRef(site, postId, "original", legacyURL, md5, endpointId)
	}
	var file *core.PostFile
	if urlStr != "" && (md5 != "" || isImage) {
		size := core.AsInt(d["file_size"])
		if size == 0 {
			size = core.AsInt(mediaAsset["file_size"])
		}
		w := core.AsInt(d["image_width"])
		if w == 0 {
			w = core.AsInt(mediaAsset["image_width"])
		}
		h := core.AsInt(d["image_height"])
		if h == 0 {
			h = core.AsInt(mediaAsset["image_height"])
		}
		file = &core.PostFile{URL: urlStr, MD5: md5, Ext: ext,
			Size: size, Width: int(w), Height: int(h)}
	}
	legacyPreview := core.Str_(d["preview_file_url"])
	preview := legacyPreview
	var sample string
	if isImage {
		// large_file_url is a downscaled still for images but the raw video
		// for mp4/webm — only trust it for images
		sample = core.Str_(d["large_file_url"])
		if sample == "" {
			sample = legacyURL
		}
		if sample == "" {
			sample = legacyPreview
		}
		if postId > 0 {
			preview = e.mediaRef(site, postId, "preview", preview, md5, endpointId)
			sample = e.mediaRef(site, postId, "sample", sample, md5, endpointId)
		}
	} else {
		sample = preview
	}
	return &core.PostInfo{
		ID:         core.I(postId),
		Site:       site.Key,
		File:       file,
		PreviewURL: preview,
		SampleURL:  sample,
		Score:      core.AsInt(d["score"]),
		FavCount:   core.AsInt(d["fav_count"]),
		Rating:     core.StrDflt_(d["rating"], "?"),
		Artists:    splitWords(core.Str_(d["tag_string_artist"])),
		Tags:       splitWords(core.Str_(d["tag_string"])),
		CreatedAt:  core.ParseTime(d["created_at"]),
	}
}

func splitWords(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Fields(s)
}

var catMap = map[int64]int64{5: 7} // Danbooru meta(5) -> palette meta(7)

// Autocomplete fetches suggestions from /autocomplete.json.
func (e *Engine) Autocomplete(c *core.ApiClient, site *core.Site,
	query string) ([]core.TagSuggestion, error) {

	if len(query) < 3 {
		return nil, nil
	}
	q := url.Values{}
	q.Set("search[query]", query)
	q.Set("search[type]", "tag_query")
	q.Set("limit", "10")
	data, err := c.GetJSON(nil, site.Base()+"/autocomplete.json", q, nil)
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
		cat := core.AsInt(d["category"])
		mapped := cat
		if m, ok := catMap[cat]; ok {
			mapped = m
		}
		ts := core.TagSuggestion{Name: name,
			PostCount: core.AsInt(d["post_count"]), Category: int(mapped)}
		if a, ok := d["antecedent"].(string); ok {
			ts.Antecedent = a
		}
		out = append(out, ts)
	}
	return out, nil
}

// Register installs the engine and both sites.
func Register() {
	core.RegisterEngine(&Engine{E6: &e621.Engine{}})
	registerDanbooruDonmai()
	registerAIBooru()
}

func registerDanbooruDonmai() {
	core.RegisterSite(&core.Site{
		Key:          "danbooru",
		Label:        "danbooru.donmai.us",
		Domain:       "danbooru.donmai.us",
		StaticDomain: "cdn.donmai.us",
		Engine:       "danbooru",
		PageLimit:    200,
		// Anonymous access remains the default; if Cloudflare starts 403ing,
		// the user can replay cf_clearance + UA exactly as on e621/e6ai.
		CfProtected: true,
		// Search uses danbooru.donmai.us; every preview/original is served by
		// this official sibling host and must retain the same CF identity.
		CfExtraDomains: []string{"cdn.donmai.us"},
	})
}

func registerAIBooru() {
	core.RegisterEngine(&aiBooruEngine{Engine{E6: &e621.Engine{}}})
	core.RegisterSite(&core.Site{
		Key:          "aibooru",
		Label:        "AIBooru",
		Domain:       "aibooru.online",
		StaticDomain: "cdn.aibooru.download",
		Engine:       "aibooru",
		PageLimit:    200,
	})
}

// aiBooruEngine differs only in the media-assets route: AIBooru expects the
// nested media_asset.id while Danbooru's compatibility route expects the
// post ID. A separate engine key keeps those contracts isolated.
type aiBooruEngine struct{ Engine }

func (e *aiBooruEngine) Key() string              { return "aibooru" }
func (e *aiBooruEngine) Filters() core.FilterList { return Filters() }

func (e *aiBooruEngine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {
	return e621.FetchPostsWithParser(c, site, tags, limit, page, e.ParsePost)
}

func (e *aiBooruEngine) ParsePost(d map[string]any, site *core.Site) *core.PostInfo {
	return e.parsePost(d, site, e.MediaEndpointID)
}
func (e *aiBooruEngine) MediaEndpointID(postId int64,
	mediaAsset map[string]any) int64 {

	if n := core.AsInt(mediaAsset["id"]); n != 0 {
		return n
	}
	return postId
}
