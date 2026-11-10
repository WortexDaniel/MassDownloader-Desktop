// Package library manages physical files without changing source post identity.
package library

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"massdownloader/internal/core"
	"massdownloader/internal/history"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Origin struct {
	Site        string `json:"site"`
	PostID      string `json:"post_id"`
	Page        int    `json:"page_index"`
	Fingerprint string `json:"fingerprint"`
	FileID      string `json:"file_id"`
	Relation    string `json:"relation"`
}
type File struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	Root        string   `json:"root"`
	Site        string   `json:"site"`
	Author      string   `json:"author"`
	Size        int64    `json:"size"`
	Modified    int64    `json:"modified"`
	SHA         string   `json:"sha256"`
	Pixels      string   `json:"pixels"`
	Hash        string   `json:"phash"`
	Color       []int    `json:"color,omitempty"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
	BitDepth    int      `json:"bit_depth"`
	Frames      int      `json:"frames"`
	Format      string   `json:"format"`
	Orientation int      `json:"orientation"`
	Profile     bool     `json:"profile"`
	Error       string   `json:"error,omitempty"`
	State       string   `json:"state"`
	Version     int64    `json:"version"`
	Origins     []Origin `json:"origins,omitempty"`
	DerivedFrom string   `json:"derived_from,omitempty"`
	Redownload  bool     `json:"redownload"`
	// Group is derived from the current folder on every catalog load.
	Group string `json:"group,omitempty"`
}
type Progress struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Stage   string `json:"stage"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Path    string `json:"path,omitempty"`
	Error   string `json:"error,omitempty"`
	Running bool   `json:"running"`
}
type Service struct {
	h       *history.History
	work    sync.Mutex
	Begin   func() (func(), error)
	Changed func(map[string]string)
	Notify  func(Progress)

	cacheMu sync.Mutex
	gen     uint64
	cache   *catalog
	dupes   map[string]dupeCache
}

// catalog is an immutable snapshot of all files. Callers copy File values and
// must not modify the shared slices.
type catalog struct {
	files []File
	idx   *groupIndex
	gen   uint64
	rev   uint64
}
type dupeCache struct {
	gen    uint64
	rev    uint64
	groups []Group
}

// invalidate drops cached snapshots after any catalog write.
func (s *Service) invalidate() {
	s.cacheMu.Lock()
	s.gen++
	s.cache = nil
	s.dupes = nil
	s.cacheMu.Unlock()
}

