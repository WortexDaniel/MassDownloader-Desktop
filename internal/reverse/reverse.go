// Package reverse — reverse-image-search core: the provider contract + a
// small registry. Port of app/core/reverse.py.
//
// A reverse provider takes the bytes of an image and answers "where does this
// image appear?" Each provider (SauceNAO, Fluffle, IQDB) lives in its own file
// under internal/reverse and registers itself here, mirroring how engines and
// sites register in the core registry.
package reverse

import (
	"fmt"
	"net/url"
	"strings"
	"sync"

	"massdownloader/internal/core"
)

// Match is one "this image was found here" hit, normalised across providers.
type Match struct {
	Provider   string  `json:"provider"` // which backend produced it
	Source     string  `json:"source"`   // human label ("e621", "FurAffinity", …)
	URL        string  `json:"url"`
	ThumbURL   string  `json:"thumb_url"`
	Similarity float64 `json:"similarity"` // 0..100 confidence
	Author     string  `json:"author,omitempty"`
	SiteKey    string  `json:"site_key,omitempty"` // set when mappable in-app
	PostID     string  `json:"post_id"`
	InApp      bool    `json:"in_app"`
	Note       string  `json:"note,omitempty"`
}

// Provider is the contract for a reverse-image backend (stateless).
type Provider interface {
	Key() string
	Label() string
	// Blurb is a one-line note shown under the provider in the results header.
	Blurb() string
	// Available reports readiness; a provider needing an API key returns
	// false plus the reason so the UI can prompt instead of failing silently.
	Available(getSetting func(string) string) (bool, string)
	// Search returns matches for image (raw bytes). ctx cancellation stops it.
	Search(c *core.ApiClient, image []byte, filename string,
		getSetting func(string) string,
		shouldStop func() bool) ([]Match, error)
}

var (
	regMu    sync.Mutex
	registry = map[string]Provider{}
	order    []string
)

// Register installs one provider preserving order.
func Register(p Provider) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, seen := registry[p.Key()]; !seen {
		order = append(order, p.Key())
	}
	registry[p.Key()] = p
}

// Providers lists registered providers in registration order.
func Providers() []Provider {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]Provider, 0, len(order))
	for _, k := range order {
		out = append(out, registry[k])
	}
	return out
}

// FindSitePost maps a found post URL onto one of the app's sites and its post
// id so the match can be browsed/downloaded in-app. Returns "" for an
// unsupported host — those stay browser-only. The host picks the Site; the id
// is parsed by that site's engine (IdFromURL), keeping every URL scheme next
// to the engine that owns it.
func FindSitePost(rawURL string) (string, core.PostId) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "", core.PostId{}
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	if key := map[string]string{"twitter.com": "twitter", "kemono.party": "kemono", "kemono.su": "kemono"}[host]; key != "" {
		if site := core.GetSite(key); site != nil {
			return key, core.Registry.EngineFor(site).IdFromURL(site, rawURL)
		}
	}
	for _, site := range core.Sites() {
		dom := strings.TrimPrefix(strings.ToLower(site.Domain), "www.")
		if host == dom || strings.HasSuffix(host, "."+dom) {
			engine := core.Registry.EngineFor(site)
			if engine == nil {
				return "", core.PostId{}
			}
			pid := engine.IdFromURL(site, rawURL)
			if !pid.IsNu || pid.Num != 0 || pid.Str != "" {
				if pid.IsNu && pid.Num == 0 {
					return site.Key, core.I(0)
				}
				return site.Key, pid
			}
			return site.Key, core.I(0)
		}
	}
	return "", core.PostId{}
}

// GuessMime maps a file extension to the multipart upload content-type.
func GuessMime(filename string) string {
	ext := ""
	if i := strings.LastIndex(filename, "."); i >= 0 {
		ext = strings.ToLower(filename[i+1:])
	}
	switch ext {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "bmp":
		return "image/bmp"
	}
	return "application/octet-stream"
}

// minSim reads the shared reverse_min_similarity setting with its default.
func minSim(getSetting func(string) string) float64 {
	v := getSetting("reverse_min_similarity")
	f, err := strconvParseFloat(v)
	if err != nil || f <= 0 {
		return 55.0
	}
	return f
}

func strconvParseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	return f, err
}

// resolveInApp auto-fills SiteKey/PostID/InApp from the match URL.
func resolveInApp(m *Match) {
	if m.URL != "" && m.SiteKey == "" {
		key, pid := FindSitePost(m.URL)
		if key != "" {
			m.SiteKey = key
			m.PostID = pid.String()
			m.InApp = pid.String() != "" && pid.String() != "0" && core.SupportsReverseFetch(core.Registry.EngineFor(core.GetSite(key)))
		}
	}
}
