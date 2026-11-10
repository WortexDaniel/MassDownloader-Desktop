package history

import (
	"database/sql"
	"massdownloader/internal/core"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Catalog and history deliberately share a database: changing the physical
// file and all its source references is one SQL transaction.
func initLibrary(conn *sql.DB) error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS library_files (id TEXT PRIMARY KEY, path_key TEXT UNIQUE NOT NULL, state TEXT NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS library_refs (site TEXT NOT NULL, post_id TEXT NOT NULL, page_index INTEGER NOT NULL, fingerprint TEXT NOT NULL DEFAULT '', file_id TEXT NOT NULL, relation TEXT NOT NULL DEFAULT 'original', PRIMARY KEY(site,post_id,page_index))`,
		`CREATE INDEX IF NOT EXISTS library_refs_file ON library_refs(file_id)`,
		`CREATE TABLE IF NOT EXISTS library_operations (id TEXT PRIMARY KEY, kind TEXT NOT NULL, state TEXT NOT NULL, data TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS library_ignored (a TEXT NOT NULL, b TEXT NOT NULL, PRIMARY KEY(a,b))`,
		`CREATE TABLE IF NOT EXISTS library_routes (root TEXT NOT NULL, site TEXT NOT NULL, author TEXT NOT NULL, target_site TEXT NOT NULL, target_author TEXT NOT NULL, PRIMARY KEY(root,site,author))`,
		`CREATE TABLE IF NOT EXISTS library_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS library_author_groups (id TEXT PRIMARY KEY, name TEXT NOT NULL, folder TEXT NOT NULL, primary_site TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS library_author_members (group_id TEXT NOT NULL, site TEXT NOT NULL, author TEXT NOT NULL, PRIMARY KEY(site,author))`,
		`CREATE INDEX IF NOT EXISTS library_author_members_group ON library_author_members(group_id)`,
		`CREATE TABLE IF NOT EXISTS library_author_ignored (a TEXT NOT NULL, b TEXT NOT NULL, PRIMARY KEY(a,b))`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// WithDB serializes catalog transactions with the history cache. Callbacks
// must not reenter History methods. They must not delete source identities.
func (h *History) WithDB(fn func(*sql.DB) error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fn(h.conn)
}

func (h *History) managedOutput(site string, id core.PostId, index int, fingerprint string) (string, bool) {
	var path, state, fp string
	err := h.conn.QueryRow(`SELECT json_extract(f.data,'$.path'),f.state,r.fingerprint FROM library_refs r JOIN library_files f ON f.id=r.file_id WHERE r.site=? AND r.post_id=? AND r.page_index=?`, site, id.String(), index).Scan(&path, &state, &fp)
	if err != nil {
		return "", false
	}
	if state != "active" || fp != "" && fingerprint != "" && fp != fingerprint {
		return "", true
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return "", true
	}
	return path, true
}

// ManagedOutput also returns known=true for a deliberately removed attachment.
// Callers must not fall back to a stale filename in that case.
func (h *History) ManagedOutput(p *core.PostInfo) (string, bool) {
	if p == nil {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.managedOutput(p.Site, p.ID, p.PageIndex, core.FileFingerprint(p))
}

// AuthorsDir holds merged author folders for the flat and by-site layouts.
const AuthorsDir = "_authors"

// Author folder layouts. The layout is stored with the catalog so that the
// downloader and a layout migration always agree on the same value.
const (
	LayoutFlat    = "flat"
	LayoutBySite  = "by_site"
	LayoutPrimary = "primary"
)

type AuthorMember struct {
	Site   string `json:"site"`
	Author string `json:"author"`
}

// Key identifies a member independently of the folder it is stored in.
func (m AuthorMember) Key() string { return m.Site + "\x00" + m.Author }

type AuthorGroup struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Folder      string         `json:"folder"`
	PrimarySite string         `json:"primary_site"`
	Created     int64          `json:"created"`
	Members     []AuthorMember `json:"members"`
}

type Queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func ValidLayout(layout string) bool {
	return layout == LayoutFlat || layout == LayoutBySite || layout == LayoutPrimary
}

// GroupDir is the folder of a group's files. by_site appends the source site;
// an unknown site keeps the file directly in the group folder.
func GroupDir(root, layout string, g AuthorGroup, site string) string {
	switch layout {
	case LayoutBySite:
		return filepath.Join(root, AuthorsDir, g.Folder, site)
	case LayoutPrimary:
		return filepath.Join(root, g.PrimarySite, g.Folder)
	default:
		return filepath.Join(root, AuthorsDir, g.Folder)
	}
}

func LibraryMeta(q Queryer, key, fallback string) string {
	var value string
	if q.QueryRow(`SELECT value FROM library_meta WHERE key=?`, key).Scan(&value) != nil || value == "" {
		return fallback
	}
	return value
}

func AuthorLayout(q Queryer) string {
	layout := LibraryMeta(q, "author_layout", LayoutFlat)
	if !ValidLayout(layout) {
		return LayoutFlat
	}
	return layout
}

func LoadAuthorGroups(q Queryer) ([]AuthorGroup, error) {
	rows, err := q.Query(`SELECT id,name,folder,primary_site,created FROM library_author_groups ORDER BY name COLLATE NOCASE,id`)
	if err != nil {
		return nil, err
	}
	groups := []AuthorGroup{}
	index := map[string]int{}
	for rows.Next() {
		var g AuthorGroup
		if err = rows.Scan(&g.ID, &g.Name, &g.Folder, &g.PrimarySite, &g.Created); err != nil {
			rows.Close()
			return nil, err
		}
		g.Members = []AuthorMember{}
		index[g.ID] = len(groups)
		groups = append(groups, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = q.Query(`SELECT group_id,site,author FROM library_author_members`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var m AuthorMember
		if err = rows.Scan(&id, &m.Site, &m.Author); err != nil {
			return nil, err
		}
		if i, ok := index[id]; ok {
			groups[i].Members = append(groups[i].Members, m)
		}
	}
	for i := range groups {
		sort.Slice(groups[i].Members, func(a, b int) bool {
			x, y := groups[i].Members[a], groups[i].Members[b]
			if x.Site != y.Site {
				return x.Site < y.Site
			}
			return strings.ToLower(x.Author) < strings.ToLower(y.Author)
		})
	}
	return groups, rows.Err()
}

// AuthorFolder returns the storage folder of a merged author. A subscription's
// site and query are never changed; only where its files are written.
func (h *History) AuthorFolder(root, site, author string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var g AuthorGroup
	err := h.conn.QueryRow(`SELECT g.id,g.name,g.folder,g.primary_site FROM library_author_members m JOIN library_author_groups g ON g.id=m.group_id WHERE m.site=? AND m.author=?`, site, author).Scan(&g.ID, &g.Name, &g.Folder, &g.PrimarySite)
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return GroupDir(filepath.Clean(abs), AuthorLayout(h.conn), g, site), true
}

// LibraryRevision changes whenever history alters catalog references outside
// the library service, so cached catalog snapshots can be invalidated.
func (h *History) LibraryRevision() uint64 { return h.libraryRev.Load() }

func (h *History) hasManaged(site string, id core.PostId) bool {
	var n int
	_ = h.conn.QueryRow(`SELECT COUNT(*) FROM library_refs WHERE site=? AND post_id=?`, site, id.String()).Scan(&n)
	return n > 0
}

func (h *History) HasManaged(p *core.PostInfo) bool {
	if p == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hasManaged(p.Site, p.ID)
}

func (h *History) HasActiveLegacy(p *core.PostInfo) bool {
	if p == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var path string
	if h.conn.QueryRow(`SELECT json_extract(f.data,'$.path') FROM library_refs r JOIN library_files f ON f.id=r.file_id WHERE r.site=? AND r.post_id=? AND r.page_index=-1 AND f.state='active'`, p.Site, p.ID.String()).Scan(&path) != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// A manual download can override this rule; automatic scans remember removals.
func (h *History) SkipDeleted(p *core.PostInfo) bool {
	if p == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var state, fp string
	var redownload int
	err := h.conn.QueryRow(`SELECT f.state,r.fingerprint,COALESCE(json_extract(f.data,'$.redownload'),0) FROM library_refs r JOIN library_files f ON f.id=r.file_id WHERE r.site=? AND r.post_id=? AND r.page_index IN (?,-1) ORDER BY r.page_index DESC LIMIT 1`, p.Site, p.ID.String(), p.PageIndex).Scan(&state, &fp, &redownload)
	return err == nil && (state == "quarantined" || state == "deleted") && redownload == 0 && (fp == "" || fp == core.FileFingerprint(p))
}
