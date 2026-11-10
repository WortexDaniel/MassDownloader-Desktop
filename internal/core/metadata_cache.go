package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type metadataEntry struct {
	data          []byte
	expires, used time.Time
}
type metadataFlight struct {
	done   chan struct{}
	cancel context.CancelFunc
	users  int
	data   []byte
	err    error
}
type MetadataStats struct {
	Hits, Misses, Shared, Cancelled uint64
	Entries, Bytes                  int
}

// MetadataCache owns bounded immutable byte snapshots. Every caller gets its
// own copy; one caller's cancellation cannot cancel another caller's work.
type MetadataCache struct {
	mu         sync.Mutex
	entries    map[string]metadataEntry
	flights    map[string]*metadataFlight
	active     map[*metadataFlight]bool
	bytes      int
	generation uint64
	closed     bool
	stats      MetadataStats
}

func NewMetadataCache() *MetadataCache {
	return &MetadataCache{entries: map[string]metadataEntry{}, flights: map[string]*metadataFlight{}, active: map[*metadataFlight]bool{}}
}
func (m *MetadataCache) Clear() {
	slog.Debug("Кэш метаданных: очистка")
	m.mu.Lock()
	m.entries = map[string]metadataEntry{}
	m.flights = map[string]*metadataFlight{} // refresh must not join an older generation
	m.bytes = 0
	m.generation++
	m.mu.Unlock()
}
func (m *MetadataCache) Close() {
	m.mu.Lock()
	m.closed = true
	for f := range m.active {
		f.cancel()
	}
	m.entries = map[string]metadataEntry{}
	m.bytes = 0
	m.generation++
	m.mu.Unlock()
}
func (m *MetadataCache) Stats() MetadataStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stats
	s.Entries = len(m.entries)
	s.Bytes = m.bytes
	return s
}
func (m *MetadataCache) Fetch(ctx context.Context, key string, ttl time.Duration, load func(context.Context) ([]byte, error)) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, DownloadCancelled
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, DownloadCancelled
	}
	now := time.Now()
	if entry, ok := m.entries[key]; ok {
		if now.Before(entry.expires) {
			entry.used = now
			m.entries[key] = entry
			m.stats.Hits++
			data := append([]byte(nil), entry.data...)
			m.mu.Unlock()
			return data, nil
		}
		m.bytes -= len(entry.data)
		delete(m.entries, key)
	}
	f := m.flights[key]
	if f != nil {
		f.users++
		m.stats.Shared++
	} else {
		workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &metadataFlight{done: make(chan struct{}), cancel: cancel, users: 1}
		m.flights[key] = f
		m.active[f] = true
		m.stats.Misses++
		generation := m.generation
		go func() {
			data, err := load(workCtx)
			m.mu.Lock()
			f.data = append([]byte(nil), data...)
			f.err = err
			delete(m.active, f)
			if m.flights[key] == f {
				delete(m.flights, key)
			}
			if err == nil && ttl > 0 && len(data) > 0 && len(data) <= 8<<20 && workCtx.Err() == nil && !m.closed && generation == m.generation {
				for k, e := range m.entries {
					if !time.Now().Before(e.expires) {
						m.bytes -= len(e.data)
						delete(m.entries, k)
					}
				}
				for len(m.entries) >= 512 || m.bytes+len(data) > 32<<20 {
					oldest := ""
					var used time.Time
					for k, e := range m.entries {
						if oldest == "" || e.used.Before(used) {
							oldest = k
							used = e.used
						}
					}
					if oldest == "" {
						break
					}
					m.bytes -= len(m.entries[oldest].data)
					delete(m.entries, oldest)
				}
				m.entries[key] = metadataEntry{data: append([]byte(nil), data...), expires: time.Now().Add(ttl), used: time.Now()}
				m.bytes += len(data)
			}
			close(f.done)
			m.mu.Unlock()
			cancel()
		}()
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		m.mu.Lock()
		f.users--
		m.stats.Cancelled++
		if f.users == 0 {
			if m.flights[key] == f {
				delete(m.flights, key)
			}
			f.cancel()
		}
		m.mu.Unlock()
		return nil, DownloadCancelled
	case <-f.done:
		if ctx.Err() != nil {
			return nil, DownloadCancelled
		}
		return append([]byte(nil), f.data...), f.err
	}
}

