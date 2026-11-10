package reverse

import (
	"crypto/md5"
	"fmt"
	"massdownloader/internal/core"
	"strings"
)

type ExactMD5 struct{}

func (ExactMD5) Key() string   { return "exact_md5" }
func (ExactMD5) Label() string { return "Точный MD5" }
func (ExactMD5) Blurb() string {
	return "Оригиналы: e621/e6ai · AIBooru · Konachan · Gelbooru-сайты"
}
func (ExactMD5) Available(func(string) string) (bool, string) { return true, "" }
func (p ExactMD5) Search(c *core.ApiClient, image []byte, _ string, _ func(string) string, stop func() bool) ([]Match, error) {
	digest := fmt.Sprintf("%x", md5.Sum(image))
	out := []Match{}
	for _, key := range []string{"e621", "e6ai", "aibooru", "konachan", "gelbooru", "rule34", "xbooru", "tbib"} {
		if stop != nil && stop() {
			break
		}
		site := core.GetSite(key)
		e := core.Registry.EngineFor(site)
		if e == nil {
			continue
		}
		posts, err := e.FetchPosts(c, site, "md5:"+digest, 1, 1)
		if err != nil {
			continue
		}
		for _, post := range posts[:min(1, len(posts))] {
			if post.File == nil || !strings.EqualFold(post.File.MD5, digest) {
				continue
			}
			out = append(out, Match{Provider: p.Label(), Source: site.Label, URL: e.Permalink(site, post), ThumbURL: post.PreviewURL, Similarity: 100, Author: strings.Join(core.PostArtists(post, nil), ", "), SiteKey: key, PostID: post.ID.String(), InApp: core.SupportsReverseFetch(e), Note: "точное совпадение MD5"})
		}
	}
	return out, nil
}
