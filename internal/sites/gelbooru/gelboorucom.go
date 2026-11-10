package gelbooru

import (
	"html"
	"net/url"
	"strconv"

	"massdownloader/internal/core"
)

func htmlUnescapeImpl(s string) string { return html.UnescapeString(s) }

// GelbooruComEngine ports GelbooruComEngine: gelbooru.com reuses the dapi
// protocol but serves suggestions from index.php?page=autocomplete2 with a
// cleaner JSON shape (a separate numeric post_count and a word category —
// "tag" for general). A small override of just that one seam; everything
// else (auth, permalinks, video hash skipping) is inherited.
type GelbooruComEngine struct{ Engine }

func (e *GelbooruComEngine) Key() string { return "gelbooru_com" }

// Embedded Engine methods call Engine.suggest, so bind these entry points
// explicitly to Gelbooru's endpoint.
func (e *GelbooruComEngine) Autocomplete(c *core.ApiClient, site *core.Site,
	query string) ([]core.TagSuggestion, error) {
	return e.suggest(c, site, query, 2)
}

func (e *GelbooruComEngine) SearchArtists(c *core.ApiClient, site *core.Site,
	query string, limit int) ([]core.TagSuggestion, error) {
	return searchArtistsWithSuggester(c, site, query, limit, e.suggest)
}

// suggest uses Gelbooru's tag-query endpoint and response shape.
func (e *GelbooruComEngine) suggest(c *core.ApiClient, site *core.Site,
	query string, minLen int) ([]core.TagSuggestion, error) {

	if len(query) < minLen {
		return nil, nil
	}
	q := url.Values{}
	q.Set("page", "autocomplete2")
	q.Set("term", query)
	q.Set("type", "tag_query")
	q.Set("limit", "20")
	data, err := c.GetJSON(nil, site.Base()+"/index.php", q,
		map[string]string{"Referer": site.Base() + "/"})
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
		// an unknown category (gelbooru's "tag") falls back to general (0)
		out = append(out, core.TagSuggestion{
			Name:      name,
			PostCount: core.AsInt(d["post_count"]),
			Category:  tagTypeWord[core.Str_(d["category"])],
		})
	}
	return out, nil
}

var _ = strconv.Itoa // keep strconv linked like the Python module's helpers
