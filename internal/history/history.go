// Package history — download-history database, the duplicate guard.
// Port of app/history.py, schema-compatible with the Python app's history.db.
package history

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"massdownloader/internal/core"
)

// History identifies a downloaded file by its md5 (stable across sites and
// re-uploads of the same bytes) and additionally by (site, post_id) so the
// same query never re-fetches a post even if the file was deleted on disk.
// Seen() answers O(1) from an in-memory set loaded at startup; inserts go
// straight to disk so a crash loses nothing.
type History struct {
	mu     sync.Mutex
	conn   *sql.DB
	closed bool
	md5s   map[string]bool
	posts  map[string]bool // "site\x00idKey"
	// libraryRev counts catalog reference changes made by history itself.
	libraryRev atomic.Uint64
}

// Open creates/loads the database at dbPath.
func Open(dbPath string) (*History, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	conn, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	h := &History{conn: conn, md5s: map[string]bool{}, posts: map[string]bool{}}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS downloads (
			md5      TEXT NOT NULL,
			site     TEXT NOT NULL,
			post_id  INTEGER NOT NULL,
			path     TEXT NOT NULL,
			ts       INTEGER NOT NULL,
			PRIMARY KEY (site, post_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_downloads_md5 ON downloads(md5)`,
		`CREATE TABLE IF NOT EXISTS archive_outputs (site TEXT NOT NULL, post_id INTEGER NOT NULL, page_index INTEGER NOT NULL, path TEXT NOT NULL, ts INTEGER NOT NULL, PRIMARY KEY(site,post_id,page_index))`,
		`CREATE TABLE IF NOT EXISTS download_outputs (site TEXT NOT NULL, post_id INTEGER NOT NULL, page_index INTEGER NOT NULL, path TEXT NOT NULL, is_archive INTEGER NOT NULL DEFAULT 0, fingerprint TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL, PRIMARY KEY(site,post_id,page_index))`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if err := initLibrary(conn); err != nil {
		conn.Close()
		return nil, err
	}
	// SQLite lacks ADD COLUMN IF NOT EXISTS. Inspect older Python schemas.
	cols, err := conn.Query(`PRAGMA table_info(download_outputs)`)
	if err != nil {
		conn.Close()
		return nil, err
	}
	hasFingerprint := false
	for cols.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if err := cols.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			cols.Close()
			conn.Close()
			return nil, err
		}
		if name == "fingerprint" {
			hasFingerprint = true
		}
	}
	cols.Close()
	if !hasFingerprint {
		if _, err := conn.Exec(`ALTER TABLE download_outputs ADD COLUMN fingerprint TEXT NOT NULL DEFAULT ''`); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if _, err := conn.Exec(`CREATE INDEX IF NOT EXISTS idx_download_outputs_fingerprint ON download_outputs(fingerprint)`); err != nil {
		conn.Close()
		return nil, err
	}
	rows, err := conn.Query(`SELECT md5, site, post_id FROM downloads`)
	if err != nil {
		conn.Close()
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var md5Str, site string
		var pid any
		if err := rows.Scan(&md5Str, &site, &pid); err != nil {
			continue
		}
		id := core.FromIDString(asString(pid))
		if md5Str != "" {
			h.md5s[md5Str] = true
		}
		h.posts[postKey(site, id)] = true
	}
	return h, nil
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int64:
		return core.I(t).String()
	}
	return ""
}

func postKey(site string, id core.PostId) string {
	return site + "\x00" + id.Key()
}

// Seen reports whether this file or post was already downloaded. Engines
// without a content hash (Pixiv) pass md5="" — dedup then relies solely on
// (site, post_id), unique per file there.
func (h *History) Seen(md5Sum, site string, postID core.PostId) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return (md5Sum != "" && h.md5s[md5Sum]) || h.posts[postKey(site, postID)]
}

// Add records a completed download.
func (h *History) Add(md5Sum, site string, postID core.PostId, path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.conn.Exec(
		`INSERT OR REPLACE INTO downloads (md5, site, post_id, path, ts)
		 VALUES (?, ?, ?, ?, ?)`,
		md5Sum, site, postID.String(), path, time.Now().Unix())
	if err == nil {
		if md5Sum != "" {
			h.md5s[md5Sum] = true
		}
		h.posts[postKey(site, postID)] = true
	}
	return err
}

// Count returns the number of distinct posts in history.
func (h *History) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int
	_ = h.conn.QueryRow(`SELECT COUNT(*) FROM (SELECT site,post_id FROM downloads UNION SELECT site,post_id FROM download_outputs UNION SELECT site,post_id FROM archive_outputs)`).Scan(&n)
	return n
}

// ForgetAll wipes the history (the «сбросить историю» button).
func (h *History) ForgetAll() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.conn.Exec(`DELETE FROM downloads`); err != nil {
		return err
	}
	if _, err := h.conn.Exec(`DELETE FROM download_outputs`); err != nil {
		return err
	}
	if _, err := h.conn.Exec(`DELETE FROM archive_outputs`); err != nil {
		return err
	}
	h.md5s = map[string]bool{}
	h.posts = map[string]bool{}
	return nil
}

// Close releases the connection (idempotent).
func (h *History) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.conn.Close()
	h.closed = true
}
