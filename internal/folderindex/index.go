package folderindex

import (
	"massdownloader/internal/archive"
	"massdownloader/internal/core"
	"massdownloader/internal/history"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var hashName = regexp.MustCompile(`_([0-9a-fA-F]{64}|[0-9a-fA-F]{40}|[0-9a-fA-F]{32})(?:_|\.)`)
var extracted = regexp.MustCompile(`_extracted(?:_\d+)?$`)

type snapshot struct {
	hashes map[string][]string
	files  []string
}
type Index struct {
	mu    sync.Mutex
	root  string
	sites map[string]snapshot
}

func New() *Index            { return &Index{sites: map[string]snapshot{}} }
func (i *Index) Invalidate() { i.mu.Lock(); defer i.mu.Unlock(); i.sites = map[string]snapshot{} }
func (i *Index) forSite(root, site string) snapshot {
	i.mu.Lock()
	defer i.mu.Unlock()
	if root != i.root {
		i.root = root
		i.sites = map[string]snapshot{}
	}
	if s, ok := i.sites[site]; ok {
		return s
	}
	// Content hashes are global within the selected root; post IDs are scoped
	// to their site. One walk supplies both the downloader and UI lookups.
	hashes := map[string][]string{}
	files := map[string][]string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			if extracted.MatchString(info.Name()) || strings.HasPrefix(info.Name(), ".massdownloader-") {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || strings.HasSuffix(strings.ToLower(info.Name()), ".part") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		key, _, ok := strings.Cut(rel, string(filepath.Separator))
		if ok {
			files[key] = append(files[key], p)
		}
		if m := hashName.FindStringSubmatch(info.Name()); m != nil {
			hash := strings.ToLower(m[1])
			hashes[hash] = append(hashes[hash], p)
		}
		return nil
	})
	for key, paths := range files {
		i.sites[key] = snapshot{hashes: hashes, files: paths}
	}
	i.sites[site] = snapshot{hashes: hashes, files: files[site]}
	return i.sites[site]
}

func existingFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// File finds one attachment, including an identical file from another post,
// page or site. The hash represents the original content, not the full name.
func (i *Index) File(root string, p *core.PostInfo) string {
	if p == nil || p.File == nil {
		return ""
	}
	s := i.forSite(root, p.Site)
	if p.File.MD5 != "" {
		for _, path := range s.hashes[strings.ToLower(p.File.MD5)] {
			if existingFile(path) {
				return path
			}
		}
	}
	id := regexp.QuoteMeta(p.ID.String())
	pattern := `^` + id + `(?:_([0-9a-fA-F]{64}|[0-9a-fA-F]{40}|[0-9a-fA-F]{32}))?`
	if p.PageCount > 1 {
		pattern += `_p` + core.I(int64(p.PageIndex)).String()
	}
	prefix := regexp.MustCompile(pattern + `(?:_|\.)`)
	for _, path := range s.files {
		match := prefix.FindStringSubmatch(filepath.Base(path))
		if match == nil || p.File.MD5 != "" && match[1] != "" && !strings.EqualFold(match[1], p.File.MD5) {
			continue
		}
		matchesExt := strings.EqualFold(filepath.Ext(path), "."+p.File.Ext)
		if p.Site == "pawchive" && core.IMAGE_EXTS[strings.ToLower(p.File.Ext)] && strings.EqualFold(filepath.Ext(path), ".jpg") {
			matchesExt = true
		}
		if matchesExt && existingFile(path) {
			return path
		}
	}
	return ""
}

func (i *Index) Parts(root string, p *core.PostInfo) map[int]bool {
	parts := map[int]bool{}
	if p == nil || p.File == nil {
		return parts
	}
	if pages := core.LocalPostPages(p); len(pages) > 0 {
		for _, page := range pages {
			if path := i.File(root, page); path != "" {
				parts[page.PageIndex] = archive.IsArchive(path)
			}
		}
		return parts
	}
	s := i.forSite(root, p.Site)
	id := regexp.QuoteMeta(p.ID.String())
	prefix := regexp.MustCompile(`^` + id + `(?:_[0-9a-fA-F]{32,64})?_p(\d+)(?:_|\.)`)
	for _, path := range s.files {
		name := filepath.Base(path)
		if match := prefix.FindStringSubmatch(name); match != nil {
			index := int(core.AsInt(match[1]))
			if index >= 0 && index < p.PageCount && existingFile(path) {
				parts[index] = archive.IsArchive(path)
			}
		}
	}
	return parts
}
func (i *Index) Progress(root string, p *core.PostInfo) history.Progress {
	return history.ProgressFromParts(p, i.Parts(root, p))
}
func (i *Index) Locate(root string, p *core.PostInfo) string {
	if p == nil || p.File == nil {
		return ""
	}
	if p.PageCount > 1 && p.Extra["expanded_page"] != true && p.Extra["standalone_attachment"] != true && i.Progress(root, p).State != "complete" {
		return ""
	}
	return i.File(root, p)
}
