package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"massdownloader/internal/applog"
	"massdownloader/internal/core"
	"massdownloader/internal/downloader"
	"massdownloader/internal/history"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (a *App) operationClient() *core.ApiClient {
	c := core.NewClient()
	c.Metadata = a.metadata
	c.SetCredentials(a.manager.Client.Credentials())
	a.configureMediaClient(c)
	return c
}

func (a *App) configureMediaClient(c *core.ApiClient) {
	if c == nil || a.settings == nil {
		return
	}
	filters, _ := a.settings.Get("filters_state").(map[string]any)
	danbooru, _ := filters["danbooru"].(map[string]any)
	c.SetDanbooruGold(danbooru["use_gold"] == true, strList(danbooru["gold_tags"]))
}

type PreviewPage struct {
	Posts   []*core.PostInfo `json:"posts"`
	Next    any              `json:"next"`
	HasMore bool             `json:"has_more"`
}

// Advance on the unfiltered page: local image filters must not lose the cursor.
func (a *App) SearchPage(siteKey, query string, cursor any) (PreviewPage, error) {
	return a.searchPage(context.Background(), siteKey, query, cursor)
}
func (a *App) runSearchPage(ctx context.Context, siteKey, query string, cursor any) (PreviewPage, error) {
	site := core.GetSite(siteKey)
	e := core.Registry.EngineFor(site)
	if e == nil {
		return PreviewPage{}, fmt.Errorf("unknown site %q", siteKey)
	}
	query = a.textFilterQuery(site, query)
	clean, images := core.SplitImagesOnly(query)
	validationQuery, _ := core.SplitHideText(clean)
	if err := e.ValidateQuery(validationQuery); err != nil {
		return PreviewPage{}, err
	}
	if cursor == nil || core.Str_(cursor) == "" {
		cursor = 1
	}
	if n, ok := cursor.(float64); ok {
		cursor = int(n)
	}
	c := a.operationClient()
	c.SetContext(ctx)
	result, err := core.FetchEnginePage(c, e, site, clean, site.PageLimit, cursor)
	if err != nil {
		return PreviewPage{}, err
	}
	posts := result.Posts
	out := PreviewPage{Posts: posts, Next: result.Next, HasMore: result.HasMore}
	if images {
		out.Posts = core.DropAnim(posts)
	}
	if out.Posts == nil {
		out.Posts = []*core.PostInfo{}
	}
	return out, nil
}

