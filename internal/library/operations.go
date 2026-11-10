package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"massdownloader/internal/history"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type DedupeGroup struct {
	IDs            []string `json:"ids"`
	KeepID         string   `json:"keep_id"`
	AllowDifferent bool     `json:"allow_different"`
}

type Request struct {
	Kind            string        `json:"kind"`
	IDs             []string      `json:"ids"`
	KeepID          string        `json:"keep_id"`
	AllowDifferent  bool          `json:"allow_different"`
	DedupeGroups    []DedupeGroup `json:"dedupe_groups,omitempty"`
	AllowRedownload bool          `json:"allow_redownload"`
	UseCopies       bool          `json:"use_copies"`
	// Legacy fields of the removed folder move; read only for old journals.
	TargetSite   string          `json:"target_site,omitempty"`
	TargetAuthor string          `json:"target_author,omitempty"`
	Remember     bool            `json:"remember,omitempty"`
	Conversion   Conversion      `json:"conversion"`
	Authors      *AuthorsRequest `json:"authors,omitempty"`
	Auto         *AutoDedupe     `json:"auto,omitempty"`
}

// AutoDedupe resolves every duplicate group of a query with one policy.
type AutoDedupe struct {
	Query          Query  `json:"query"`
	Policy         string `json:"policy"`
	Preferred      string `json:"preferred"`
	AllowDifferent bool   `json:"allow_different"`
}
type Change struct {
	Before File  `json:"before"`
	After  File  `json:"after"`
	Keep   *File `json:"keep,omitempty"`
	// KeepPath is where the kept file will be after the same plan moves it.
	KeepPath string `json:"keep_path,omitempty"`
	Backup   string `json:"backup,omitempty"`
	Temp     string `json:"temp,omitempty"`
}
type Step struct {
	From string `json:"from"`
	To   string `json:"to"`
	SHA  string `json:"sha256"`
}
type PathRow struct {
	Table    string `json:"table"`
	Site     string `json:"site"`
	PostID   string `json:"post_id"`
	Page     int    `json:"page_index"`
	Path     string `json:"path"`
	Expected string `json:"expected"`
}
type Route struct{ Root, Site, Author, TargetSite, TargetAuthor string }
type Snapshot struct {
	Paths  []PathRow             `json:"paths"`
	Refs   []Origin              `json:"refs"`
	Routes []Route               `json:"routes,omitempty"`
	Groups []history.AuthorGroup `json:"groups,omitempty"`
	Layout string                `json:"layout,omitempty"`
}
type Plan struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	State        string   `json:"state"`
	Request      Request  `json:"request"`
	Changes      []Change `json:"changes"`
	Steps        []Step   `json:"steps,omitempty"`
	Started      int      `json:"started"`
	Snapshot     Snapshot `json:"snapshot"`
	Created      int64    `json:"created"`
	Error        string   `json:"error,omitempty"`
	Restores     string   `json:"restores,omitempty"`
	RestoreFiles []File   `json:"restore_files,omitempty"`
	OriginalKind string   `json:"original_kind,omitempty"`
	// Groups and Layout are the author state written by an authors/layout plan.
	Groups  []history.AuthorGroup `json:"groups,omitempty"`
	Layout  string                `json:"layout,omitempty"`
	Skipped int                   `json:"skipped,omitempty"`
}

func authorKind(kind string) bool { return kind == "authors" || kind == "layout" }

// keepTarget is the final path of the kept file of a duplicate change.
func keepTarget(c Change) string {
	if c.KeepPath != "" {
		return c.KeepPath
	}
	return c.Keep.Path
}

