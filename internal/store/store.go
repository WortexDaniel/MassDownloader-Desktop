// Package store — shared SQLite store for user state (settings + favourites,
// app.db). Port of app/store.py.
//
// The app keeps its on-disk state in data/ as three distinct things:
//
//   - history.db — the download-dedup guard (internal/history). Its own file
//     on purpose: a hot path written on every download.
//   - app.db     — settings and favourites.
//   - thumbs/    — the LRU image cache (internal/imagecache).
//
// Connections are cached per resolved path so the many short-lived Settings
// instances share one handle. Each connection is guarded by its own mutex.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"massdownloader/internal/core"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is one SQLite connection to app.db plus the mutex serialising access
// to it, shared by every Settings/Favorites pointing at the same file.
type Store struct {
	Path string
	mu   sync.Mutex
	conn *sql.DB
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*Store{}
)

// Get returns the shared Store for path, opening it once.
func Get(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if st, ok := cache[abs]; ok {
		return st, nil
	}
	st, err := open(abs)
	if err != nil {
		return nil, err
	}
	cache[abs] = st
	return st, nil
}

// CloseAll closes and forgets every cached connection — the explicit boundary
// tests need to remove a temporary data dir reliably on Windows.
func CloseAll() {
	cacheMu.Lock()
	stores := make([]*Store, 0, len(cache))
	for _, st := range cache {
		stores = append(stores, st)
	}
	cache = map[string]*Store{}
	cacheMu.Unlock()
	for _, st := range stores {
		st.mu.Lock()
		st.conn.Close()
		st.mu.Unlock()
	}
}

func open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// modernc/sqlite wants its own driver options in the DSN; WAL + busy
	// timeout are set as pragmas below (mirrors Python's connect args).
	conn, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	st := &Store{Path: path, conn: conn}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS fav_posts (
			site     TEXT    NOT NULL,
			post_id  INTEGER NOT NULL,
			saved_at INTEGER NOT NULL,
			data     TEXT    NOT NULL,
			PRIMARY KEY (site, post_id)
		)`,
		`CREATE TABLE IF NOT EXISTS fav_authors (
			site     TEXT    NOT NULL,
			query    TEXT    NOT NULL,
			name     TEXT    NOT NULL,
			saved_at INTEGER NOT NULL,
			state    TEXT    NOT NULL DEFAULT '{}',
			PRIMARY KEY (site, query)
		)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			conn.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	// fav_authors.state was added after favourites shipped; CREATE TABLE IF
	// NOT EXISTS does not add columns, so migrate explicitly.
	rows, err := conn.Query(`PRAGMA table_info(fav_authors)`)
	if err != nil {
		conn.Close()
		return nil, err
	}
	hasState := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil &&
			name == "state" {
			hasState = true
		}
	}
	rows.Close()
	if !hasState {
		if _, err := conn.Exec(
			`ALTER TABLE fav_authors ADD COLUMN state TEXT NOT NULL DEFAULT '{}'`); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return st, nil
}

// Conn returns the raw handle while holding the lock until release is called.
func (s *Store) Conn() (*sql.DB, func()) {
	s.mu.Lock()
	return s.conn, s.mu.Unlock
}

// -- JSON migration ----------------------------------------------------------

func hasRows(conn *sql.DB, table string) bool {
	row := conn.QueryRow(fmt.Sprintf(`SELECT 1 FROM %s LIMIT 1`, table))
	var one int
	return row.Scan(&one) == nil
}

// retire renames a successfully imported JSON file to <name>.migrated so it
// stays as a backup but is never read again. Best-effort.
func retire(path string) {
	_ = os.Rename(path, path+".migrated")
}

func importFavorites(conn *sql.DB, path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var obj struct {
		Posts   []map[string]any `json:"posts"`
		Authors []map[string]any `json:"authors"`
	}
	if err := core.DecodeJSON(data, &obj); err != nil {
		return false
	}
	for _, d := range obj.Posts {
		site, _ := d["site"].(string)
		pid := NormalizePostIDAny(d["id"])
		if site == "" || pid == "" {
			continue
		}
		savedAt := int64(0)
		switch t := d["saved_at"].(type) {
		case json.Number:
			savedAt = core.AsInt(t)
		case float64:
			savedAt = int64(t)
		}
		enc, _ := json.Marshal(d)
		conn.Exec(`INSERT OR REPLACE INTO fav_posts
			(site, post_id, saved_at, data) VALUES (?, ?, ?, ?)`,
			site, pid, savedAt, string(enc))
	}
	for _, d := range obj.Authors {
		q, _ := d["query"].(string)
		s, _ := d["site"].(string)
		name, _ := d["name"].(string)
		savedAt := int64(0)
		switch t := d["saved_at"].(type) {
		case json.Number:
			savedAt = core.AsInt(t)
		case float64:
			savedAt = int64(t)
		}
		if q == "" || s == "" {
			continue
		}
		conn.Exec(`INSERT OR REPLACE INTO fav_authors
			(site, query, name, saved_at) VALUES (?, ?, ?, ?)`,
			s, q, name, savedAt)
	}
	return true
}

// MigrateFavoritesJSON imports legacy favourites into empty app.db tables
// and retires favorites.json to a .migrated backup.
func MigrateFavoritesJSON(dbPath, favoritesJSON string) error {
	st, err := Get(dbPath)
	if err != nil {
		return err
	}
	conn, release := st.Conn()
	defer release()

	if favoritesJSON != "" {
		if _, err := os.Stat(favoritesJSON); err == nil {
			if hasRows(conn, "fav_posts") || hasRows(conn, "fav_authors") {
				retire(favoritesJSON)
			} else if importFavorites(conn, favoritesJSON) {
				retire(favoritesJSON)
			}
		}
	}
	return nil
}

// NormalizePostIDAny converts an any-typed persisted id into the canonical
// storage key (decimal strings -> int text, everything else verbatim).
func NormalizePostIDAny(v any) string {
	switch t := v.(type) {
	case float64:
		return fmt.Sprintf("%d", int64(t))
	case string:
		return normalizeIDText(t)
	case json.Number:
		return t.String()
	}
	return ""
}

func normalizeIDText(s string) string {
	trim := trimSpaces(s)
	if trim == "" {
		return ""
	}
	allDec := true
	for _, r := range trim {
		if r < '0' || r > '9' {
			allDec = false
			break
		}
	}
	if allDec {
		return trim // canonical int form (leading zeros collapse)
	}
	return trim
}

func trimSpaces(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' ||
		s[start] == '\n' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' ||
		s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// NowSeconds is time.time() for saved_at/checked_at columns.
func NowSeconds() int64 { return time.Now().Unix() }
