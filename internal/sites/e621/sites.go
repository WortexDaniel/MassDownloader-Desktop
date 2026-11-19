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
		OptionalAuth: true,
		AuthURL:      "https://e621.net/api_keys",
		AuthFie:      []core.CredField{{Key: "login", Label: "Имя пользователя e621"}, {Key: "api_key", Label: "API-ключ e621"}},
		// e621 documents a hard limit of 2 req/s and asks for ~1 req/s.
		MinInterval: 0.75,
		// e621 sits behind Cloudflare's interactive challenge; when it 403s,
		// the user pastes a cf_clearance cookie + their browser User-Agent
		CfProtected:    true,
		CfExtraDomains: []string{"static1.e621.net"},
	})
}

func registerFloofGens() {
	const domain = "floofgens.net"
	const cdn = "static1.floofgens.net"
	core.RegisterSite(&core.Site{
		Key:            "e6ai", // persistent key for existing local data
		Label:          domain,
		Domain:         domain,
		StaticDomain:   cdn,
		PreviousHosts:  map[string]string{"e6ai.net": domain, "static1.e6ai.net": cdn},
		Engine:         "e621",
		MinInterval:    1.0,
		CfProtected:    true,
		CfExtraDomains: []string{cdn},
	})
}