func New(h *history.History) *Service { return &Service{h: h} }
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func key(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func same(a, b string) bool { return key(a) == key(b) }
func fileData(f File) string {
	f.Origins = nil
	f.Group = ""
	data, _ := json.Marshal(f)
	return string(data)
}
func saveFile(tx *sql.Tx, f File) error {
	_, err := tx.Exec(`INSERT INTO library_files(id,path_key,state,data) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET path_key=excluded.path_key,state=excluded.state,data=excluded.data`, f.ID, key(f.Path), f.State, fileData(f))
	return err
}
func readFiles(db *sql.DB) ([]File, error) {
	rows, err := db.Query(`SELECT data FROM library_files ORDER BY path_key`)
	if err != nil {
		return nil, err
	}
	files := []File{}
	for rows.Next() {
		var data string
		if err = rows.Scan(&data); err != nil {
			break
		}
		var f File
		if err = json.Unmarshal([]byte(data), &f); err != nil {
			break
		}
		files = append(files, f)
	}
	end := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if end != nil {
		return nil, end
	}
	byID := map[string]int{}
	for i, f := range files {
		byID[f.ID] = i
	}
	refs, err := db.Query(`SELECT site,post_id,page_index,fingerprint,file_id,relation FROM library_refs`)
	if err != nil {
		return nil, err
	}
	defer refs.Close()
	for refs.Next() {
		var r Origin
		if err = refs.Scan(&r.Site, &r.PostID, &r.Page, &r.Fingerprint, &r.FileID, &r.Relation); err != nil {
			return nil, err
		}
		if i, ok := byID[r.FileID]; ok {
			files[i].Origins = append(files[i].Origins, r)
		}
	}
	return files, refs.Err()
}
func (s *Service) snapshot() (*catalog, error) {
	s.cacheMu.Lock()
	gen, rev := s.gen, s.h.LibraryRevision()
	if c := s.cache; c != nil && c.gen == gen && c.rev == rev {
		s.cacheMu.Unlock()
		return c, nil
	}
	s.cacheMu.Unlock()
	c := &catalog{gen: gen, rev: rev}
	err := s.h.WithDB(func(db *sql.DB) error {
		var err error
		if c.files, err = readFiles(db); err != nil {
			return err
		}
		c.idx, err = loadIndex(db)
		return err
	})
	if err != nil {
		return nil, err
	}
	for i := range c.files {
		c.files[i].Group = c.idx.groupOf(c.files[i])
	}
	s.cacheMu.Lock()
	if s.gen == gen {
		s.cache = c
	}
	s.cacheMu.Unlock()
	return c, nil
}
func (s *Service) Files() ([]File, error) {
	c, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	return c.files, nil
}
func (s *Service) File(id string) (File, error) {
	var f File
	err := s.h.WithDB(func(db *sql.DB) error {
		var data string
		if err := db.QueryRow(`SELECT data FROM library_files WHERE id=?`, id).Scan(&data); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			return err
		}
		rows, err := db.Query(`SELECT site,post_id,page_index,fingerprint,file_id,relation FROM library_refs WHERE file_id=?`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Origin
			if err = rows.Scan(&r.Site, &r.PostID, &r.Page, &r.Fingerprint, &r.FileID, &r.Relation); err != nil {
				return err
			}
			f.Origins = append(f.Origins, r)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		idx, err := loadIndex(db)
		if err == nil {
			f.Group = idx.groupOf(f)
		}
		return err
	})
	return f, err
}
func (s *Service) progress(p Progress) {
	if s.Notify != nil {
		s.Notify(p)
	}
}

// linkFile imports existing per-attachment history. Source metadata is never
// rewritten to describe a converted local image.
func linkFile(tx *sql.Tx, f File) error {
	if f.DerivedFrom != "" {
		return nil
	}
	rows, err := tx.Query(`SELECT site,post_id,page_index,fingerprint FROM download_outputs WHERE path=? COLLATE NOCASE`, f.Path)
	if err != nil {
		return err
	}
	refs := []Origin{}
	for rows.Next() {
		r := Origin{FileID: f.ID, Relation: "original"}
		if err = rows.Scan(&r.Site, &r.PostID, &r.Page, &r.Fingerprint); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, r)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	{
		rows, err = tx.Query(`SELECT site,post_id,md5 FROM downloads WHERE path=? COLLATE NOCASE`, f.Path)
		if err != nil {
			return err
		}
		for rows.Next() {
			r := Origin{FileID: f.ID, Relation: "original"}
			if err = rows.Scan(&r.Site, &r.PostID, &r.Fingerprint); err != nil {
				rows.Close()
				return err
			}
			// A legacy parent record does not identify an album's first page.
			r.Page = -1
			for _, part := range strings.Split(strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path)), "_") {
				if len(part) > 1 && part[0] == 'p' {
					if n, e := strconv.Atoi(part[1:]); e == nil && n >= 0 {
						r.Page = n
						break
					}
				}
			}
			matched := false
			for _, existing := range refs {
				if existing.Site == r.Site && existing.PostID == r.PostID {
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			refs = append(refs, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if len(refs) == 0 && core.GetSite(f.Site) != nil {
		stem := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		bits := strings.Split(stem, "_")
		if len(bits) > 0 && bits[0] != "" {
			r := Origin{Site: f.Site, PostID: bits[0], Page: filenamePage(f.Path), FileID: f.ID, Relation: "inferred"}
			for _, part := range bits[1:] {
				if len(part) == 32 || len(part) == 40 || len(part) == 64 {
					if _, e := hex.DecodeString(part); e == nil {
						r.Fingerprint = strings.ToLower(part)
						break
					}
				}
			}
			refs = append(refs, r)
		}
	}
	for _, r := range refs {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO library_refs(site,post_id,page_index,fingerprint,file_id,relation) VALUES(?,?,?,?,?,?)`, r.Site, r.PostID, r.Page, r.Fingerprint, r.FileID, r.Relation); err != nil {
			return err
		}
	}
	return nil
}
func filenamePage(path string) int {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	for _, p := range strings.Split(stem, "_") {
		if len(p) > 1 && p[0] == 'p' {
			if n, e := strconv.Atoi(p[1:]); e == nil && n >= 0 {
				return n
			}
		}
	}
	return 0
}

// plainLocation derives the source identity from <root>/<site>/<author>/...
func plainLocation(f *File) {
	rel, err := filepath.Rel(f.Root, f.Path)
	if err != nil {
		return
	}
	parts := strings.Split(rel, string(filepath.Separator))
	f.Site = ""
	f.Author = ""
	if len(parts) >= 2 && parts[0] != history.AuthorsDir {
		f.Site = parts[0]
		if len(parts) >= 3 {
			f.Author = parts[1]
		}
	}
}

func (s *Service) Scan(ctx context.Context, roots []string, force ...bool) error {
	if !s.work.TryLock() {
		return fmt.Errorf("операция библиотеки уже выполняется")
	}
	defer s.work.Unlock()
	defer s.invalidate()
	if err := s.checkRecovery(); err != nil {
		return err
	}
	var idx *groupIndex
	if err := s.h.WithDB(func(db *sql.DB) error { var e error; idx, e = loadIndex(db); return e }); err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("выберите папку для сканирования")
	}
	old, err := s.Files()
	if err != nil {
		return err
	}
	byPath := map[string]File{}
	for _, f := range old {
		byPath[key(f.Path)] = f
	}
	clean := []string{}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		abs, e := filepath.Abs(root)
		if e != nil {
			return e
		}
		abs, e = filepath.EvalSymlinks(abs)
		if e != nil {
			return e
		}
		info, e := os.Stat(abs)
		if e != nil || !info.IsDir() {
			return fmt.Errorf("папка недоступна: %s", abs)
		}
		duplicate := false
		for _, p := range clean {
			if same(p, abs) {
				duplicate = true
			}
		}
		if !duplicate {
			clean = append(clean, abs)
		}
	}
	if len(clean) == 0 {
		return fmt.Errorf("выберите папку для сканирования")
	}
	sort.Slice(clean, func(i, j int) bool { return len(clean[i]) < len(clean[j]) })
	type candidate struct{ path, root string }
	paths := []candidate{}
	seen := map[string]bool{}
	slog.Info("Библиотека: сканирование начато", "roots", clean)
	for _, root := range clean {
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				slog.Warn("Библиотека: папка или файл недоступны", "path", path, "error", walkErr)
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".massdownloader-") {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
			if !imageExtension(ext) || seen[key(path)] {
				return nil
			}
			real, e := filepath.EvalSymlinks(path)
			if e != nil || !within(root, real) {
				return nil
			}
			seen[key(path)] = true
			paths = append(paths, candidate{path, root})
			return nil
		})
		if err != nil {
			return err
		}
	}
	count, failed := 0, 0
	for _, c := range paths {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.progress(Progress{Stage: "Сканирование", Done: count, Total: len(paths), Path: c.path})
		f, exists := byPath[key(c.path)]
		st, e := os.Stat(c.path)
		if e != nil {
			continue
		}
		if !st.Mode().IsRegular() || st.Size() == 0 {
			continue
		}
		if !exists {
			f = File{ID: newID(), Path: c.path, Root: c.root, Version: 1}
		}
		if f.Root != c.root {
			f.Version++
			f.Root = c.root
			idx.locate(&f)
		}
		if !exists || len(force) > 0 && force[0] || f.Size != st.Size() || f.Modified != st.ModTime().UnixNano() || f.SHA == "" || f.State != "active" {
			f.Path = c.path
			f.State = "active"
			f.Version++
			idx.locate(&f)
			if e = inspect(ctx, &f); e != nil {
				return e
			}
			slog.Debug("Библиотека: файл проиндексирован", "path", f.Path, "format", f.Format, "width", f.Width, "height", f.Height, "bytes", f.Size)
			if f.Error != "" {
				failed++
				slog.Warn("Библиотека: анализ изображения ограничен", "path", f.Path, "error", f.Error)
			}
		}
		err = s.h.WithDB(func(db *sql.DB) error {
			tx, e := db.Begin()
			if e != nil {
				return e
			}
			defer tx.Rollback()
			if e = saveFile(tx, f); e != nil {
				return e
			}
			if e = linkFile(tx, f); e != nil {
				return e
			}
			// A new download inside a flat group folder names its site only in
			// the history; resolve the author once the reference is linked.
			if f.Site == "" && idx.groupOf(f) != "" {
				var site string
				if tx.QueryRow(`SELECT site FROM library_refs WHERE file_id=? ORDER BY page_index LIMIT 1`, f.ID).Scan(&site) == nil {
					f.Origins = []Origin{{Site: site}}
					idx.locate(&f)
					f.Origins = nil
					if e = saveFile(tx, f); e != nil {
						return e
					}
				}
			}
			return tx.Commit()
		})
		if err != nil {
			return err
		}
		count++
		if count%10 == 0 || count == len(paths) {
			s.progress(Progress{Stage: "Сканирование", Done: count, Total: len(paths), Path: c.path})
		}
	}
	// Missing files keep their identity and source references. An unavailable
	// root is never treated as an instruction to delete catalog records.
	err = s.h.WithDB(func(db *sql.DB) error {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		for _, f := range old {
			if f.State != "active" || seen[key(f.Path)] {
				continue
			}
			covered := false
			for _, root := range clean {
				covered = covered || within(root, f.Path)
			}
			if covered {
				if _, e = os.Stat(f.Path); os.IsNotExist(e) {
					f.State = "missing"
					f.Version++
					if e = saveFile(tx, f); e != nil {
						return e
					}
				}
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return err
	}
	slog.Info("Библиотека: сканирование завершено", "files", count, "limited", failed)
	if s.Changed != nil {
		s.Changed(nil)
	}
	return nil
}

type Query struct {
	Site      string `json:"site"`
	Author    string `json:"author"`
	Group     string `json:"group"`
	Search    string `json:"search"`
	Format    string `json:"format"`
	Sort      string `json:"sort"`
	Mode      string `json:"mode"`
	Threshold int    `json:"threshold"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}
type FilePage struct {
	Files []File `json:"files"`
	Total int    `json:"total"`
}

func filtered(files []File, q Query, idx *groupIndex) []File {
	author, search := strings.ToLower(strings.TrimSpace(q.Author)), strings.ToLower(strings.TrimSpace(q.Search))
	out := []File{}
	for _, f := range files {
		if f.State != "active" {
			continue
		}
		match := q.Site == "" || f.Site == q.Site
		for _, o := range f.Origins {
			match = match || o.Site == q.Site
		}
		if !match || q.Group != "" && f.Group != q.Group && idx.memberGroup(f.Site, f.Author) != q.Group {
			continue
		}
		if q.Format != "" && f.Format != q.Format && !(q.Format == "jpg" && f.Format == "jpeg") {
			continue
		}
		if author != "" && !strings.Contains(strings.ToLower(f.Author), author) && !strings.Contains(strings.ToLower(idx.name(f.Group)), author) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(f.Path), search) {
			continue
		}
		out = append(out, f)
	}
	switch q.Sort {
	case "newest":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Modified > out[j].Modified })
	case "largest":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	case "resolution":
		sort.SliceStable(out, func(i, j int) bool {
			return int64(out[i].Width)*int64(out[i].Height) > int64(out[j].Width)*int64(out[j].Height)
		})
	}
	return out
}
func (s *Service) filteredFiles(q Query) ([]File, error) {
	c, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	return filtered(c.files, q, c.idx), nil
}
func (s *Service) List(q Query) (FilePage, error) {
	files, err := s.filteredFiles(q)
	if err != nil {
		return FilePage{}, err
	}
	total := len(files)
	start := min(max(q.Offset, 0), total)
	limit := min(max(q.Limit, 1), 200)
	return FilePage{Files: files[start:min(total, start+limit)], Total: total}, nil
}

// Check the source immediately before a mutation. Modification timestamps
// alone do not authorize applying a previously prepared plan.
func validateFile(ctx context.Context, f File) error {
	if f.State != "active" {
		return fmt.Errorf("файл недоступен: %s", f.Path)
	}
	real, err := filepath.EvalSymlinks(f.Path)
	if err != nil {
		return err
	}
	if !within(f.Root, real) {
		return fmt.Errorf("файл находится за пределами папки библиотеки")
	}
	st, err := os.Lstat(f.Path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != f.Size || st.ModTime().UnixNano() != f.Modified {
		return fmt.Errorf("файл изменился после сканирования: %s", f.Path)
	}
	hash, err := hashFile(ctx, f.Path)
	if err != nil {
		return err
	}
	if hash != f.SHA {
		return fmt.Errorf("содержимое файла изменилось: %s", f.Path)
	}
	return nil
}

// Check the nearest existing parent as well: a junction inside the library
// must not redirect a planned destination outside the selected root.
func validateDestination(root, path string) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("путь занят специальным файлом: %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !within(root, path) {
		return fmt.Errorf("путь находится за пределами библиотеки: %s", path)
	}
	parent := filepath.Dir(path)
	for {
		real, err := filepath.EvalSymlinks(parent)
		if err == nil {
			if !within(root, real) {
				return fmt.Errorf("папка назначения ссылается за пределы библиотеки: %s", path)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return err
		}
		parent = next
	}
}

func (s *Service) checkRecovery() error {
	return s.h.WithDB(func(db *sql.DB) error {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM library_operations WHERE state IN ('applying','recovery_required')`).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("есть незавершённая операция: перезапустите приложение для восстановления")
		}
		return nil
	})
}

func (s *Service) Ignore(ids []string) error {
	defer s.invalidate()
	if len(ids) < 2 {
		return fmt.Errorf("выберите минимум два файла")
	}
	if len(ids) > 200 {
		return fmt.Errorf("слишком много файлов")
	}
	return s.h.WithDB(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for i, first := range ids {
			for _, second := range ids[i+1:] {
				a, b := first, second
				if a > b {
					a, b = b, a
				}
				if _, err = tx.Exec(`INSERT OR IGNORE INTO library_ignored(a,b) VALUES(?,?)`, a, b); err != nil {
					return err
				}
			}
		}
		return tx.Commit()
	})
}

func (s *Service) Select(q Query) ([]string, error) {
	files, err := s.filteredFiles(q)
	if err != nil {
		return nil, err
	}
	if len(files) > 10000 {
		return nil, fmt.Errorf("сузьте фильтр: за одну операцию можно выбрать до 10000 файлов")
	}
	ids := make([]string, 0, len(files))
	for _, f := range files {
		ids = append(ids, f.ID)
	}
	return ids, nil
}