// Author galleries use an author query rather than the main search's tokens.
func (a *App) textFilterQuery(site *core.Site, query string) string {
	if core.FiltersFor(site).Find("hide_text") == nil {
		return query
	}
	filters, _ := a.settings.Get("filters_state").(map[string]any)
	values, _ := filters[site.Key].(map[string]any)
	if values["hide_text"] == true {
		if _, enabled := core.SplitHideText(query); !enabled {
			return strings.TrimSpace(query + " " + core.HIDE_TEXT_TOKEN)
		}
	}
	return query
}
func (a *App) DownloadStates(posts []*core.PostInfo) map[string]history.Progress {
	out := map[string]history.Progress{}
	if len(posts) == 0 {
		return out
	}
	queued := a.manager.QueuedPostKeys()
	mode, root := a.settings.GetString("dedup_mode"), a.settings.GetString("download_root")
	for _, p := range posts {
		if p != nil {
			key := p.Site + "\x00" + p.ID.String()
			queueKey := key
			if p.Extra["expanded_page"] == true || p.Extra["standalone_attachment"] == true {
				queueKey = fmt.Sprintf("%s\x00%d", key, p.PageIndex)
			}
			if p.File == nil {
				progress := history.Progress{State: "none", Total: 1, Queued: queued[queueKey]}
				if a.LocalMedia(p) != "" {
					progress.State, progress.Downloaded = "complete", 1
				}
				out[key] = progress
				continue
			}
			parts := map[int]bool{}
			if mode != "folder" {
				parts = a.history.Parts(p)
			}
			if mode == "folder" || mode == "both" {
				for index, isArchive := range a.folderIndex.Parts(root, p) {
					parts[index] = isArchive
				}
				if pages := core.LocalPostPages(p); len(pages) > 0 {
					for _, page := range pages {
						for _, output := range a.history.FileOutputs(page) {
							if withinRoot(root, output.Path) {
								if st, err := os.Stat(output.Path); err == nil && (st.IsDir() || st.Size() > 0) {
									parts[page.PageIndex] = output.Archive
									break
								}
							}
						}
					}
				} else {
					for index, output := range a.history.OutputsFor(p.Site, p.ID) {
						if withinRoot(root, output.Path) {
							if st, err := os.Stat(output.Path); err == nil && (st.IsDir() || st.Size() > 0) {
								parts[index] = output.Archive
							}
						}
					}
				}
			}
			progress := history.ProgressFromParts(p, parts)
			progress.Queued = queued[queueKey]
			out[key] = progress
		}
	}
	return out
}
func withinRoot(root, path string) bool {
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
func (a *App) Jobs() []*downloader.DownloadJob     { return a.manager.JobsSnapshot() }
func (a *App) PostQueued(post *core.PostInfo) bool { return a.manager.PostQueued(post) }
func (a *App) JobsByID(ids []int) []*downloader.DownloadJob {
	return a.manager.JobsSnapshotByID(ids)
}
func (a *App) ClearJobs() (int, error) { return a.manager.ClearJobs() }
func (a *App) ResolvePost(p *core.PostInfo) (*core.PostInfo, error) {
	return a.resolvePost(context.Background(), p)
}
func (a *App) runResolvePost(ctx context.Context, p *core.PostInfo) (*core.PostInfo, error) {
	if p == nil {
		return nil, fmt.Errorf("empty post")
	}
	e := core.Registry.EngineFor(core.GetSite(p.Site))
	if e == nil {
		return nil, fmt.Errorf("unknown site")
	}
	p = p.Clone()
	c := a.operationClient()
	c.SetContext(ctx)
	_, err := e.PrepareView(c, p)
	c.ApplyMediaPolicy(p)
	return p, err
}
func (a *App) Permalink(p *core.PostInfo) string {
	if p == nil {
		return ""
	}
	site := core.GetSite(p.Site)
	e := core.Registry.EngineFor(site)
	if e == nil {
		return ""
	}
	return e.Permalink(site, p)
}
func (a *App) ArtistQuery(name string, p *core.PostInfo) string {
	if p == nil {
		return ""
	}
	e := core.Registry.EngineFor(core.GetSite(p.Site))
	if e == nil {
		return ""
	}
	return e.ArtistQuery(name, p)
}
func (a *App) ArtistsForPost(p *core.PostInfo, hints []string) []string {
	if p == nil {
		return nil
	}
	return core.PostArtists(p, hints)
}
func (a *App) LocalMedia(p *core.PostInfo) string {
	if p == nil {
		return ""
	}
	if p.File == nil {
		return a.localMediaPath(a.feedTextPath(p))
	}
	if path, known := a.history.ManagedOutput(p); known {
		return a.localMediaPath(path)
	}
	paths := []string{a.history.OutputFor(p.Site, p.ID, p.PageIndex, func() string {
		if p.File.MD5 != "" {
			return p.File.MD5
		}
		return p.File.URL
	}())}
	if p.PageCount <= 1 {
		paths = append(paths, a.history.PathFor(p.File.MD5, p.Site, p.ID))
	}
	if p.PageCount <= 1 || p.Extra["expanded_page"] == true || p.Extra["standalone_attachment"] == true {
		for _, output := range a.history.FileOutputs(p) {
			paths = append(paths, output.Path)
		}
	}
	paths = append(paths, a.folderIndex.Locate(a.settings.GetString("download_root"), p))
	return a.localMediaPath(paths...)
}
func (a *App) localMediaPath(paths ...string) string {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if a.settings.GetString("dedup_mode") == "folder" && !withinRoot(a.settings.GetString("download_root"), path) {
			continue
		}
		if st, err := os.Stat(path); err == nil && (st.IsDir() || st.Size() > 0) {
			return path
		}
	}
	return ""
}
func (a *App) LocalFileURL(p *core.PostInfo) string {
	return a.grantLocalFile(a.LocalMedia(p))
}
func (a *App) grantLocalFile(path string) string {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return ""
	}
	token := fmt.Sprintf("%x", sha256.Sum256([]byte(path)))
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	if a.localFiles == nil || len(a.localFiles) > 10000 {
		a.localFiles = map[string]string{}
	}
	a.localFiles[token] = path
	return "/local-media?token=" + token + fmt.Sprintf("&v=%d-%d", st.ModTime().UnixNano(), st.Size())
}