func metadataTTL(method string, raw string) time.Duration {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	p := u.Path
	if method == http.MethodPost {
		switch p {
		case "/api/notes/show", "/api/users/show":
			return 5 * time.Minute
		case "/api/notes/search", "/api/users/notes", "/api/users/search", "/api/hashtags/search":
			return 20 * time.Second
		}
		if strings.HasSuffix(p, "/SearchTimeline") {
			return 20 * time.Second
		}
		return 0
	}
	if method != http.MethodGet {
		return 0
	}
	if strings.Contains(p, "/auth/") || strings.Contains(p, "watch") {
		return 0
	}
	if strings.Contains(p, "autocomplete") || strings.Contains(p, "/tags/") || p == "/tag.json" || p == "/tags.json" {
		return 5 * time.Minute
	}
	if p == "/rpc/cps.php" || p == "/ajax/search/users" || p == "/index.php" && u.Query().Get("page") == "autocomplete2" || strings.HasSuffix(p, "/profile/illusts") {
		return 5 * time.Minute
	}
	if strings.HasSuffix(p, "/profile/all") {
		return 10 * time.Minute
	}
	if strings.HasSuffix(p, "/creators") {
		return 15 * time.Minute
	}
	if strings.Contains(p, "/media_assets/") || strings.Contains(p, "/ajax/illust/") {
		return 5 * time.Minute
	}
	if strings.HasSuffix(p, ".json") || strings.Contains(p, "/ajax/search/") || strings.HasPrefix(p, "/api/v1/") {
		return 20 * time.Second
	}
	if p == "/index.php" && u.Query().Get("page") == "dapi" && u.Query().Get("s") == "post" {
		return 20 * time.Second
	}
	if p == "/autocomplete.php" {
		return 5 * time.Minute
	}
	return 0
}
func (c *ApiClient) metadataKey(method, raw string, body []byte, headers map[string]string) string {
	u, err := url.Parse(raw)
	if err == nil {
		u.RawQuery = u.Query().Encode()
		raw = u.String()
	}
	hdr := http.Header{}
	for k, v := range headers {
		hdr.Set(k, v)
	}
	hdr.Set("User-Agent", c.UserAgent)
	b, _ := json.Marshal([]any{method, raw, body, hdr, c.AuthIdentity()})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type cachedHTTPResponse struct {
	Data   []byte
	Header http.Header
	Status int
}

var errMetadataUncacheable = errors.New("uncacheable metadata")

func validMetadata(data []byte) bool {
	if !json.Valid(data) {
		return false
	}
	var v map[string]json.RawMessage
	if json.Unmarshal(data, &v) == nil && v != nil {
		if errFlag := strings.TrimSpace(string(v["error"])); errFlag != "" && errFlag != "false" && errFlag != "null" && errFlag != "\"\"" {
			return false
		}
		if string(v["success"]) == "false" {
			return false
		}
	}
	return true
}

// Known response schemas are checked before retaining a successful HTTP body.
// Keep the original body available to the engine's diagnostic parser on failure.
func validMetadataResponse(raw string, data []byte) bool {
	if !validMetadata(data) {
		return false
	}
	var value any
	if DecodeJSON(data, &value) != nil || value == nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	p := u.Path
	object, _ := value.(map[string]any)
	if strings.HasSuffix(p, "/profile/all") {
		body, ok := object["body"].(map[string]any)
		return ok && (body["illusts"] != nil || body["manga"] != nil)
	}
	if strings.HasSuffix(p, "/profile/illusts") {
		body, _ := object["body"].(map[string]any)
		_, ok := body["works"].(map[string]any)
		return ok
	}
	if strings.Contains(p, "/ajax/illust/") {
		if strings.HasSuffix(p, "/pages") {
			pages, ok := object["body"].([]any)
			if !ok || len(pages) == 0 {
				return false
			}
			for _, page := range pages {
				entry, _ := page.(map[string]any)
				urls, _ := entry["urls"].(map[string]any)
				if Str_(urls["original"]) == "" {
					return false
				}
			}
			return true
		}
		_, ok := object["body"].(map[string]any)
		return ok
	}
	if strings.HasPrefix(p, "/ajax/search/artworks/") {
		body, _ := object["body"].(map[string]any)
		im, _ := body["illustManga"].(map[string]any)
		_, ok := im["data"].([]any)
		return ok
	}
	if p == "/posts.json" || strings.HasPrefix(p, "/api/v1/") && strings.HasSuffix(p, "/posts") {
		if object != nil {
			value = object["posts"]
		}
		rows, ok := value.([]any)
		if !ok {
			return false
		}
		for _, row := range rows {
			if _, ok := row.(map[string]any); !ok {
				return false
			}
		}
		return true
	}
	return true
}
