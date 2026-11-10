package main

import (
	"context"
	"fmt"
	"log/slog"
	"massdownloader/internal/imagecache"
	"massdownloader/internal/library"
	"os"
	"path/filepath"
	"time"
)

func (a *App) libraryService() (*library.Service, error) {
	a.libraryMu.Lock()
	defer a.libraryMu.Unlock()
	if a.library != nil {
		return a.library, a.libraryError
	}
	if a.history == nil {
		return nil, fmt.Errorf("история загрузок недоступна")
	}
	s := library.New(a.history)
	s.Begin = func() (func(), error) {
		a.libraryThumbMu.Lock()
		if a.manager == nil {
			return func() { a.libraryThumbMu.Unlock() }, nil
		}
		release, err := a.manager.BeginLibrary()
		if err != nil {
			a.libraryThumbMu.Unlock()
			return nil, err
		}
		return func() { release(); a.libraryThumbMu.Unlock() }, nil
	}
	s.Changed = a.libraryChanged
	s.Notify = func(p library.Progress) {
		a.libraryMu.Lock()
		p.ID = a.libraryState.ID
		p.Kind = a.libraryState.Kind
		p.Running = a.libraryState.Running
		a.libraryState = p
		a.libraryMu.Unlock()
		a.emit("library:progress", p)
	}
	a.library = s
	a.libraryError = s.Recover()
	if a.libraryError != nil {
		slog.Error("Библиотека: ошибка восстановления", "error", a.libraryError)
	}
	return s, a.libraryError
}
func (a *App) libraryChanged(changes map[string]string) {
	if a.manager != nil {
		a.manager.RemapLibraryPaths(changes)
	}
	if a.folderIndex != nil {
		a.folderIndex.Invalidate()
	}
	a.mediaMu.Lock()
	for token, path := range a.localFiles {
		for from, to := range changes {
			if path == from {
				if to == "" {
					delete(a.localFiles, token)
				} else {
					a.localFiles[token] = to
				}
				break
			}
		}
	}
	a.mediaMu.Unlock()
	a.emit("history:changed")
	a.emit("library:changed")
}

type LibraryInfo struct {
	Roots           []string `json:"roots"`
	Count           int      `json:"count"`
	Size            int64    `json:"size"`
	Limited         int      `json:"limited"`
	Formats         []string `json:"formats"`
	Quarantine      int      `json:"quarantine"`
	QuarantineBytes int64    `json:"quarantine_bytes"`
}

func (a *App) LibraryInfo() (LibraryInfo, error) {
	s, err := a.libraryService()
	if err != nil {
		return LibraryInfo{}, err
	}
	files, err := s.Files()
	if err != nil {
		return LibraryInfo{}, err
	}
	roots := []string{}
	if list, ok := a.settings.Get("library_roots").([]any); ok {
		for _, v := range list {
			if root, ok := v.(string); ok && root != "" {
				roots = append(roots, root)
			}
		}
	}
	if len(roots) == 0 {
		roots = append(roots, a.settings.GetString("download_root"))
	}
	info := LibraryInfo{Roots: roots, Formats: []string{"jpg", "png", "webp"}}
	for _, f := range files {
		switch f.State {
		case "active":
			info.Count++
			info.Size += f.Size
			if f.Error != "" {
				info.Limited++
			}
		case "quarantined":
			info.Quarantine++
			info.QuarantineBytes += f.Size
		}
	}
	return info, nil
}

func (a *App) LibraryAuthors() (library.AuthorOverview, error) {
	s, err := a.libraryService()
	if err != nil {
		return library.AuthorOverview{}, err
	}
	return s.Authors()
}
func (a *App) LibraryAuthorCandidates(o library.CandidateOptions) ([]library.Candidate, error) {
	s, err := a.libraryService()
	if err != nil {
		return nil, err
	}
	return s.Candidates(o)
}
func (a *App) LibraryIdentities(search string) ([]library.Identity, error) {
	s, err := a.libraryService()
	if err != nil {
		return nil, err
	}
	return s.Identities(search, 40)
}
func (a *App) LibraryIgnoreAuthors(keys []string) error {
	s, err := a.libraryService()
	if err != nil {
		return err
	}
	if err = s.IgnoreAuthors(keys); err == nil {
		slog.Info("Библиотека: авторы отмечены как разные", "authors", len(keys))
	}
	return err
}