// Resolve an actual completed queue item independently of the current dedup root.
func (a *App) downloadJobPath(id int) string {
	job := a.manager.JobSnapshot(id)
	if job == nil || (job.Status != downloader.StatusDone && job.Status != downloader.StatusSkipped) {
		return ""
	}
	for _, path := range []string{job.ResultPath, job.Dest} {
		if st, err := os.Stat(path); err == nil && (st.IsDir() || st.Size() > 0) {
			return path
		}
	}
	return ""
}
func (a *App) JobFileURL(id int) string {
	job := a.manager.JobSnapshot(id)
	if job == nil || !core.IMAGE_EXTS[strings.ToLower(job.Ext)] {
		return ""
	}
	return a.grantLocalFile(a.downloadJobPath(id))
}
func (a *App) RevealJob(id int) error {
	path := a.downloadJobPath(id)
	if path == "" {
		return fmt.Errorf("скачанный файл не найден")
	}
	return revealPath(path)
}
func (a *App) RevealPost(p *core.PostInfo) error {
	path := a.postLocation(p)
	if path == "" {
		return fmt.Errorf("скачанный файл не найден")
	}
	return revealPath(path)
}
func (a *App) postLocation(p *core.PostInfo) string {
	if path := a.LocalMedia(p); path != "" {
		return path
	}
	if p == nil || p.File == nil || p.Extra["expanded_page"] == true || p.Extra["standalone_attachment"] == true {
		return ""
	}
	// A whole album can be located through another saved page or a legacy record.
	// LocalMedia still resolves only the requested page for playback and attachments.
	outputs := a.history.OutputsFor(p.Site, p.ID)
	paths := make([]string, 0, len(outputs)+1)
	for i := 0; i < max(1, p.PageCount); i++ {
		paths = append(paths, outputs[i].Path)
	}
	paths = append(paths, a.history.PathFor(p.File.MD5, p.Site, p.ID))
	return a.localMediaPath(paths...)
}
func revealPath(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return exec.Command("explorer", path).Start()
	}
	return exec.Command("explorer", "/select,", path).Start()
}
func (a *App) Logs() []applog.Entry { return applog.Entries() }
func (a *App) ClearLogs()           { applog.ClearEntries() }
func (a *App) DefaultRateLimits() map[string]float64 {
	out := map[string]float64{}
	for _, s := range core.Sites() {
		out[s.Key] = core.DefaultIntervalFor(s)
	}
	return out
}

// Serve media from the authenticated Go session. Range allows video seeking.
type mediaHandler struct{ app *App }

func (h mediaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.app.serveMedia(w, r) }
func (a *App) serveMedia(w http.ResponseWriter, r *http.Request) {
	priority := core.PriorityInteractive
	if r.URL.Query().Get("thumb") == "1" {
		// Grid thumbnails yield to search, viewer and autocomplete requests.
		priority = core.PriorityThumbnail
	}
	r = r.WithContext(core.WithRequestPriority(r.Context(), priority))
	if r.URL.Path == "/local-media" {
		a.mediaMu.Lock()
		path := a.localFiles[r.URL.Query().Get("token")]
		a.mediaMu.Unlock()
		if path == "" {
			http.NotFound(w, r)
			return
		}
		file, err := os.Open(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		st, err := file.Stat()
		if err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, filepath.Base(path), st.ModTime(), file)
		return
	}
	if r.URL.Path != "/media" {
		http.NotFound(w, r)
		return
	}
	ref := r.URL.Query().Get("url")
	u, err := url.Parse(ref)
	if err != nil || !(u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "danbooru-media") {
		http.Error(w, "invalid media URL", 400)
		return
	}
	c := a.operationClient()
	c.SetContext(r.Context())
	if r.URL.Query().Get("image_cache") == "1" && a.loader != nil {
		data, err := a.loader.GetWithClient(r.Context(), c, ref)
		if err != nil {
			http.Error(w, "image unavailable", http.StatusBadGateway)
			return
		}
		hash := sha256.Sum256(data)
		w.Header().Set("ETag", fmt.Sprintf("\"%x\"", hash[:]))
		contentType := http.DetectContentType(data)
		if len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")) && (bytes.Equal(data[8:12], []byte("avif")) || bytes.Equal(data[8:12], []byte("avis"))) {
			contentType = "image/avif"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "private, no-cache")
		http.ServeContent(w, r, "image", time.Time{}, bytes.NewReader(data))
		return
	}
	if ext := videoExtension(ref, r.URL.Query().Get("video_ext")); ext != "" && a.settings != nil && a.settings.GetBool("cache_videos") {
		a.serveVideo(w, r, c, ref, ext)
		return
	}
	serveRemoteMedia(w, r, c, ref)
}

func serveRemoteMedia(w http.ResponseWriter, r *http.Request, c *core.ApiClient, ref string) {
	for _, raw := range c.MediaURLs([]string{ref}) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, raw, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept-Encoding", "identity")
		for k, v := range c.ImgHeaders(raw) {
			req.Header.Set(k, v)
		}
		for _, key := range []string{"Range", "If-Range"} {
			if v := r.Header.Get(key); v != "" {
				req.Header.Set(key, v)
			}
		}
		resp, err := c.DoMedia(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == 429 {
			resp.Body.Close()
			w.Header().Set("Retry-After", resp.Header.Get("Retry-After"))
			http.Error(w, "HTTP 429 — сайт ограничил запросы", 429)
			return
		}
		if resp.StatusCode != 200 && resp.StatusCode != 206 && resp.StatusCode != 416 {
			resp.Body.Close()
			continue
		}
		if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			resp.Body.Close()
			continue
		}
		for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
			if v := resp.Header.Get(key); v != "" {
				w.Header().Set(key, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		resp.Body.Close()
		return
	}
	http.Error(w, "media unavailable", http.StatusBadGateway)
}
