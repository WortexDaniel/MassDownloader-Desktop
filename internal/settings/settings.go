// Package settings persists user settings in app.db.
package settings

import (
	"encoding/json"
	"path/filepath"
	"sync"

	"massdownloader/internal/appdir"
	"massdownloader/internal/store"
)

// Defaults mirror Python's DEFAULTS dict. A newly-added setting is available
// even against an older DB because defaults are merged in on load.
var Defaults = map[string]any{
	"feed_hide_text":           false,
	"download_root":            filepath.Join(appdir.BaseDir(), "downloads"),
	"threads":                  4,
	"confirm_download_main":    true,
	"confirm_download_author":  true,
	"max_posts":                200,
	"site":                     "e621",
	"include_tags":             []any{},
	"exclude_tags":             []any{},
	"artists":                  []any{},
	"min_score":                0,
	"score_enabled":            false,
	"score_op":                 ">=",
	"min_faves":                0,
	"faves_enabled":            false,
	"faves_op":                 ">=",
	"rating":                   "any", // any | s | q | e
	"skip_seen":                true,
	"dedup_mode":               "db", // db | folder | both
	"subfolder_by_artist":      true,
	"thumb_cache_mb":           512,   // on-disk preview cache cap (LRU)
	"cache_videos":             false, // stream viewer videos unless explicitly enabled
	"video_cache_mb":           2048,  // complete viewer videos (LRU)
	"credentials":              map[string]any{},
	"saucenao_api_key":         "",
	"reverse_min_similarity":   55,
	"filters_state":            map[string]any{},
	"rate_limits":              map[string]any{},
	"search_presets":           []any{},
	"check_authors_on_startup": false,
	"ui_motion":                "full",
	"proxy":                    map[string]any{"mode": "system", "type": "socks5", "host": "127.0.0.1", "port": 10808, "username": "", "password": ""},
}

// Settings loads and saves the settings table in app.db.
type Settings struct {
	mu     sync.RWMutex
	dbPath string
	st     *store.Store
	Data   map[string]any
}

// New loads settings from path (or APP_DIR/app.db when empty), merging
// defaults under stored values.
func New(path string) (*Settings, error) {
	if path == "" {
		path = filepath.Join(appdir.Dir(), "app.db")
	}
	st, err := store.Get(path)
	if err != nil {
		return nil, err
	}
	s := &Settings{dbPath: path, st: st, Data: map[string]any{}}
	for k, v := range Defaults {
		s.Data[k] = clone(v)
	}
	// Resolve the portable default on each load, not at package initialization.
	s.Data["download_root"] = filepath.Join(appdir.BaseDir(), "downloads")
	if err := s.load(); err != nil {
		return nil, err
	}
	if err := s.Save(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Settings) load() error {
	conn, release := s.st.Conn()
	defer release()
	rows, err := conn.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(value), &v); err != nil {
			continue
		}
		s.Data[key] = v
	}
	return rows.Err()
}

// Get returns the value for key (nil when unset everywhere).
func clone(v any) any {
	data, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}
func (s *Settings) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.Data).(map[string]any)
}
func (s *Settings) Get(key string) any { s.mu.RLock(); defer s.mu.RUnlock(); return clone(s.Data[key]) }

// GetString returns a string setting.
func (s *Settings) GetString(key string) string {
	v, _ := s.Get(key).(string)
	return v
}

// GetInt returns an int64 setting.
func (s *Settings) GetInt(key string) int64 {
	switch t := s.Get(key).(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	}
	return 0
}

// GetBool returns a bool setting.
func (s *Settings) GetBool(key string) bool {
	v, _ := s.Get(key).(bool)
	return v
}

// Set stores a value (call Save to flush).
func (s *Settings) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Data[key] = clone(value)
}

// Save flushes the whole dict in a single database transaction.
func (s *Settings) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := json.Marshal(s.Data); err != nil {
		return err
	}
	conn, release := s.st.Conn()
	defer release()
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM settings`); err != nil {
		return err
	}
	for k, v := range s.Data {
		enc, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`,
			k, string(enc)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DBPath is the backing app.db file.
func (s *Settings) DBPath() string { return s.dbPath }

// Migrate imports legacy favourites from this installation's data directory.
func Migrate() {
	appDB := filepath.Join(appdir.Dir(), "app.db")
	_ = store.MigrateFavoritesJSON(appDB,
		filepath.Join(appdir.Dir(), "favorites.json"))
}
