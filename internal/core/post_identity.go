package core

import "strings"

func ContentHash(p *PostInfo) string {
	if p == nil || p.File == nil {
		return ""
	}
	hash := strings.ToLower(p.File.MD5)
	if len(hash) != 32 && len(hash) != 40 && len(hash) != 64 {
		return ""
	}
	for _, char := range hash {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return ""
		}
	}
	return hash
}

// FileFingerprint identifies an attachment independently of its parent post.
func FileFingerprint(p *PostInfo) string {
	if p == nil || p.File == nil {
		return ""
	}
	if p.File.MD5 != "" {
		return strings.ToLower(p.File.MD5)
	}
	return p.File.URL
}

// LocalPostPages reads already available attachment metadata without network I/O.
// A nil result means the engine still needs to resolve the album's pages.
func LocalPostPages(p *PostInfo) []*PostInfo {
	if p == nil || p.File == nil {
		return nil
	}
	if p.PageCount <= 1 || p.Extra["expanded_page"] == true || p.Extra["standalone_attachment"] == true {
		return []*PostInfo{p}
	}
	if engine := Registry.EngineFor(GetSite(p.Site)); engine != nil {
		if local, ok := engine.(interface{ LocalPages(*PostInfo) []*PostInfo }); ok {
			pages := local.LocalPages(p.Clone())
			if len(pages) == p.PageCount {
				return pages
			}
		}
	}
	return nil
}

// AuthorIdentity excludes presentation and local download options. They must
// not make a view of the same creator belong to a different subscription.
func AuthorIdentity(siteKey, query string) string {
	engine := Registry.EngineFor(GetSite(siteKey))
	if engine == nil || engine.Key() != "onlyhaven" && engine.Key() != "kemono" {
		return strings.TrimSpace(query)
	}
	parts := []string{}
	for _, token := range strings.Fields(query) {
		switch token {
		case "preview-only", "extract-archives", "no-extract-archives", "delete-archives", "extract-to-artist":
			continue
		}
		parts = append(parts, token)
	}
	return strings.Join(parts, " ")
}
