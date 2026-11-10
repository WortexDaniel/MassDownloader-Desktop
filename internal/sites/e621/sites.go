package e621

import "massdownloader/internal/core"

// registerE621Net ports app/sites/e621/e621_net.py: the flagship furry booru.
func registerE621Net() {
	core.RegisterSite(&core.Site{
		Key:          "e621",
		Label:        "e621.net",
		Domain:       "e621.net",
		StaticDomain: "static1.e621.net",
		Engine:       "e621",
		// e621 documents a hard limit of 2 req/s and asks for ~1 req/s.
		MinInterval: 0.75,
		// e621 sits behind Cloudflare's interactive challenge; when it 403s,
		// the user pastes a cf_clearance cookie + their browser User-Agent
		CfProtected:    true,
		CfExtraDomains: []string{"static1.e621.net"},
	})
}

// registerE6aiNet ports e6ai_net.py: sister site (AI art). Same engine; the
// author category is called "director" instead of "artist" (parser handles).
func registerE6aiNet() {
	core.RegisterSite(&core.Site{
		Key:            "e6ai",
		Label:          "e6ai.net",
		Domain:         "e6ai.net",
		StaticDomain:   "static1.e6ai.net",
		Engine:         "e621",
		MinInterval:    1.0,  // answered 429 (Retry-After: 2) at 0.55 s
		CfProtected:    true, // same Cloudflare front as e621
		CfExtraDomains: []string{"static1.e6ai.net"},
	})
}
