package gelbooru

import (
	"strings"

	"massdownloader/internal/core"
)

// LegacyEngine ports LegacyGelbooruEngine — boorus whose dapi post payload
// predates the file_url/preview_url fields (tbib.org and friends). They carry
// no ready-made URLs; each post has directory+image from which the file,
// sample and thumbnail URLs are built.
type LegacyEngine struct{ Engine }

func (e *LegacyEngine) Key() string { return "gelbooru_legacy" }

func (e *LegacyEngine) FetchPosts(c *core.ApiClient, site *core.Site, tags string,
	limit int, page any) ([]*core.PostInfo, error) {
	return fetchPostsWithParser(c, site, tags, limit, page, e.ParsePost)
}

// ParsePost builds URLs from directory/image:
//
//	file      → <base>/images/<directory>/<image>
//	sample    → <base>/samples/<directory>/sample_<root>.jpg (when sample:true)
//	thumbnail → <base>/thumbnails/<directory>/thumbnail_<root>.jpg
func (e *LegacyEngine) ParsePost(d map[string]any, site *core.Site) *core.PostInfo {
	image := core.Str_(d["image"])
	directory := core.Str_(d["directory"])
	h := core.Str_(d["hash"])
	if h == "" {
		h = core.Str_(d["md5"])
	}
	ext, root := "", image
	if i := strings.LastIndex(image, "."); i >= 0 {
		ext = strings.ToLower(image[i+1:])
		root = image[:i]
	}
	hasDir := image != "" && d["directory"] != nil
	urlStr := ""
	if hasDir {
		urlStr = site.Base() + "/images/" + directory + "/" + image
	}
	var file *core.PostFile
	if urlStr != "" && h != "" {
		file = &core.PostFile{URL: urlStr, MD5: h, Ext: ext,
			Size:   core.AsInt(d["file_size"]),
			Width:  int(core.AsInt(d["width"])),
			Height: int(core.AsInt(d["height"]))}
	}
	preview := ""
	if hasDir {
		preview = site.Base() + "/thumbnails/" + directory +
			"/thumbnail_" + root + ".jpg"
	}
	// samples exist only for large images; otherwise the full file doubles as
	// the sample (the viewer just loads a bigger picture)
	sample := urlStr
	if hasDir {
		if s, ok := d["sample"].(bool); ok && s {
			sample = site.Base() + "/samples/" + directory +
				"/sample_" + root + ".jpg"
		}
	}
	owner := strings.TrimSpace(core.Str_(d["owner"]))
	var artists []string
	if owner != "" {
		artists = []string{owner}
	}
	return &core.PostInfo{
		ID:         core.I(core.AsInt(d["id"])),
		Site:       site.Key,
		File:       file,
		PreviewURL: preview,
		SampleURL:  sample,
		Score:      core.AsInt(d["score"]),
		FavCount:   0,
		Rating:     core.NormRating(d["rating"]),
		Artists:    artists,
		Tags:       splitWords(htmlUnescape(core.Str_(d["tags"]))),
		// legacy payloads only carry change (a unix epoch)
		CreatedAt: firstTime(d["change"], d["created_at"]),
	}
}

func htmlUnescape(s string) string {
	// encoding/html already imported in engine.go; reuse it via a tiny wrapper
	// to keep this file's imports minimal.
	return htmlUnescapeImpl(s)
}

func firstTime(a, b any) int64 {
	if t := core.ParseTime(a); t != 0 {
		return t
	}
	return core.ParseTime(b)
}
