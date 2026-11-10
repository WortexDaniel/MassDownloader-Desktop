package history

import (
	"massdownloader/internal/core"
	"strings"
)

// FileOutputs matches this attachment's fingerprint. A content hash can also
// reuse records belonging to other posts/sites; hashless URLs stay post-scoped.
// Callers using folder mode must additionally check existence and the root.
func (h *History) FileOutputs(p *core.PostInfo) []Output {
	if p == nil || p.File == nil {
		return nil
	}
	fp := core.FileFingerprint(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	if path, known := h.managedOutput(p.Site, p.ID, p.PageIndex, fp); known {
		if path == "" {
			return nil
		}
		return []Output{{Path: path, Fingerprint: fp}}
	}
	out := []Output{}
	seen := map[string]bool{}
	appendRows := func(query string, args ...any) {
		rows, err := h.conn.Query(query, args...)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var o Output
			if rows.Scan(&o.Path, &o.Archive, &o.Fingerprint) == nil && o.Path != "" && !seen[o.Path] {
				out = append(out, o)
				seen[o.Path] = true
			}
		}
	}
	appendRows(`SELECT path,is_archive,fingerprint FROM download_outputs WHERE site=? AND post_id=? AND page_index=? AND (fingerprint='' OR ?='' OR fingerprint=?)`, p.Site, p.ID.String(), p.PageIndex, fp, fp)
	if core.ContentHash(p) != "" {
		appendRows(`SELECT path,is_archive,fingerprint FROM download_outputs WHERE fingerprint=? ORDER BY ts DESC`, fp)
	}
	if p.File.MD5 != "" {
		appendRows(`SELECT path,0,md5 FROM downloads WHERE md5=? ORDER BY ts DESC`, strings.ToLower(p.File.MD5))
	} else if p.PageCount <= 1 {
		appendRows(`SELECT path,0,md5 FROM downloads WHERE site=? AND post_id=?`, p.Site, p.ID.String())
	}
	if len(out) == 0 {
		appendRows(`SELECT path,1,'' FROM archive_outputs WHERE site=? AND post_id=? AND page_index=? AND NOT EXISTS (SELECT 1 FROM download_outputs WHERE site=? AND post_id=? AND page_index=?)`, p.Site, p.ID.String(), p.PageIndex, p.Site, p.ID.String(), p.PageIndex)
	}
	return out
}

// Parts never promotes an entire album because only its first file is known.
func (h *History) Parts(p *core.PostInfo) map[int]bool {
	parts := map[int]bool{}
	if p == nil || p.File == nil {
		return parts
	}
	if pages := core.LocalPostPages(p); len(pages) > 0 {
		for _, page := range pages {
			if outputs := h.FileOutputs(page); len(outputs) > 0 {
				parts[page.PageIndex] = outputs[0].Archive
			}
		}
	} else {
		for index, output := range h.OutputsFor(p.Site, p.ID) {
			if index >= 0 && index < max(1, p.PageCount) {
				page := p.Clone()
				page.PageIndex = index
				page.File = nil
				if path, known := h.ManagedOutput(page); !known || path != "" {
					parts[index] = output.Archive
				}
			}
		}
	}
	// Old databases recorded only a completed parent post, without page rows.
	if len(parts) == 0 && len(h.OutputsFor(p.Site, p.ID)) == 0 && (!h.HasManaged(p) || h.HasActiveLegacy(p)) && h.Seen("", p.Site, p.ID) {
		for index := 0; index < max(1, p.PageCount); index++ {
			parts[index] = false
		}
	}
	return parts
}
