// Central registry of engines and sites.
// Port of app/core/registry.py + the Engine contract from engine.py.
package core

import (
	"fmt"
	"regexp"
	"sort"
)

// Engine is the contract every booru protocol implements. Engines are
// stateless: the shared *ApiClient is passed in on every call so a single
// rate-limited HTTP session is reused. Concrete engines live under
// internal/sites/<engine> and register via RegisterEngine in an init().
type Engine interface {
	Key() string

	// UsesCursor: true -> e621 cursor pagination (page=b<lowest_id>);
	// false -> plain numeric pages (1, 2, 3...).
	UsesCursor() bool

	// SupportsMediaAssets: whether this engine emits internal
	// danbooru-media:// references resolved through a registered site's
	// /media_assets/<id>.json endpoint. The client checks this capability
	// before following the embedded site key, so a persisted/tampered
	// reference cannot turn the resolver into an arbitrary-host request.
	SupportsMediaAssets() bool
	// SupportsAlbums reports whether a post can contain multiple attachments.
	SupportsAlbums() bool

	// Filters is the filter spec sequence offered in the sidebar; a site may
	// override via Site.Filters.
	Filters() FilterList

	// TagPhrase: whether the tag/exclude fields hold free-text phrases rather
	// than booru tags (DeviantArt). See ui tag editor.
	TagPhrase() bool

	// FetchPosts runs one page of a post search and returns parsed posts.
	FetchPosts(c *ApiClient, site *Site, tags string, limit int,
		page any) ([]*PostInfo, error)

	// ValidateQuery reports ApiError when tags cannot form a search request.
	// Most engines allow an empty query; engines with mandatory terms
	// override this hook; the preview UI calls it before starting a worker.
	ValidateQuery(tags string) error

	// NextCursor returns the page token for the page after batch (the page
	// just fetched for tags). Plain numeric-page engines increment; cursor
	// engines page by boundary post id — e621 walks forward with a<id> under
	// oldest-first order:id instead of back with b<id>. batch non-empty.
	NextCursor(tags string, batch []*PostInfo, cursor any) any

	// Autocomplete gives tag suggestions for include/exclude fields.
	Autocomplete(c *ApiClient, site *Site, query string) ([]TagSuggestion, error)

	// SearchArtists gives artist-tag suggestions for the authors field.
	SearchArtists(c *ApiClient, site *Site, query string,
		limit int) ([]TagSuggestion, error)

	// DownloadURLs lists candidate download URLs tried in order (Pixiv
	// guesses .png then .jpg).
	DownloadURLs(post *PostInfo) []string

	// PrepareView makes post fully viewable, rewriting its image URLs in
	// place, returning whether anything changed (so the viewer reloads).
	// DeviantArt overrides it for watchers-only works.
	PrepareView(c *ApiClient, post *PostInfo) (bool, error)

	// Release undoes transient account state taken on to fetch a run's files;
	// called once a download wave finishes and on shutdown. DeviantArt
	// unwatches here. Must be safe repeatedly and when nothing was taken.
	Release(c *ApiClient)

	// CheckAvailable probes site health for the header indicator.
	CheckAvailable(c *ApiClient, site *Site) bool

	// VerifiesHash reports whether bytes served at download time should hash
	// to post.File.MD5. Gelbooru transcodes video on its CDN -> false there.
	VerifiesHash(post *PostInfo) bool

	// ExpandPost splits a multi-image work into one PostInfo per image;
	// single-image posts return [post] unchanged (Pixiv albums override).
	ExpandPost(c *ApiClient, post *PostInfo) []*PostInfo

	// IdFromURL parses a post id out of a permalink on site (the inverse of
	// Site.PostURL); 0 when not found. Backs reverse-image-search mapping.
	IdFromURL(site *Site, url string) PostId

	// Permalink is the human-facing page URL for post («открыть на сайте»).
	Permalink(site *Site, post *PostInfo) string

	// PostByID fetches one post by id (reverse-search hit resolution);
	// nil when absent.
	PostByID(c *ApiClient, site *Site, id PostId) (*PostInfo, error)

	// ArtistQuery builds the «работы автора» query; "" signals unavailable.
	ArtistQuery(artist string, post *PostInfo) string

	// DeriveArtists recovers author tags for display/folder naming.
	DeriveArtists(post *PostInfo, hintTags []string) []string
}

