package downloader

import (
	"massdownloader/internal/core"
	"massdownloader/internal/history"
	"os"
	"path/filepath"
	"strings"
)

func usableOutput(path string) bool {
	st, err := os.Stat(path)
	return err == nil && (st.IsDir() || st.Mode().IsRegular() && st.Size() > 0)
}

func outputWithinRoot(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (m *Manager) existingOutput(p *core.PostInfo, cfg Config) (history.Output, bool) {
	exact := m.History.OutputFor(p.Site, p.ID, p.PageIndex, fingerprint(p))
	if usableOutput(exact) && (cfg.DedupMode != "folder" || outputWithinRoot(cfg.Root, exact)) {
		return history.Output{Path: exact}, true
	}
	if !cfg.SkipSeen {
		return history.Output{}, false
	}
	if cfg.DedupMode == "db" || cfg.DedupMode == "both" {
		if outputs := m.History.FileOutputs(p); len(outputs) > 0 {
			return outputs[0], true
		}
	}
	if cfg.DedupMode == "folder" || cfg.DedupMode == "both" {
		for _, output := range m.History.FileOutputs(p) {
			if outputWithinRoot(cfg.Root, output.Path) && usableOutput(output.Path) {
				return output, true
			}
		}
		if path := m.folderIndex.File(cfg.Root, p); path != "" {
			return history.Output{Path: path}, true
		}
	}
	return history.Output{}, false
}

func contentJobKey(job *DownloadJob) string {
	if !job.config.SkipSeen {
		return ""
	}
	hash := core.ContentHash(job.Post)
	if hash == "" {
		return ""
	}
	root, _ := filepath.Abs(job.config.Root)
	return root + "\x00" + hash
}

// Wait before taking a transfer slot, so duplicate attachments cannot block
// their source job. Failed sources release ownership to one waiting job.
func (m *Manager) reuseContentJob(job *DownloadJob) (bool, error) {
	key := contentJobKey(job)
	if key == "" {
		return false, nil
	}
	for {
		m.mu.Lock()
		ctx := m.runCtx
		source := m.contentJobs[key]
		if source == nil || source == job {
			m.contentJobs[key] = job
			m.mu.Unlock()
			return false, nil
		}
		done := source.finished
		m.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return false, core.DownloadCancelled
		}
		m.mu.Lock()
		landed := source.Status == StatusDone || source.Status == StatusSkipped
		path := source.ResultPath
		if path == "" {
			path = source.Dest
		}
		m.mu.Unlock()
		if landed && usableOutput(path) {
			m.mu.Lock()
			job.ResultPath = path
			m.mu.Unlock()
			return true, nil
		}
		m.mu.Lock()
		if m.contentJobs[key] == source {
			m.contentJobs[key] = job
		}
		m.mu.Unlock()
	}
}