func savePlan(db *sql.DB, p Plan) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO library_operations(id,kind,state,data,error,ts) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,data=excluded.data,error=excluded.error`, p.ID, p.Kind, p.State, string(data), p.Error, p.Created)
	return err
}
func (s *Service) storePlan(p Plan) error {
	return s.h.WithDB(func(db *sql.DB) error { return savePlan(db, p) })
}
func (s *Service) Plan(id string) (Plan, error) {
	var p Plan
	err := s.h.WithDB(func(db *sql.DB) error {
		var data string
		if err := db.QueryRow(`SELECT data FROM library_operations WHERE id=?`, id).Scan(&data); err != nil {
			return err
		}
		return json.Unmarshal([]byte(data), &p)
	})
	return p, err
}
func (s *Service) Operations() ([]Plan, error) {
	plans := []Plan{}
	err := s.h.WithDB(func(db *sql.DB) error {
		rows, e := db.Query(`SELECT data FROM library_operations ORDER BY ts DESC,rowid DESC LIMIT 100`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var data string
			if e = rows.Scan(&data); e != nil {
				return e
			}
			var p Plan
			if e = json.Unmarshal([]byte(data), &p); e != nil {
				return e
			}
			plans = append(plans, p)
		}
		return rows.Err()
	})
	return plans, err
}
func safeComponent(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 150 || strings.ContainsAny(s, "<>:\"/\\|?*\x00") || strings.TrimRight(s, ". ") != s {
		return false
	}
	for _, r := range s {
		if r < 32 {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(s, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return false
	}
	return true
}
func uniquePath(path, id string, reserved map[string]bool) (string, error) {
	base, ext := strings.TrimSuffix(path, filepath.Ext(path)), filepath.Ext(path)
	for i := 0; i < 10000; i++ {
		candidate := path
		if i > 0 {
			candidate = fmt.Sprintf("%s_lib-%s-%d%s", base, id[:8], i, ext)
		}
		if reserved[key(candidate)] {
			continue
		}
		_, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			reserved[key(candidate)] = true
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("не удалось выбрать свободное имя")
}
func quarantine(f File, op string) string {
	return filepath.Join(f.Root, ".massdownloader-trash", op, f.ID, filepath.Base(f.Path))
}
func (s *Service) Preview(r Request) (Plan, error) {
	if err := s.checkRecovery(); err != nil {
		return Plan{}, err
	}
	if authorKind(r.Kind) {
		return s.previewAuthors(r)
	}
	skipped := 0
	if r.Kind == "dedupe" && r.Auto != nil {
		if len(r.DedupeGroups) > 0 || len(r.IDs) > 0 {
			return Plan{}, fmt.Errorf("автоматический выбор нельзя смешивать с ручным")
		}
		groups, n, err := s.autoGroups(*r.Auto)
		if err != nil {
			return Plan{}, err
		}
		if len(groups) == 0 {
			if n > 0 {
				return Plan{}, fmt.Errorf("во всех группах версии различаются; разрешите замену отличающихся версий")
			}
			return Plan{}, fmt.Errorf("нет групп для автоматического разбора")
		}
		r.DedupeGroups, skipped = groups, n
	}
	if len(r.DedupeGroups) > 0 {
		if r.Kind != "dedupe" || len(r.IDs) > 0 || r.KeepID != "" || r.AllowDifferent {
			return Plan{}, fmt.Errorf("группы дублей нельзя смешивать с другим выбором файлов")
		}
		for _, group := range r.DedupeGroups {
			r.IDs = append(r.IDs, group.KeepID)
			r.IDs = append(r.IDs, group.IDs...)
			if len(r.IDs) > 10000 {
				return Plan{}, fmt.Errorf("выберите от 1 до 10000 файлов")
			}
		}
	}
	if len(r.IDs) == 0 || len(r.IDs) > 10000 {
		return Plan{}, fmt.Errorf("выберите от 1 до 10000 файлов")
	}
	if r.Kind != "delete" && r.Kind != "dedupe" && r.Kind != "convert" {
		return Plan{}, fmt.Errorf("неизвестная операция")
	}
	files, err := s.Files()
	if err != nil {
		return Plan{}, err
	}
	byID := map[string]File{}
	for _, f := range files {
		byID[f.ID] = f
	}
	// A known converted copy belongs to the same image family. Prepare an
	// explicit redirect in the reviewable plan instead of marking a post
	// missing while another chosen local representation remains available.
	selected := map[string]bool{}
	for _, id := range r.IDs {
		selected[id] = true
	}
	rootCache := map[string]string{}
	familyRoot := func(id string) string {
		if rootCache[id] != "" {
			return rootCache[id]
		}
		current := id
		visited := map[string]bool{}
		for len(visited) < 64 {
			if visited[current] {
				break
			}
			visited[current] = true
			parent := byID[current].DerivedFrom
			if parent == "" {
				break
			}
			current = parent
		}
		for child := range visited {
			rootCache[child] = current
		}
		return current
	}
	bestSHA, bestFamily := map[string]File{}, map[string]File{}
	better := func(candidate, current File) bool {
		return current.ID == "" || int64(candidate.Width)*int64(candidate.Height) > int64(current.Width)*int64(current.Height)
	}
	if r.Kind == "delete" && r.UseCopies {
		for _, f := range files {
			if selected[f.ID] || f.State != "active" {
				continue
			}
			if better(f, bestSHA[f.SHA]) {
				bestSHA[f.SHA] = f
			}
			family := familyRoot(f.ID)
			if better(f, bestFamily[family]) {
				bestFamily[family] = f
			}
		}
	}
	p := Plan{ID: newID(), Kind: r.Kind, State: "prepared", Request: r, Created: time.Now().Unix(), Changes: []Change{}, Skipped: skipped}
	dedupeKeepers := map[string]File{}
	dedupeAllowed := map[string]bool{}
	dedupeIDs := []string{}
	if r.Kind == "dedupe" {
		groups := r.DedupeGroups
		if len(groups) == 0 {
			groups = []DedupeGroup{{IDs: r.IDs, KeepID: r.KeepID, AllowDifferent: r.AllowDifferent}}
		}
		kept := map[string]bool{}
		owners := map[string]int{}
		for index, group := range groups {
			keeper, ok := byID[group.KeepID]
			if !ok || keeper.State != "active" {
				return p, fmt.Errorf("выберите сохраняемый файл для каждой группы")
			}
			kept[keeper.ID] = true
			removing := false
			for _, id := range group.IDs {
				if id == keeper.ID {
					continue
				}
				removing = true
				if owner, exists := owners[id]; exists {
					if owner != index {
						return p, fmt.Errorf("файл выбран для удаления в нескольких группах")
					}
					continue
				}
				owners[id] = index
				dedupeKeepers[id] = keeper
				dedupeAllowed[id] = group.AllowDifferent
				dedupeIDs = append(dedupeIDs, id)
			}
			if !removing {
				return p, fmt.Errorf("выберите удаляемые файлы в каждой группе")
			}
		}
		for id := range dedupeKeepers {
			if kept[id] {
				return p, fmt.Errorf("сохраняемый файл также выбран для удаления")
			}
		}
	}
	o := &p.Request.Conversion
	if r.Kind == "convert" {
		if o.Format == "jpeg" {
			o.Format = "jpg"
		}
		if o.Format != "jpg" && o.Format != "png" && o.Format != "webp" {
			return p, fmt.Errorf("доступны JPEG, PNG и WebP без потерь")
		}
		if o.Quality == 0 {
			o.Quality = 90
		}
		if o.Quality < 1 || o.Quality > 100 || o.MaxWidth < 0 || o.MaxHeight < 0 || o.MaxWidth > 50000 || o.MaxHeight > 50000 {
			return p, fmt.Errorf("недопустимые параметры конвертации")
		}
	}
	seen := map[string]bool{}
	reserved := map[string]bool{}
	for _, f := range files {
		reserved[key(f.Path)] = true
	}
	ids := r.IDs
	if r.Kind == "dedupe" {
		ids = dedupeIDs
	}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		f, ok := byID[id]
		if !ok || f.State != "active" {
			return p, fmt.Errorf("файл недоступен; обновите каталог")
		}
		c := Change{Before: f, After: f}
		if keeper, ok := dedupeKeepers[id]; ok {
			c.Keep = &keeper
		}
		if r.Kind == "delete" && r.UseCopies {
			candidate := bestSHA[f.SHA]
			if candidate.ID == "" {
				candidate = bestFamily[familyRoot(f.ID)]
			}
			if candidate.ID != "" {
				chosen := candidate
				c.Keep = &chosen
			}
		}
		c.After.Version++
		switch r.Kind {
		case "delete", "dedupe":
			c.After.Redownload = r.AllowRedownload
			if c.Keep != nil && f.SHA != c.Keep.SHA && !dedupeAllowed[id] && r.Kind == "dedupe" {
				return p, fmt.Errorf("выбранные версии различаются: подтвердите замену явно")
			}
			c.After.State = "quarantined"
			c.Backup = quarantine(f, p.ID)
		case "convert":
			if f.Frames > 1 {
				return p, fmt.Errorf("анимация не конвертируется в статическое изображение: %s", filepath.Base(f.Path))
			}
			if f.Pixels == "" {
				return p, fmt.Errorf("нет декодера для конвертации: %s", filepath.Base(f.Path))
			}
			if f.Format == "tiff" && o.Metadata {
				return p, fmt.Errorf("сохранение полного EXIF из TIFF не поддерживается; отключите сохранение метаданных")
			}
			target := strings.TrimSuffix(f.Path, filepath.Ext(f.Path)) + "." + o.Format
			c.Temp = filepath.Join(f.Root, ".massdownloader-tmp", p.ID, f.ID+"."+o.Format)
			if o.Replace {
				c.Backup = quarantine(f, p.ID)
				if !same(target, f.Path) {
					target, err = uniquePath(target, f.ID, reserved)
				}
			} else {
				if same(target, f.Path) {
					target = strings.TrimSuffix(target, filepath.Ext(target)) + "_converted." + o.Format
				}
				target, err = uniquePath(target, f.ID, reserved)
				c.After.ID = newID()
				c.After.Version = 1
				c.After.DerivedFrom = f.ID
			}
			if err != nil {
				return p, err
			}
			c.After.Path = target
			c.After.Group = ""
			c.After.Format = o.Format
			c.After.Pixels = ""
			c.After.Hash = ""
		}
		p.Changes = append(p.Changes, c)
	}
	if len(p.Changes) == 0 {
		return p, fmt.Errorf("нет файлов для изменения")
	}
	if err = s.storePlan(p); err != nil {
		return p, err
	}
	slog.Info("Библиотека: подготовлен план", "operation", p.ID, "kind", p.Kind, "files", len(p.Changes))
	return p, nil
}

func (s *Service) previewAuthors(r Request) (Plan, error) {
	c, err := s.snapshot()
	if err != nil {
		return Plan{}, err
	}
	p := Plan{ID: newID(), Kind: r.Kind, State: "prepared", Request: r, Created: time.Now().Unix(), Changes: []Change{}}
	if err = s.planAuthors(r, &p, c); err != nil {
		return p, err
	}
	if len(p.Changes) > 20000 {
		return p, fmt.Errorf("слишком много файлов для одной операции: %d", len(p.Changes))
	}
	if err = s.storePlan(p); err != nil {
		return p, err
	}
	slog.Info("Библиотека: подготовлен план", "operation", p.ID, "kind", p.Kind, "files", len(p.Changes))
	return p, nil
}

// autoGroups chooses keepers in every duplicate group matching the query.
func (s *Service) autoGroups(a AutoDedupe) ([]DedupeGroup, int, error) {
	q := a.Query
	if q.Mode == "" {
		q.Mode = "bytes"
	}
	all, err := s.duplicateGroups(q)
	if err != nil {
		return nil, 0, err
	}
	out, skipped, total := []DedupeGroup{}, 0, 0
	for _, g := range all {
		keeper := chooseKeeper(g.Files, a.Preferred, a.Policy)
		group := DedupeGroup{KeepID: keeper.ID}
		for _, f := range g.Files {
			if f.ID == keeper.ID {
				continue
			}
			group.IDs = append(group.IDs, f.ID)
			group.AllowDifferent = group.AllowDifferent || f.SHA != keeper.SHA
		}
		if group.AllowDifferent && !a.AllowDifferent {
			skipped++
			continue
		}
		total += len(group.IDs) + 1
		if total > 10000 {
			return nil, 0, fmt.Errorf("слишком много файлов: сузьте фильтр (до 10000 за операцию)")
		}
		out = append(out, group)
	}
	return out, skipped, nil
}

func moveChecked(ctx context.Context, from, to, expected string) error {
	if same(from, to) {
		return nil
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	before, err := src.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("источник не является обычным файлом")
	}
	if err = os.MkdirAll(filepath.Dir(to), 0700); err != nil {
		return err
	}
	scratch := to + ".massdownloader-moving"
	dst, err := os.OpenFile(scratch, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	published := false
	ok := false
	defer func() {
		dst.Close()
		os.Remove(scratch)
		if published && !ok {
			os.Remove(to)
		}
	}()
	if _, err = io.Copy(dst, contextReader{ctx, src}); err != nil {
		return err
	}
	if err = dst.Sync(); err != nil {
		return err
	}
	if err = dst.Close(); err != nil {
		return err
	}
	hash, err := hashFile(ctx, scratch)
	if err != nil {
		return err
	}
	if hash != expected {
		return fmt.Errorf("контрольная сумма копии не совпала")
	}
	now, err := os.Stat(from)
	if err != nil {
		return err
	}
	if !os.SameFile(before, now) || before.Size() != now.Size() || before.ModTime() != now.ModTime() {
		return fmt.Errorf("источник изменился во время копирования")
	}
	if err = os.Chtimes(scratch, before.ModTime(), before.ModTime()); err != nil {
		return err
	}
	if err = publishExclusive(scratch, to); err != nil {
		return err
	}
	published = true
	src.Close()
	if err = os.Remove(from); err != nil {
		return err
	}
	ok = true
	return nil
}
func equalFiles(ctx context.Context, a, b string) (bool, error) {
	fa, e := os.Open(a)
	if e != nil {
		return false, e
	}
	defer fa.Close()
	fb, e := os.Open(b)
	if e != nil {
		return false, e
	}
	defer fb.Close()
	aa, bb := make([]byte, 128<<10), make([]byte, 128<<10)
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		na, ea := io.ReadFull(fa, aa)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb {
			return false, nil
		}
		for i := 0; i < na; i++ {
			if aa[i] != bb[i] {
				return false, nil
			}
		}
		if ea == io.EOF || ea == io.ErrUnexpectedEOF {
			return eb == io.EOF || eb == io.ErrUnexpectedEOF, nil
		}
		if ea != nil {
			return false, ea
		}
		if eb != nil {
			return false, eb
		}
	}
}
func rollbackSteps(p Plan) error {
	var failures []error
	ctx := context.Background()
	for i := min(p.Started, len(p.Steps)) - 1; i >= 0; i-- {
		step := p.Steps[i]
		if err := validateJournalPath(p, step.From); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := validateJournalPath(p, step.To); err != nil {
			failures = append(failures, err)
			continue
		}
		_, fromErr := os.Stat(step.From)
		_, toErr := os.Stat(step.To)
		if os.IsNotExist(toErr) {
			if os.IsNotExist(fromErr) {
				failures = append(failures, fmt.Errorf("оба пути операции отсутствуют: %s и %s", step.From, step.To))
				continue
			}
			if fromErr == nil {
				hash, e := hashFile(ctx, step.From)
				if e == nil && hash == step.SHA {
					_ = os.Remove(step.To + ".massdownloader-moving")
				}
			}
			continue
		}
		if toErr != nil {
			failures = append(failures, toErr)
			continue
		}
		hash, e := hashFile(ctx, step.To)
		if e != nil || hash != step.SHA {
			failures = append(failures, fmt.Errorf("нельзя восстановить изменённый файл %s", step.To))
			continue
		}
		if fromErr == nil {
			hash, e = hashFile(ctx, step.From)
			if e == nil && hash == step.SHA {
				e = os.Remove(step.To)
			} else if e == nil {
				e = fmt.Errorf("исходный путь уже занят изменённым файлом: %s", step.From)
			}
		} else if os.IsNotExist(fromErr) {
			e = moveChecked(ctx, step.To, step.From, step.SHA)
		} else {
			e = fromErr
		}
		if e != nil {
			failures = append(failures, e)
		}
		_ = os.Remove(step.To + ".massdownloader-moving")
	}
	return errors.Join(failures...)
}

func validateJournalPath(p Plan, path string) error {
	roots := map[string]bool{}
	for _, c := range p.Changes {
		roots[c.Before.Root] = true
	}
	for _, f := range p.RestoreFiles {
		roots[f.Root] = true
	}
	for root := range roots {
		if within(root, path) {
			return validateDestination(root, path)
		}
	}
	return fmt.Errorf("путь журнала находится за пределами папок операции: %s", path)
}
func cleanupTemps(p Plan) {
	for _, c := range p.Changes {
		if c.Temp != "" {
			_ = os.Remove(c.Temp)
			_ = os.Remove(c.Temp + ".metadata")
		}
	}
}

func snapshotPaths(tx *sql.Tx, p *Plan) error {
	for _, c := range p.Changes {
		expected := c.After.Path
		if p.Kind == "delete" {
			expected = ""
		}
		if c.Keep != nil {
			expected = keepTarget(c)
		}
		if p.Kind == "convert" && !p.Request.Conversion.Replace {
			continue
		}
		for _, table := range []string{"downloads", "download_outputs", "archive_outputs"} {
			query := `SELECT site,post_id,0,path FROM downloads WHERE path=? COLLATE NOCASE`
			if table != "downloads" {
				query = `SELECT site,post_id,page_index,path FROM ` + table + ` WHERE path=? COLLATE NOCASE`
			}
			rows, err := tx.Query(query, c.Before.Path)
			if err != nil {
				return err
			}
			for rows.Next() {
				r := PathRow{Table: table, Expected: expected}
				if err = rows.Scan(&r.Site, &r.PostID, &r.Page, &r.Path); err != nil {
					rows.Close()
					return err
				}
				p.Snapshot.Paths = append(p.Snapshot.Paths, r)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		// References can be added after preview without changing file bytes.
		// Snapshot them at apply time so undo restores every affected source.
		refs, err := tx.Query(`SELECT site,post_id,page_index,fingerprint,file_id,relation FROM library_refs WHERE file_id=?`, c.Before.ID)
		if err != nil {
			return err
		}
		for refs.Next() {
			var ref Origin
			if err = refs.Scan(&ref.Site, &ref.PostID, &ref.Page, &ref.Fingerprint, &ref.FileID, &ref.Relation); err != nil {
				refs.Close()
				return err
			}
			p.Snapshot.Refs = append(p.Snapshot.Refs, ref)
		}
		err = refs.Err()
		refs.Close()
		if err != nil {
			return err
		}
	}
	if p.Kind == "move" && p.Request.Remember {
		rows, err := tx.Query(`SELECT root,site,author,target_site,target_author FROM library_routes`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r Route
			if err = rows.Scan(&r.Root, &r.Site, &r.Author, &r.TargetSite, &r.TargetAuthor); err != nil {
				rows.Close()
				return err
			}
			p.Snapshot.Routes = append(p.Snapshot.Routes, r)
		}
		err = rows.Err()
		rows.Close()
		return err
	}
	return nil
}
func checkVersion(tx *sql.Tx, f File) error {
	var data string
	if err := tx.QueryRow(`SELECT data FROM library_files WHERE id=?`, f.ID).Scan(&data); err != nil {
		return err
	}
	var current File
	if err := json.Unmarshal([]byte(data), &current); err != nil {
		return err
	}
	if current.Version != f.Version || current.State != f.State || !same(current.Path, f.Path) {
		return fmt.Errorf("каталог изменился; подготовьте новый план")
	}
	return nil
}

func (s *Service) Apply(ctx context.Context, id string) (err error) {
	if !s.work.TryLock() {
		return fmt.Errorf("операция библиотеки уже выполняется")
	}
	defer s.work.Unlock()
	defer s.invalidate()
	if err = s.checkRecovery(); err != nil {
		return err
	}
	p, err := s.Plan(id)
	if err != nil {
		return err
	}
	if p.State != "prepared" {
		return fmt.Errorf("план уже выполнен или отменён")
	}
	release := func() {}
	if s.Begin != nil {
		release, err = s.Begin()
		if err != nil {
			return err
		}
	}
	defer release()
	slog.Info("Библиотека: операция начата", "operation", p.ID, "kind", p.Kind, "files", len(p.Changes))
	defer func() {
		if err != nil && p.State == "prepared" {
			p.State = "failed"
			p.Error = err.Error()
			_ = s.storePlan(p)
			slog.Warn("Библиотека: проверка плана не пройдена", "operation", p.ID, "error", err)
		}
	}()
	for _, c := range p.Changes {
		if err = validateFile(ctx, c.Before); err != nil {
			return err
		}
		slog.Debug("Библиотека: содержимое проверено", "operation", p.ID, "path", c.Before.Path, "sha256", c.Before.SHA)
		for _, path := range []string{c.After.Path, c.Backup, c.Temp, c.KeepPath} {
			if path != "" {
				if err = validateDestination(c.Before.Root, path); err != nil {
					return err
				}
			}
		}
		if c.Keep != nil {
			if err = validateFile(ctx, *c.Keep); err != nil {
				return err
			}
			if c.Before.SHA == c.Keep.SHA {
				equal, e := equalFiles(ctx, c.Before.Path, c.Keep.Path)
				if e != nil {
					return e
				}
				if !equal {
					return fmt.Errorf("содержимое файлов различается")
				}
			}
		}
	}
	err = s.h.WithDB(func(db *sql.DB) error {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		for _, c := range p.Changes {
			if e = checkVersion(tx, c.Before); e != nil {
				return e
			}
			if c.Keep != nil {
				if e = checkVersion(tx, *c.Keep); e != nil {
					return e
				}
			}
		}
		if authorKind(p.Kind) {
			if e = checkGroups(tx, p.Snapshot.Groups, p.Snapshot.Layout); e != nil {
				return e
			}
		}
		if e = snapshotPaths(tx, &p); e != nil {
			return e
		}
		return tx.Commit()
	})
	if err != nil {
		return err
	}
	p.State = "applying"
	if err = s.storePlan(p); err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("внутренняя ошибка операции: %v", recovered)
		}
		if err == nil {
			return
		}
		slog.Warn("Библиотека: откат начат", "operation", p.ID, "error", err)
		rollbackErr := rollbackSteps(p)
		cleanupTemps(p)
		p.State = "failed"
		p.Error = err.Error()
		if rollbackErr != nil {
			p.State = "recovery_required"
			p.Error += "; восстановление: " + rollbackErr.Error()
		}
		if rollbackErr == nil {
			slog.Info("Библиотека: откат завершён", "operation", p.ID)
		}
		if e := s.storePlan(p); e != nil {
			slog.Error("Библиотека: не удалось записать ошибку", "operation", p.ID, "error", e)
		}
		slog.Error("Библиотека: операция не выполнена", "operation", p.ID, "error", p.Error)
	}()
	for i := range p.Changes {
		c := &p.Changes[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.progress(Progress{Stage: "Подготовка", Done: i, Total: len(p.Changes), Path: c.Before.Path})
		if p.Kind == "convert" {
			slog.Debug("Библиотека: конвертация файла", "operation", p.ID, "path", c.Before.Path, "format", p.Request.Conversion.Format, "replace", p.Request.Conversion.Replace)
			if err = os.MkdirAll(filepath.Dir(c.Temp), 0700); err != nil {
				return err
			}
			if err = convert(ctx, c.Before.Path, c.Temp, p.Request.Conversion); err != nil {
				return err
			}
			result := c.After
			result.Path = c.Temp
			if err = inspect(ctx, &result); err != nil {
				return err
			}
			if result.Pixels == "" {
				return fmt.Errorf("результат конвертации не прошёл проверку: %s", result.Error)
			}
			result.Path = c.After.Path
			result.Root = c.Before.Root
			c.After = result
			if c.Backup != "" {
				p.Steps = append(p.Steps, Step{c.Before.Path, c.Backup, c.Before.SHA})
			}
			p.Steps = append(p.Steps, Step{c.Temp, c.After.Path, c.After.SHA})
		} else if c.After.State == "quarantined" {
			p.Steps = append(p.Steps, Step{c.Before.Path, c.Backup, c.Before.SHA})
		} else if !same(c.Before.Path, c.After.Path) {
			p.Steps = append(p.Steps, Step{c.Before.Path, c.After.Path, c.Before.SHA})
		}
	}
	if err = s.storePlan(p); err != nil {
		return err
	}
	for i, step := range p.Steps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.Started = i + 1
		if err = s.storePlan(p); err != nil {
			return err
		}
		s.progress(Progress{Stage: "Изменение файлов", Done: i, Total: len(p.Steps), Path: step.From})
		slog.Debug("Библиотека: перенос файла", "operation", p.ID, "from", step.From, "to", step.To)
		if err = moveChecked(ctx, step.From, step.To, step.SHA); err != nil {
			p.Started = i
			return err
		}
	}
	// Stat the final paths, not a temporary encoder output. Managed files must
	// not appear externally modified immediately after a successful operation.
	for i := range p.Changes {
		c := &p.Changes[i]
		if c.After.State == "active" {
			st, e := os.Stat(c.After.Path)
			if e != nil {
				return e
			}
			c.After.Size = st.Size()
			c.After.Modified = st.ModTime().UnixNano()
		}
	}
	slog.Info("Библиотека: обновление БД", "operation", p.ID, "files", len(p.Changes))
	err = s.h.WithDB(func(db *sql.DB) error {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		for _, c := range p.Changes {
			if e = checkVersion(tx, c.Before); e != nil {
				return e
			}
			if e = saveFile(tx, c.After); e != nil {
				return e
			}
			if p.Kind == "convert" && !p.Request.Conversion.Replace {
				continue
			}
			target := c.After.Path
			relation := "original"
			if p.Kind == "delete" {
				target = ""
			}
			if p.Kind == "convert" {
				relation = "converted"
			}
			if c.Keep != nil {
				target = keepTarget(c)
				relation = "duplicate"
				if c.Before.SHA != c.Keep.SHA {
					relation = "replacement"
				}
				if _, e = tx.Exec(`UPDATE library_refs SET file_id=?,relation=? WHERE file_id=?`, c.Keep.ID, relation, c.Before.ID); e != nil {
					return e
				}
			} else if p.Kind == "convert" {
				if _, e = tx.Exec(`UPDATE library_refs SET relation=? WHERE file_id=?`, relation, c.Before.ID); e != nil {
					return e
				}
			}
			for _, table := range []string{"downloads", "download_outputs", "archive_outputs"} {
				if _, e = tx.Exec(`UPDATE `+table+` SET path=? WHERE path=? COLLATE NOCASE`, target, c.Before.Path); e != nil {
					return e
				}
			}
		}
		if authorKind(p.Kind) {
			if e = checkGroups(tx, p.Snapshot.Groups, p.Snapshot.Layout); e != nil {
				return e
			}
			layout := ""
			if p.Kind == "layout" {
				layout = p.Layout
			}
			if e = writeGroups(tx, p.Snapshot.Groups, p.Groups, layout); e != nil {
				return e
			}
		}
		p.State = "done"
		data, _ := json.Marshal(p)
		if _, e = tx.Exec(`UPDATE library_operations SET state='done',data=?,error='' WHERE id=?`, string(data), p.ID); e != nil {
			return e
		}
		return tx.Commit()
	})
	if err != nil {
		return err
	}
	cleanupTemps(p)
	changes := map[string]string{}
	for _, c := range p.Changes {
		if p.Kind == "convert" && !p.Request.Conversion.Replace {
			continue
		}
		to := c.After.Path
		if p.Kind == "delete" {
			to = ""
		}
		if c.Keep != nil {
			to = keepTarget(c)
		}
		changes[c.Before.Path] = to
	}
	slog.Info("Библиотека: операция завершена", "operation", p.ID, "kind", p.Kind, "files", len(p.Changes))
	if s.Changed != nil {
		s.Changed(changes)
	}
	return nil
}

// An unfinished filesystem mutation is rolled back on startup. A committed
// SQL operation is never rolled back merely because the process then exited.
func (s *Service) Recover() error {
	defer s.invalidate()
	plans := []Plan{}
	err := s.h.WithDB(func(db *sql.DB) error {
		rows, e := db.Query(`SELECT data FROM library_operations WHERE state IN ('applying','recovery_required')`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var data string
			if e = rows.Scan(&data); e != nil {
				return e
			}
			var p Plan
			if e = json.Unmarshal([]byte(data), &p); e != nil {
				return e
			}
			plans = append(plans, p)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, p := range plans {
		slog.Warn("Библиотека: восстановление прерванной операции", "operation", p.ID)
		if err = rollbackSteps(p); err != nil {
			return fmt.Errorf("операция %s требует восстановления: %w", p.ID, err)
		}
		cleanupTemps(p)
		p.State = "failed"
		p.Error = "прерванная операция отменена при запуске"
		if err = s.storePlan(p); err != nil {
			return err
		}
		slog.Info("Библиотека: восстановление завершено", "operation", p.ID)
	}
	return nil
}