// EngineBase provides the Python defaults for every optional method. Engines
// embed it and override selectively.
type EngineBase struct{}

func (EngineBase) UsesCursor() bool           { return false }
func (EngineBase) SupportsMediaAssets() bool  { return false }
func (EngineBase) SupportsAlbums() bool       { return false }
func (EngineBase) TagPhrase() bool            { return false }
func (EngineBase) ValidateQuery(string) error { return nil }

func (EngineBase) NextCursor(tags string, batch []*PostInfo,
	cursor any) any {
	if c, ok := cursor.(int); ok {
		return c + 1
	}
	if s, ok := cursor.(string); ok {
		if n := atoiOr0(s); n > 0 {
			return n + 1
		}
	}
	return 2
}

func (EngineBase) Autocomplete(*ApiClient, *Site,
	string) ([]TagSuggestion, error) {
	return nil, nil
}

func (EngineBase) SearchArtists(*ApiClient, *Site, string,
	int) ([]TagSuggestion, error) {
	return nil, nil
}

func (b EngineBase) DownloadURLs(post *PostInfo) []string {
	if post != nil && post.File != nil {
		return []string{post.File.URL}
	}
	return nil
}

func (EngineBase) PrepareView(*ApiClient, *PostInfo) (bool, error) {
	return false, nil
}

func (EngineBase) Release(*ApiClient) {}

func (EngineBase) CheckAvailable(c *ApiClient, site *Site) bool {
	if c == nil {
		return false
	}
	return c.IsReachable(site.Base())
}

func (EngineBase) VerifiesHash(*PostInfo) bool { return true }

func (EngineBase) ExpandPost(_ *ApiClient, post *PostInfo) []*PostInfo {
	if post == nil {
		return nil
	}
	return []*PostInfo{post}
}

var postsRe = regexp.MustCompile(`/posts/(\d+)`)

func (EngineBase) IdFromURL(_ *Site, url string) PostId {
	if m := postsRe.FindStringSubmatch(url); m != nil {
		n := atoiOr0(m[1])
		if n > 0 {
			return I(n)
		}
	}
	return I(0)
}

func (EngineBase) Permalink(site *Site, post *PostInfo) string {
	return site.PostURL(post.ID)
}

func (e EngineBase) PostByID(c *ApiClient, site *Site,
	id PostId) (*PostInfo, error) {
	if !id.IsNu || id.Num <= 0 {
		return nil, nil
	}
	engine := Registry.EngineFor(site)
	if engine == nil {
		return nil, fmt.Errorf("no engine for %s", site.Key)
	}
	posts, err := engine.FetchPosts(c, site, fmt.Sprintf("id:%d", id.Num), 1, 1)
	if err != nil || len(posts) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
	return posts[0], nil
}

func (EngineBase) ArtistQuery(artist string, _ *PostInfo) string {
	return trimSpace(artist)
}

// HintedOrUploader mirrors Engine.hinted_or_uploader: the searched artist
// tags when the post carries them, else the stashed uploader names.
func HintedOrUploader(post *PostInfo, hintTags []string) []string {
	tagset := make(map[string]bool, len(post.Tags))
	for _, t := range post.Tags {
		tagset[t] = true
	}
	var hinted []string
	for _, t := range hintTags {
		if tagset[t] {
			hinted = append(hinted, t)
		}
	}
	if len(hinted) > 0 {
		return hinted
	}
	return append([]string(nil), post.Artists...)
}

