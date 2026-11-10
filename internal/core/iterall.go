package core

// IterAll walks a site's whole result set through the engine's own pagination
// (the iterator seam used by author-subscription checks).
func IterAll(c *ApiClient, site *Site, engine Engine, tags string,
	maxPosts int) func(yield func(*PostInfo) error) error {

	return func(yield func(*PostInfo) error) error {
		return c.IterPosts(nil, site.Key, tags, maxPosts,
			nil, func(p *PostInfo) bool { return yield(p) == nil })
	}
}
