package downloader

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Reserve enqueue operations until filesystem/history changes are committed.
func (m *Manager) BeginLibrary() (func(), error) {
	if !m.opMu.TryLock() {
		return nil, fmt.Errorf("дождитесь завершения подготовки загрузок")
	}
	m.mu.Lock()
	busy := m.closed || !m.scanDone || m.pending > 0 || m.releasing
	m.mu.Unlock()
	if busy {
		m.opMu.Unlock()
		return nil, fmt.Errorf("дождитесь завершения загрузок перед изменением библиотеки")
	}
	return func() { m.opMu.Unlock() }, nil
}
func (m *Manager) RemapLibraryPaths(changes map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.jobs {
		if job.Status != StatusDone && job.Status != StatusSkipped {
			continue
		}
		to := ""
		changed := false
		if m.History != nil && job.Post != nil {
			if path, known := m.History.ManagedOutput(job.Post); known {
				to = path
				changed = true
			}
		}
		for old, mapped := range changes {
			if changed {
				break
			}
			if strings.EqualFold(filepath.Clean(job.ResultPath), filepath.Clean(old)) || job.ResultPath == "" && strings.EqualFold(filepath.Clean(job.Dest), filepath.Clean(old)) {
				to = mapped
				changed = true
				break
			}
		}
		if changed {
			job.LocalRevision++
			job.LocalMissing = to == ""
			job.ResultPath = to
			job.Dest = to
			if to != "" {
				job.Name = filepath.Base(to)
				job.Ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(to)), ".")
			}
		}
	}
	m.folderIndex.Invalidate()
}
