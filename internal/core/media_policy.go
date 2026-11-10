package core

import (
	"net/url"
	"strings"
)

// SetDanbooruGold installs the tags that require the media_assets route.
// An empty or disabled list preserves primary-first transfers.
func (c *ApiClient) SetDanbooruGold(enabled bool, tags []string) {
	wanted := map[string]bool{}
	if enabled {
		for _, tag := range tags {
			if tag = strings.ToLower(strings.TrimSpace(tag)); tag != "" {
				wanted[tag] = true
			}
		}
	}
	c.mu.Lock()
	c.danbooruGold = wanted
	c.mu.Unlock()
}

// ApplyMediaPolicy can also refresh saved favorites and selected posts without
// refetching their metadata. Keep primary URLs so disabling Gold is reversible.
func (c *ApiClient) ApplyMediaPolicy(post *PostInfo) {
	if post == nil || post.Site != "danbooru" {
		return
	}
	force := false
	c.mu.Lock()
	for _, tag := range post.Tags {
		if c.danbooruGold[strings.ToLower(strings.TrimSpace(tag))] {
			force = true
			break
		}
	}
	c.mu.Unlock()
	rewrite := func(ref string) string {
		u, err := url.Parse(ref)
		if err != nil || u.Scheme != "danbooru-media" {
			return ref
		}
		q := u.Query()
		if site := q.Get("site"); site != "" && site != "danbooru" {
			return ref
		}
		if force {
			q.Set("force_assets", "1")
		} else {
			q.Del("force_assets")
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	post.PreviewURL, post.SampleURL = rewrite(post.PreviewURL), rewrite(post.SampleURL)
	if post.File != nil {
		post.File.URL = rewrite(post.File.URL)
	}
}
