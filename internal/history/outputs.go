package history

import (
	"massdownloader/internal/core"
	"os"
	"time"
)

type Progress struct {
	State      string `json:"state"`
	Downloaded int    `json:"downloaded"`
	Total      int    `json:"total"`
	Queued     bool   `json:"queued,omitempty"`
}
type Output struct {
	Path        string
	Archive     bool
	Fingerprint string
}

func ProgressFromParts(p *core.PostInfo, parts map[int]bool) Progress {
	total := max(1, p.PageCount)
	valid := 0
	onlyArchives := true
	for i, a := range parts {
		if i >= 0 && i < total {
			valid++
			onlyArchives = onlyArchives && a
		}
	}
	r := Progress{State: "none", Total: total, Downloaded: valid}
	if valid >= total {
		r.State = "complete"
	} else if valid > 0 {
		if onlyArchives {
			r.State = "archive_only"
		} else {
			r.State = "partial"
		}
	}
	return r
}

func (h *History) AddOutput(site string, id core.PostId, index int, path string, isArchive bool, fingerprint string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.conn.Exec(`INSERT OR REPLACE INTO download_outputs(site,post_id,page_index,path,is_archive,fingerprint,ts) VALUES(?,?,?,?,?,?,?)`, site, id.String(), index, path, isArchive, fingerprint, time.Now().Unix())
	if err != nil {
		return err
	}
	if isArchive {
		_, err = h.conn.Exec(`INSERT OR REPLACE INTO archive_outputs(site,post_id,page_index,path,ts) VALUES(?,?,?,?,?)`, site, id.String(), index, path, time.Now().Unix())
	}
	if err == nil {
		_, err = h.conn.Exec(`DELETE FROM library_refs WHERE site=? AND post_id=? AND page_index=? AND file_id NOT IN (SELECT id FROM library_files WHERE state='active' AND json_extract(data,'$.path')=? COLLATE NOCASE)`, site, id.String(), index, path)
		h.libraryRev.Add(1)
	}
	return err
}
func (h *History) OutputFor(site string, id core.PostId, index int, fingerprint string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if path, known := h.managedOutput(site, id, index, fingerprint); known {
		return path
	}
	var path, fp string
	if err := h.conn.QueryRow(`SELECT path,fingerprint FROM download_outputs WHERE site=? AND post_id=? AND page_index=?`, site, id.String(), index).Scan(&path, &fp); err == nil {
		if fp == "" || fingerprint == "" || fp == fingerprint {
			return path
		}
		return ""
	}
	_ = h.conn.QueryRow(`SELECT path FROM archive_outputs WHERE site=? AND post_id=? AND page_index=?`, site, id.String(), index).Scan(&path)
	return path
}
func (h *History) OutputsFor(site string, id core.PostId) map[int]Output {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[int]Output{}
	rows, err := h.conn.Query(`SELECT page_index,path,is_archive,fingerprint FROM download_outputs WHERE site=? AND post_id=?`, site, id.String())
	if err == nil {
		for rows.Next() {
			var i int
			var o Output
			if rows.Scan(&i, &o.Path, &o.Archive, &o.Fingerprint) == nil && o.Path != "" {
				out[i] = o
			}
		}
		rows.Close()
	}
	rows, err = h.conn.Query(`SELECT page_index,path FROM archive_outputs WHERE site=? AND post_id=?`, site, id.String())
	if err == nil {
		for rows.Next() {
			var i int
			var o Output
			o.Archive = true
			if rows.Scan(&i, &o.Path) == nil && o.Path != "" {
				if _, ok := out[i]; !ok {
					out[i] = o
				}
			}
		}
		rows.Close()
	}
	managed, err := h.conn.Query(`SELECT r.page_index,json_extract(f.data,'$.path'),f.state FROM library_refs r JOIN library_files f ON f.id=r.file_id WHERE r.site=? AND r.post_id=? AND r.page_index>=0`, site, id.String())
	if err == nil {
		defer managed.Close()
		for managed.Next() {
			var index int
			var path, state string
			if managed.Scan(&index, &path, &state) != nil {
				continue
			}
			if state != "active" {
				delete(out, index)
				continue
			}
			if info, e := os.Stat(path); e == nil && info.Mode().IsRegular() && info.Size() > 0 {
				out[index] = Output{Path: path}
			} else {
				delete(out, index)
			}
		}
	}
	return out
}
func (h *History) Progress(p *core.PostInfo) Progress {
	return ProgressFromParts(p, h.Parts(p))
}
func (h *History) PathFor(hash, site string, id core.PostId) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var path string
	_ = h.conn.QueryRow(`SELECT path FROM downloads WHERE site=? AND post_id=?`, site, id.String()).Scan(&path)
	if path == "" && hash != "" {
		_ = h.conn.QueryRow(`SELECT path FROM downloads WHERE md5=? ORDER BY ts DESC LIMIT 1`, hash).Scan(&path)
	}
	return path
}