// TagOrUserQuery mirrors Engine.tag_or_user_query for flat-tag boorus: a
// real artist tag searches directly; an uploader handle via user:<name>.
func TagOrUserQuery(artist string, post *PostInfo) string {
	a := trimSpace(artist)
	if a == "" {
		return ""
	}
	if post != nil {
		for _, t := range post.Tags {
			if t == a {
				return a
			}
		}
	}
	return "user:" + a
}

// DeriveArtistsDefault mirrors Engine.derive_artists.
func DeriveArtistsDefault(post *PostInfo, hintTags []string) []string {
	if len(post.Artists) > 0 {
		return append([]string(nil), post.Artists...)
	}
	tagset := make(map[string]bool, len(post.Tags))
	for _, t := range post.Tags {
		tagset[t] = true
	}
	var out []string
	for _, t := range hintTags {
		if tagset[t] {
			out = append(out, t)
		}
	}
	return out
}

// -- registry -------------------------------------------------------------

type registry struct {
	engines map[string]Engine
	sites   map[string]*Site
	order   []string // site keys in registration order
}

// Registry holds all engines and sites, populated by package init() calls.
var Registry = &registry{
	engines: map[string]Engine{},
	sites:   map[string]*Site{},
}

// RegisterEngine installs one engine under its key.
func RegisterEngine(e Engine) { Registry.engines[e.Key()] = e }

// RegisterSite installs one site under its key, preserving order. PageLimit 0
// falls back to the models.py default (320) — the Python dataclass default.
func RegisterSite(s *Site) {
	if s.PageLimit == 0 {
		s.PageLimit = MAX_PAGE_LIMIT
	}
	if _, seen := Registry.sites[s.Key]; !seen {
		Registry.order = append(Registry.order, s.Key)
	}
	Registry.sites[s.Key] = s
}

// GetEngine panics like Python's KeyError on unknown key.
func GetEngine(key string) Engine {
	e, ok := Registry.engines[key]
	if !ok {
		panic(fmt.Sprintf("unknown engine %q", key))
	}
	return e
}

// GetSite looks a site up by key (nil when unknown — callers handle).
func GetSite(key string) *Site { return Registry.sites[key] }

// SiteKeys lists sites in registration order.
func SiteKeys() []string { return append([]string(nil), Registry.order...) }

// Sites lists all registered sites in registration order.
func Sites() []*Site {
	out := make([]*Site, 0, len(Registry.order))
	for _, k := range Registry.order {
		out = append(out, Registry.sites[k])
	}
	return out
}

// EngineFor resolves the engine handling site.
func (r *registry) EngineFor(site *Site) Engine {
	if site == nil {
		return nil
	}
	return r.engines[site.Engine]
}

func EngineCount() int { return len(Registry.engines) }

func SupportsReverseFetch(e Engine) bool {
	if e == nil {
		return false
	}
	switch e.Key() {
	case "twitter", "sankaku", "sankaku_legacy", "kemono", "onlyhaven":
		return false
	}
	return true
}

// PostArtists resolves author tags through the owning engine.
func PostArtists(post *PostInfo, hintTags []string) []string {
	site := GetSite(post.Site)
	if site == nil {
		return post.Artists
	}
	e := Registry.EngineFor(site)
	if e == nil {
		return post.Artists
	}
	return e.DeriveArtists(post, hintTags)
}

// FiltersFor returns the specs to show for a site: its own override or the
// engine's default set.
func FiltersFor(site *Site) FilterList {
	if site == nil {
		return nil
	}
	e := Registry.EngineFor(site)
	filters := site.Filters
	if filters == nil && e != nil {
		filters = e.Filters()
	}
	if e != nil && e.SupportsAlbums() && filters.Find("group_albums") == nil {
		filters = append(append(FilterList(nil), filters...), GroupAlbumsFilter())
	}
	return filters
}

// SortSites orders site keys alphabetically (UI convenience).
func SortSites(keys []string) { sort.Strings(keys) }

func atoiOr0(s string) int64 {
	n := int64(0)
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		n = n*10 + int64(s[i]-'0')
	}
	return n
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