// Library preferences are limited to their own key so the library window
// cannot change unrelated settings through the bridge.
func (a *App) LibrarySetPrefs(prefs map[string]any) error {
	allowed := map[string]bool{"policy": true, "preferred": true, "candidate_mode": true, "candidate_threshold": true, "min_shared": true, "min_ratio": true, "sort": true, "dense": true}
	current, _ := a.settings.Get("library_prefs").(map[string]any)
	next := map[string]any{}
	for k, v := range current {
		next[k] = v
	}
	for k, v := range prefs {
		if !allowed[k] {
			return fmt.Errorf("неизвестная настройка библиотеки: %s", k)
		}
		next[k] = v
	}
	a.settings.Set("library_prefs", next)
	return a.settings.Save()
}
func (a *App) LibraryFiles(q library.Query) (library.FilePage, error) {
	s, err := a.libraryService()
	if err != nil {
		return library.FilePage{}, err
	}
	return s.List(q)
}
func (a *App) LibrarySelect(q library.Query) ([]string, error) {
	s, err := a.libraryService()
	if err != nil {
		return nil, err
	}
	return s.Select(q)
}
func (a *App) LibraryDuplicates(q library.Query) (library.GroupPage, error) {
	s, err := a.libraryService()
	if err != nil {
		return library.GroupPage{}, err
	}
	return s.Duplicates(q)
}
func (a *App) LibraryPreview(r library.Request) (library.Plan, error) {
	s, err := a.libraryService()
	if err != nil {
		return library.Plan{}, err
	}
	p, err := s.Preview(r)
	if err != nil {
		slog.Warn("Библиотека: план не подготовлен", "kind", r.Kind, "error", err)
	}
	return p, err
}
func (a *App) LibraryOperations() ([]library.Plan, error) {
	s, err := a.libraryService()
	if err != nil {
		return nil, err
	}
	return s.Operations()
}
func (a *App) LibraryIgnore(ids []string) error {
	s, err := a.libraryService()
	if err != nil {
		return err
	}
	err = s.Ignore(ids)
	if err == nil {
		slog.Info("Библиотека: изображения отмечены как разные", "files", len(ids))
	}
	return err
}
func (a *App) LibraryFileURL(id string) (string, error) {
	s, err := a.libraryService()
	if err != nil {
		return "", err
	}
	f, err := s.File(id)
	if err != nil {
		return "", err
	}
	if f.State != "active" {
		return "", nil
	}
	if _, err = os.Stat(f.Path); err != nil {
		return "", err
	}
	url := a.grantLocalFile(f.Path)
	if url != "" {
		url += fmt.Sprintf("&v=%d", f.Version)
	}
	return url, nil
}
func (a *App) LibraryReveal(id string) error {
	s, err := a.libraryService()
	if err != nil {
		return err
	}
	f, err := s.File(id)
	if err != nil {
		return err
	}
	if f.State != "active" {
		return fmt.Errorf("локальный файл отсутствует")
	}
	return revealPath(f.Path)
}

// Local thumbnails share the existing cache, its size limit and clear action.
func (a *App) LibraryThumbnail(id string) (string, error) {
	s, err := a.libraryService()
	if err != nil {
		return "", err
	}
	f, err := s.File(id)
	if err != nil {
		return "", err
	}
	if f.State != "active" {
		return "", nil
	}
	if f.Pixels == "" || f.Frames > 1 {
		return a.LibraryFileURL(id)
	}
	a.libraryThumbMu.Lock()
	defer a.libraryThumbMu.Unlock()
	path := filepath.Join(imagecache.CacheDir(), "library-"+f.SHA+".jpg")
	if _, err = os.Stat(path); os.IsNotExist(err) {
		ctx := a.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		tmp := path + fmt.Sprintf("-%d.part", time.Now().UnixNano())
		defer os.Remove(tmp)
		if err = library.WriteThumbnail(ctx, f.Path, tmp); err != nil {
			slog.Debug("Библиотека: используется исходное превью", "path", f.Path, "error", err)
			return a.grantLocalFile(f.Path), nil
		}
		if err = os.Rename(tmp, path); err != nil {
			return a.grantLocalFile(f.Path), nil
		}
		a.libraryThumbWrites++
		if a.libraryThumbWrites%50 == 0 {
			imagecache.Prune(a.settings.GetInt("thumb_cache_mb")*1024*1024, imagecache.CacheDir())
		}
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return a.grantLocalFile(path), nil
}
func (a *App) LibraryStatus() library.Progress {
	a.libraryMu.Lock()
	defer a.libraryMu.Unlock()
	return a.libraryState
}
func (a *App) startLibraryTask(kind string, fn func(context.Context, *library.Service) error) (string, error) {
	s, err := a.libraryService()
	if err != nil {
		return "", err
	}
	a.libraryMu.Lock()
	if a.libraryClosing || a.libraryState.Running {
		a.libraryMu.Unlock()
		return "", fmt.Errorf("операция библиотеки уже выполняется или приложение закрывается")
	}
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	id := fmt.Sprintf("library-%d", time.Now().UnixNano())
	a.libraryCancel = cancel
	a.libraryState = library.Progress{ID: id, Kind: kind, Stage: "Подготовка", Running: true}
	a.libraryWG.Add(1)
	a.libraryMu.Unlock()
	slog.Info("Библиотека: задача запущена", "task", id, "kind", kind)
	go func() {
		defer a.libraryWG.Done()
		defer cancel()
		started := time.Now()
		err := func() (err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = fmt.Errorf("внутренняя ошибка библиотеки: %v", recovered)
				}
			}()
			return fn(ctx, s)
		}()
		a.libraryMu.Lock()
		p := a.libraryState
		p.Running = false
		p.Stage = "Завершено"
		if err != nil {
			p.Error = err.Error()
			p.Stage = "Ошибка"
			if ctx.Err() != nil {
				p.Stage = "Отменено"
			}
		}
		a.libraryState = p
		a.libraryCancel = nil
		a.libraryMu.Unlock()
		if err != nil {
			slog.Warn("Библиотека: задача завершена с ошибкой", "task", id, "kind", kind, "duration", time.Since(started), "error", err)
		} else {
			slog.Info("Библиотека: задача завершена", "task", id, "kind", kind, "duration", time.Since(started))
		}
		a.emit("library:progress", p)
	}()
	return id, nil
}
func (a *App) LibraryScan(roots []string) (string, error) {
	return a.startLibraryTask("scan", func(ctx context.Context, s *library.Service) error {
		if err := s.Scan(ctx, roots); err != nil {
			return err
		}
		a.settings.Set("library_roots", roots)
		return a.settings.Save()
	})
}
func (a *App) LibraryRescan(roots []string) (string, error) {
	return a.startLibraryTask("scan", func(ctx context.Context, s *library.Service) error {
		if err := s.Scan(ctx, roots, true); err != nil {
			return err
		}
		a.settings.Set("library_roots", roots)
		return a.settings.Save()
	})
}
func (a *App) LibraryApply(id string) (string, error) {
	return a.startLibraryTask("apply", func(ctx context.Context, s *library.Service) error { return s.Apply(ctx, id) })
}
func (a *App) LibraryRestore(id string) (string, error) {
	return a.startLibraryTask("restore", func(ctx context.Context, s *library.Service) error { return s.Restore(ctx, id) })
}
func (a *App) LibraryPurge(id string) (string, error) {
	return a.startLibraryTask("purge", func(ctx context.Context, s *library.Service) error { return s.Purge(ctx, id) })
}

// LibraryPurgeAll empties the quarantine of every completed operation.
func (a *App) LibraryPurgeAll() (string, error) {
	return a.startLibraryTask("purge", func(ctx context.Context, s *library.Service) error {
		plans, err := s.Operations()
		if err != nil {
			return err
		}
		for _, p := range plans {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if (p.State == "done" || p.State == "purging") && p.HasQuarantine() {
				if err = s.Purge(ctx, p.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func (a *App) LibraryCancel() {
	a.libraryMu.Lock()
	cancel := a.libraryCancel
	a.libraryMu.Unlock()
	if cancel != nil {
		slog.Info("Библиотека: запрошена отмена")
		cancel()
	}
}
func (a *App) closeLibrary() {
	a.libraryMu.Lock()
	a.libraryClosing = true
	cancel := a.libraryCancel
	a.libraryMu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.libraryWG.Wait()
}
