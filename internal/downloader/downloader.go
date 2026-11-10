// Package downloader — streaming scan-and-download pipeline.
// Port of app/downloader.py (Qt signals -> Wails events).
//
// One run = a single tag query against one site. A scanner goroutine walks
// the paginated result set and prepares jobs on a bounded download pool.
// ScanFirst holds transfers until scanning completes; RequireConfirmation
// additionally waits for user approval. Other callers can stream downloads.
//
// Files land in <root>/<site>/<artist>/<post_id>_<md5>.<ext> (artist
// subfolder optional). Every completed file is hash-verified against the API
// value and recorded in the history DB so it is never fetched again.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"massdownloader/internal/archive"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"massdownloader/internal/core"
	"massdownloader/internal/folderindex"
	"massdownloader/internal/history"
)

var invalidRe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
var reservedNameRe = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])(?:\.|$)`)

// Sanitize makes a filesystem-safe folder/file name (100 char cap).
func Sanitize(name, fallback string) string {
	name = strings.Trim(invalidRe.ReplaceAllString(name, ""), " .")
	if name == "" {
		name = fallback
	}
	if len(name) > 100 {
		name = string([]rune(name)[:min(100, len([]rune(name)))])
	}
	if reservedNameRe.MatchString(name) {
		name = "_" + name
	}
	return name
}

// ResolveArtist is the folder name for a post's artist ("" when unknown),
// delegated to the site's engine (Engine.derive_artists).
func ResolveArtist(post *core.PostInfo, artistTags []string) string {
	arts := core.PostArtists(post, artistTags)
	if len(arts) == 0 {
		return ""
	}
	return arts[0]
}

// Status is one download job's state (mirrors Status enum values so the
// frontend can reuse the same strings: queued/running/done/...).
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusDone      Status = "done"
	StatusSkipped   Status = "skipped"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// DownloadJob tracks one file transfer.
type DownloadJob struct {
	ID            int            `json:"id"`
	Post          *core.PostInfo `json:"-"`
	Site          string         `json:"site"`
	PostID        core.PostId    `json:"post_id"`
	PageIndex     int            `json:"page_index"`
	Ext           string         `json:"ext"`
	Dest          string         `json:"dest"`
	Name          string         `json:"name"`
	Status        Status         `json:"status"`
	Received      int64          `json:"received"`
	Total         int64          `json:"total"`
	Error         string         `json:"error"`
	ResultPath    string         `json:"result_path"`
	LocalRevision int64          `json:"local_revision,omitempty"`
	LocalMissing  bool           `json:"local_missing"`
	config        Config
	finished      chan struct{}
}

// Events is the signal bridge the app binds to Wails runtime events.
type Events interface {
	ScanUpdate(scanned, queued, skipped int)
	ScanFinished(errText string)
	JobQueued(id int, label string)
	JobStarted(id int, label string)
	JobProgress(id int, received, total int64)
	JobFinished(id int, status, errText string)
	AllFinished()
}

// Config carries one run's settings (mirrors DownloadManager.start args).
type Config struct {
	SiteKey             string
	Query               string
	Root                string
	MaxPosts            int
	SkipSeen            bool
	ByArtist            bool
	GroupAlbums         map[string]bool // per-site local download option
	DedupMode           string          // db | folder | both
	ArtistTags          []string
	Threads             int
	RequireConfirmation bool
	ScanFirst           bool
}

// Manager owns the scanner goroutine and the download pool for one run.
type Manager struct {
	History *history.History
	Client  *core.ApiClient
	Events  Events

	mu                   sync.Mutex
	opMu                 sync.Mutex
	sem                  chan struct{}
	runCtx               context.Context
	stopRun              context.CancelFunc
	closed               bool
	scanned, duplicates  int
	jobs                 map[int]*DownloadJob
	waveJobs             map[int]*DownloadJob
	jobOrder             []int
	cancel               chan struct{}
	cancelOnce           sync.Once
	nextID               int
	pending              int
	enqueuedPosts        map[string]bool
	albumLeft            map[string]int
	albumOk              map[string]int
	albumLanded          map[string]map[int]bool
	scanDone             bool
	bytesDone            int64
	folderIndex          *folderindex.Index
	contentJobs          map[string]*DownloadJob
	cfg                  Config
	configuredThreads    int
	configuredDedup      string
	configuredSkipSeen   bool
	wg                   sync.WaitGroup // in-flight downloads + scan
	releasing            bool
	waveFinished         bool
	startGate            chan struct{}
	awaitingConfirmation bool
	runID                int
}

// NewManager builds a manager; SetThreads adjusts the pool later.
func NewManager(h *history.History, ev Events) *Manager {
	c := core.NewClient()
	c.Timeout = 60 * time.Second
	return &Manager{
		History:            h,
		Client:             c,
		Events:             ev,
		jobs:               map[int]*DownloadJob{},
		waveJobs:           map[int]*DownloadJob{},
		cancel:             make(chan struct{}),
		enqueuedPosts:      map[string]bool{},
		albumLeft:          map[string]int{},
		albumOk:            map[string]int{},
		albumLanded:        map[string]map[int]bool{},
		folderIndex:        folderindex.New(),
		contentJobs:        map[string]*DownloadJob{},
		configuredSkipSeen: true,
		scanDone:           true, configuredThreads: 4, configuredDedup: "db", cfg: Config{Threads: 4, DedupMode: "db"}, sem: make(chan struct{}, 4),
	}
}

// RecordHistory: in folder mode the download folder is the source of truth —
// the history DB is neither consulted nor written.
func (m *Manager) RecordHistory() bool { return m.cfg.DedupMode != "folder" }

// SetThreads resizes the download pool (1..12).
func (m *Manager) SetThreads(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n < 1 {
		n = 1
	}
	if n > 12 {
		n = 12
	}
	m.configuredThreads = n
}
func (m *Manager) SetDedupMode(mode string) {
	if mode != "db" && mode != "folder" && mode != "both" {
		mode = "db"
	}
	m.mu.Lock()
	m.configuredDedup = mode
	m.mu.Unlock()
}

func (m *Manager) SetSkipSeen(skip bool) {
	m.mu.Lock()
	m.configuredSkipSeen = skip
	m.mu.Unlock()
}

// IsRunning reports whether a scan or any download is still in flight.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.scanDone || m.pending > 0 || m.releasing
}

// Start launches one mass run.
func (m *Manager) Start(cfg Config) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	if m.closed || !m.scanDone || m.pending > 0 || m.releasing {
		m.mu.Unlock()
		return fmt.Errorf("загрузка уже выполняется или приложение закрывается")
	}
	m.cfg = cfg
	m.runID++
	m.awaitingConfirmation = false
	m.startGate = nil
	if cfg.RequireConfirmation || cfg.ScanFirst {
		m.startGate = make(chan struct{})
	}
	if m.cfg.Threads <= 0 {
		m.cfg.Threads = m.configuredThreads
	}
	m.cfg.Threads = min(12, max(1, m.cfg.Threads))
	if m.cfg.DedupMode == "" {
		m.cfg.DedupMode = m.configuredDedup
	}
	m.sem = make(chan struct{}, m.cfg.Threads)
	m.runCtx, m.stopRun = context.WithCancel(context.Background())
	m.Client.SetContext(m.runCtx)
	m.waveFinished = false
	m.cancel = make(chan struct{})
	m.cancelOnce = sync.Once{}
	m.scanned, m.duplicates = 0, 0
	m.waveJobs = map[int]*DownloadJob{}
	m.pending = 0
	m.enqueuedPosts = map[string]bool{}
	m.albumLeft = map[string]int{}
	m.albumOk = map[string]int{}
	m.albumLanded = map[string]map[int]bool{}
	m.scanDone = false
	m.bytesDone = 0
	m.contentJobs = map[string]*DownloadJob{}
	m.mu.Unlock()
	m.wg.Add(1)
	go m.scanWorker()
	return nil
}

// DownloadPosts explicitly enqueues specific posts (preview selection /
// detail view and author/feed bulk downloads). Uses the same attachment-level
// duplicate checks as a mass run; bypasses only query/extension filters.
func (m *Manager) DownloadPosts(posts []*core.PostInfo, root string,
	byArtist bool, artistTags []string, groupAlbums ...map[string]bool) int {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	if m.closed || m.releasing {
		m.mu.Unlock()
		return 0
	}
	running := !m.scanDone || m.pending > 0
	m.mu.Unlock()

	if !running {
		m.mu.Lock()
		m.startGate = nil
		m.cfg.Root = root
		m.cfg.ByArtist = byArtist
		m.cfg.GroupAlbums = nil
		if len(groupAlbums) > 0 {
			m.cfg.GroupAlbums = groupAlbums[0]
		}
		m.cfg.ArtistTags = append([]string(nil), artistTags...)
		m.cfg.Threads = m.configuredThreads
		m.cfg.DedupMode = m.configuredDedup
		m.cfg.SkipSeen = m.configuredSkipSeen
		m.sem = make(chan struct{}, min(12, max(1, m.cfg.Threads)))
		m.runCtx, m.stopRun = context.WithCancel(context.Background())
		m.Client.SetContext(m.runCtx)
		m.waveFinished = false
		m.cancelOnce = sync.Once{}
		m.scanned, m.duplicates = 0, 0
		m.cancel = make(chan struct{})
		m.scanDone = false // hold all_finished until enqueued
		// Keep all session rows and their IDs; only the current wave's
		// counters and double-click protection start over.
		m.waveJobs = map[int]*DownloadJob{}
		m.pending = 0
		m.bytesDone = 0
		m.enqueuedPosts = map[string]bool{}
		m.contentJobs = map[string]*DownloadJob{}
		m.albumLeft = map[string]int{}
		m.albumOk = map[string]int{}
		m.albumLanded = map[string]map[int]bool{}
		m.folderIndex.Invalidate()
		m.mu.Unlock()
	}
	added := 0
	for _, post := range posts {
		if post == nil || post.File == nil {
			continue
		}
		key := queueKey(post)
		m.mu.Lock()
		seen := m.enqueuedPosts[key]
		m.mu.Unlock()
		if seen {
			continue
		}
		added += m.enqueue(post, true)
	}
	if !running {
		m.scanComplete("")
	}
	return added
}

// RetryFailed starts a wave of the exact failed file jobs, retaining IDs,
// destinations and successful jobs. It must not re-expand their parent albums.
func (m *Manager) RetryFailed() (int, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	if m.closed || !m.scanDone || m.pending > 0 || m.releasing {
		m.mu.Unlock()
		return 0, fmt.Errorf("дождитесь завершения текущей загрузки")
	}
	var failed []*DownloadJob
	for _, id := range m.jobOrder {
		if job := m.jobs[id]; job.Status == StatusFailed {
			failed = append(failed, job)
		}
	}
	if len(failed) == 0 {
		m.mu.Unlock()
		return 0, nil
	}
	if m.stopRun != nil {
		m.stopRun()
	}
	m.runCtx, m.stopRun = context.WithCancel(context.Background())
	m.Client.SetContext(m.runCtx)
	m.cancel = make(chan struct{})
	m.startGate = nil
	m.cancelOnce = sync.Once{}
	m.sem = make(chan struct{}, min(12, max(1, m.configuredThreads)))
	m.waveFinished = false
	m.scanDone = false // reserve the wave before starting workers
	m.pending = len(failed)
	m.contentJobs = map[string]*DownloadJob{}
	m.albumLeft = map[string]int{}
	m.albumOk = map[string]int{}
	m.albumLanded = map[string]map[int]bool{}
	albumPosts := map[string]*core.PostInfo{}
	failedParts := map[string]map[int]bool{}
	for _, job := range failed {
		m.waveJobs[job.ID] = job
		job.Status, job.Error = StatusQueued, ""
		job.Received, job.Total = 0, 0
		job.ResultPath = ""
		job.finished = make(chan struct{})
		p := job.Post
		if p.PageCount > 1 && p.Extra["standalone_attachment"] != true {
			key := p.Site + "\x00" + p.ID.Key()
			m.albumLeft[key]++
			albumPosts[key] = p
			if failedParts[key] == nil {
				failedParts[key] = map[int]bool{}
				m.albumLanded[key] = map[int]bool{}
			}
			failedParts[key][p.PageIndex] = true
		}
	}
	// Successful parts from the previous wave still count toward album
	// completion. Stale paths and the retried parts must not count twice.
	for key, p := range albumPosts {
		for index, output := range m.History.OutputsFor(p.Site, p.ID) {
			if index < 0 || index >= p.PageCount || failedParts[key][index] {
				continue
			}
			if st, err := os.Stat(output.Path); err == nil && (st.IsDir() || st.Size() > 0) {
				m.albumOk[key]++
				m.albumLanded[key][index] = true
			}
		}
	}
	m.wg.Add(len(failed))
	m.mu.Unlock()
	// Older waves can contain multiple failed attempts for the same file.
	// Serialize those attempts so their temporary files cannot collide.
	groups := map[string][]*DownloadJob{}
	for _, job := range failed {
		if m.Events != nil {
			m.Events.JobQueued(job.ID, job.Name)
		}
		groups[job.Dest] = append(groups[job.Dest], job)
	}
	for _, jobs := range groups {
		go func() {
			for _, job := range jobs {
				m.fileWorker(job)
			}
		}()
	}
	m.scanComplete("")
	return len(failed), nil
}

// Cancel aborts the current run («Остановить»).
func (m *Manager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.awaitingConfirmation = false
	if m.stopRun != nil {
		m.stopRun()
	}
	m.cancelOnce.Do(func() { close(m.cancel) })
}

// ConfirmRun applies only to the scanned run identified by the dialog.
func (m *Manager) ConfirmRun(runID int, accept bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.awaitingConfirmation || runID != m.runID || m.runCtx.Err() != nil {
		return false
	}
	m.awaitingConfirmation = false
	if accept {
		close(m.startGate)
	} else {
		m.stopRun()
		m.cancelOnce.Do(func() { close(m.cancel) })
	}
	return true
}

func (m *Manager) isCancelled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.cancel:
		return true
	default:
		return false
	}
}

// QueuedCount is the total number of jobs created this run.
func (m *Manager) QueuedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.waveJobs)
}

// BytesDone sums recorded file sizes of DONE jobs.
func (m *Manager) BytesDone() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bytesDone
}

// Summary counts the current wave; session_total counts retained UI rows.
func (m *Manager) Summary() map[string]int {
	out := map[string]int{
		string(StatusQueued): 0, string(StatusRunning): 0,
		string(StatusDone): 0, string(StatusSkipped): 0,
		string(StatusFailed): 0, string(StatusCancelled): 0,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.waveJobs {
		out[string(j.Status)]++
	}
	out["scanned"], out["duplicates"] = m.scanned, m.duplicates
	out["total"] = len(m.waveJobs)
	out["session_total"] = len(m.jobs)
	if m.awaitingConfirmation {
		out["awaiting_confirmation"] = m.runID
	}
	return out
}

// JobsSnapshot lists jobs oldest-first (the UI queue view).
func (m *Manager) JobsSnapshot() []*DownloadJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*DownloadJob, 0, len(m.jobOrder))
	for _, id := range m.jobOrder {
		copy := *m.jobs[id]
		out = append(out, &copy)
	}
	return out
}

// ClearJobs releases the retained UI queue without touching files or history.
// Keep IDs monotonic so delayed events cannot refer to a new job after clearing.
func (m *Manager) ClearJobs() (int, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.scanDone || m.pending > 0 || m.releasing {
		return 0, fmt.Errorf("дождитесь завершения текущей загрузки")
	}
	count := len(m.jobs)
	m.jobs = map[int]*DownloadJob{}
	m.waveJobs = map[int]*DownloadJob{}
	m.jobOrder = nil
	m.enqueuedPosts = map[string]bool{}
	m.albumLeft = map[string]int{}
	m.albumOk = map[string]int{}
	m.albumLanded = map[string]map[int]bool{}
	m.scanned, m.duplicates, m.bytesDone = 0, 0, 0
	return count, nil
}

// JobsSnapshotByID avoids copying the retained session on every status event.
func (m *Manager) JobsSnapshotByID(ids []int) []*DownloadJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*DownloadJob, 0, len(ids))
	seen := make(map[int]bool, len(ids))
	for _, id := range ids {
		if job := m.jobs[id]; job != nil && !seen[id] {
			copy := *job
			out = append(out, &copy)
			seen[id] = true
		}
	}
	return out
}

// QueuedPostKeys supplies one batch snapshot for card download-state queries.
// Parent keys cover the album; page keys cover exact attachments.
func (m *Manager) QueuedPostKeys() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := map[string]bool{}
	for _, job := range m.waveJobs {
		if job.Status != StatusQueued && job.Status != StatusRunning {
			continue
		}
		key := job.Site + "\x00" + job.PostID.String()
		keys[key] = true
		keys[fmt.Sprintf("%s\x00%d", key, job.PageIndex)] = true
	}
	return keys
}

// PostQueued checks active transfers without copying the retained session list.
// A whole post includes every album page; an attachment includes only its page.
func (m *Manager) PostQueued(post *core.PostInfo) bool {
	if post == nil {
		return false
	}
	attachment := post.Extra["expanded_page"] == true || post.Extra["standalone_attachment"] == true
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.waveJobs {
		if job.Site == post.Site && job.PostID.String() == post.ID.String() &&
			(!attachment || job.PageIndex == post.PageIndex) &&
			(job.Status == StatusQueued || job.Status == StatusRunning) {
			return true
		}
	}
	return false
}

// JobSnapshot returns immutable job metadata for the downloads view.
func (m *Manager) JobSnapshot(id int) *DownloadJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job := m.jobs[id]; job != nil {
		copy := *job
		return &copy
	}
	return nil
}

// -- pipeline internals ------------------------------------------------------

func (m *Manager) buildFolderIndex() {
	m.folderIndex.Invalidate()
}

func (m *Manager) accept(post *core.PostInfo) bool {
	// Deduplicate after expanding the post. A known first attachment is not
	// evidence that the rest of an album has been downloaded.
	return post != nil && post.File != nil
}

func (m *Manager) destFor(post *core.PostInfo) string {
	site := core.GetSite(post.Site)
	artist := Sanitize(ResolveArtist(post, m.cfg.ArtistTags), "_unknown")
	folder := filepath.Join(m.cfg.Root, site.Key)
	groupAlbum := post.PageCount > 1 && m.cfg.GroupAlbums[post.Site]
	if m.cfg.ByArtist || groupAlbum {
		folder = filepath.Join(folder, artist)
	}
	// A merged author always uses the group's folder, whatever the layout.
	if m.History != nil {
		if dir, ok := m.History.AuthorFolder(m.cfg.Root, site.Key, artist); ok {
			folder = dir
		}
	}
	if groupAlbum {
		folder = filepath.Join(folder, Sanitize(core.Str_(post.Extra["title"]), Sanitize(post.ID.String(), "post")))
	}
	// booru posts get <id>_<md5>; engines without a hash (Pixiv) use <id>
	stem := Sanitize(post.ID.String(), "post")
	if post.File.MD5 != "" {
		stem += "_" + post.File.MD5
	}
	// album pages share one id — keep them apart on disk with _pN
	if post.PageCount > 1 {
		stem += fmt.Sprintf("_p%d", post.PageIndex)
	}
	if archive.IsArchive(post.File.Ext) {
		name := filepath.Base(strings.ReplaceAll(core.Str_(post.Extra["download_name"]), "\\", "/"))
		name = strings.TrimSuffix(name, filepath.Ext(name))
		if name != "" && name != "." {
			stem += "_" + Sanitize(name, "archive")
		}
	}
	return filepath.Join(folder, stem+"."+Sanitize(post.File.Ext, "bin"))
}

func fingerprint(p *core.PostInfo) string {
	return core.FileFingerprint(p)
}
func queueKey(p *core.PostInfo) string {
	key := p.Site + "\x00" + p.ID.Key()
	if p.Extra["standalone_attachment"] == true || p.Extra["expanded_page"] == true {
		key += fmt.Sprintf("\x00%d", p.PageIndex)
	}
	return key
}
func (m *Manager) enqueue(post *core.PostInfo, explicit ...bool) int {
	if post.PageCount > 1 && post.Extra["expanded_page"] == true && post.Extra["standalone_attachment"] != true {
		post = post.Clone()
		post.Extra["standalone_attachment"] = true
	}
	m.mu.Lock()
	key := queueKey(post)
	if m.enqueuedPosts[key] {
		m.mu.Unlock()
		return 0
	}
	m.enqueuedPosts[key] = true
	m.mu.Unlock()
	engine := core.Registry.EngineFor(core.GetSite(post.Site))
	if engine == nil {
		return 0
	}
	// Old saved posts may lack the title supplied by today's search parser.
	m.mu.Lock()
	groupAlbum := m.cfg.GroupAlbums[post.Site] && post.PageCount > 1
	m.mu.Unlock()
	if groupAlbum && core.Str_(post.Extra["title"]) == "" {
		post = post.Clone()
		_, _ = engine.PrepareView(m.Client, post)
	}
	pages := []*core.PostInfo{post}
	// The viewer has already resolved this attachment. Re-expanding its
	// parent would silently download the entire album instead of one file.
	if post.Extra["expanded_page"] != true && post.Extra["standalone_attachment"] != true {
		pages = engine.ExpandPost(m.Client, post)
	}
	pending := []*core.PostInfo{}
	completed := 0
	firstPath := ""
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	for _, page := range pages {
		if page == nil || page.File == nil {
			continue
		}
		if cfg.SkipSeen && (len(explicit) == 0 || !explicit[0]) && m.History.SkipDeleted(page) {
			continue
		}
		if output, found := m.existingOutput(page, cfg); found {
			completed++
			if firstPath == "" {
				firstPath = output.Path
			}
			_ = m.History.AddOutput(page.Site, page.ID, page.PageIndex, output.Path, archive.IsArchive(page.File.Ext), fingerprint(page))
		} else {
			pending = append(pending, page)
		}
	}
	standalone := post.Extra["standalone_attachment"] == true
	if !standalone && len(pending) == 0 && completed >= max(1, post.PageCount) && cfg.DedupMode != "folder" {
		_ = m.History.Add(strings.ToLower(post.File.MD5), post.Site, post.ID, firstPath)
	}
	if len(pages) > 1 && !standalone && len(pending) > 0 {
		key := post.Site + "\x00" + post.ID.Key()
		m.mu.Lock()
		m.albumLeft[key] = len(pending)
		m.albumOk[key] = completed
		m.mu.Unlock()
	}
	for _, page := range pending {
		m.enqueueJob(page)
	}
	return len(pending)
}

func (m *Manager) enqueueJob(post *core.PostInfo) {
	m.mu.Lock()
	jid := m.nextID
	m.nextID++
	dest := m.destFor(post)
	job := &DownloadJob{ID: jid, Post: post, Dest: dest, Site: post.Site, PostID: post.ID, PageIndex: post.PageIndex, Ext: post.File.Ext,
		Name: filepath.Base(dest), Status: StatusQueued, config: m.cfg, finished: make(chan struct{})}
	job.config.ArtistTags = append([]string(nil), m.cfg.ArtistTags...)
	m.jobs[jid] = job
	m.waveJobs[jid] = job
	m.jobOrder = append(m.jobOrder, jid)
	m.pending++
	m.enqueuedPosts[queueKey(post)] = true
	if hash := contentJobKey(job); hash != "" && m.contentJobs[hash] == nil {
		m.contentJobs[hash] = job
	}
	m.mu.Unlock()
	if m.Events != nil {
		m.Events.JobQueued(jid, job.Name)
	}
	m.wg.Add(1)
	go m.fileWorker(job)
}

// scanWorker walks the result set, enqueueing accepted posts as they come.
func (m *Manager) scanWorker() {
	defer m.wg.Done()
	scanned, skipped := 0, 0
	errText := ""
	site := core.GetSite(m.cfg.SiteKey)
	var scanErr error
	if site == nil {
		scanErr = fmt.Errorf("unknown site %q", m.cfg.SiteKey)
	} else if e := core.Registry.EngineFor(site); e == nil {
		scanErr = fmt.Errorf("no engine for %q", m.cfg.SiteKey)
	} else if err := e.ValidateQuery(m.cfg.Query); err != nil {
		scanErr = err
	}
	if scanErr == nil {
		m.buildFolderIndex()
		maxPosts := m.cfg.MaxPosts
		err := m.Client.IterPosts(m.runCtx, m.cfg.SiteKey, m.cfg.Query,
			maxPosts, func() bool { return m.isCancelled() },
			func(post *core.PostInfo) bool {
				if m.isCancelled() {
					return false
				}
				scanned++
				m.mu.Lock()
				m.scanned = scanned
				m.mu.Unlock()
				if !m.accept(post) || m.enqueue(post) == 0 {
					skipped++
					m.mu.Lock()
					m.duplicates = skipped
					m.mu.Unlock()
				}
				if scanned%20 == 0 && m.Events != nil {
					m.Events.ScanUpdate(scanned, m.QueuedCount(), skipped)
				}
				return true
			})
		switch {
		case err == nil:
		case err == core.DownloadCancelled:
			// «Остановить» mid-page — a clean stop
		default:
			errText = err.Error()
			slog.Warn("Скан прерван", "site", m.cfg.SiteKey,
				"query", m.cfg.Query, "err", err)
		}
	} else {
		errText = scanErr.Error()
	}
	if m.startGate != nil {
		m.mu.Lock()
		if errText != "" && m.stopRun != nil {
			m.stopRun()
		}
		ready := errText == "" && m.runCtx.Err() == nil && len(m.waveJobs) > 0
		m.awaitingConfirmation = ready && m.cfg.RequireConfirmation
		waiting := m.awaitingConfirmation
		if ready && !waiting {
			close(m.startGate)
		}
		m.mu.Unlock()
		if !ready {
			m.Cancel()
		}
	}
	if m.Events != nil {
		m.Events.ScanUpdate(scanned, m.QueuedCount(), skipped)
		m.Events.ScanFinished(errText)
	}
	if m.startGate != nil {
		select {
		case <-m.startGate:
		case <-m.runCtx.Done():
		}
	}
	m.scanComplete(errText)
}

// maxRateLimitRounds bounds how often one file waits out a site's 429 pause
// before it is reported as failed (retryable from the downloads panel).
const maxRateLimitRounds = 4

// rateLimitBaseWait is the minimum first wait (doubling per round) when the
// zone pause is shorter or unknown. A variable so tests can shorten it.
var rateLimitBaseWait = 15 * time.Second

// fileWorker transfers one job's file.
func (m *Manager) fileWorker(job *DownloadJob) {
	defer m.wg.Done()
	m.mu.Lock()
	gate, ctx := m.startGate, m.runCtx
	m.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			m.complete(job, StatusCancelled)
			return
		}
		if ctx.Err() != nil {
			m.complete(job, StatusCancelled)
			return
		}
	}
	if reused, err := m.reuseContentJob(job); reused || err != nil {
		if err != nil {
			m.complete(job, StatusCancelled)
		} else {
			m.complete(job, StatusSkipped)
		}
		return
	}
	sem := m.sem
	select {
	case sem <- struct{}{}:
	case <-m.runCtx.Done():
		m.complete(job, StatusCancelled)
		return
	}
	held := true
	defer func() {
		if held {
			<-sem
		}
	}()
	post := job.Post
	tmp := job.Dest + ".part"
	rmTmp := func() { _ = os.Remove(tmp) }
	if m.isCancelled() {
		m.complete(job, StatusCancelled)
		return
	}
	if st, err := os.Stat(job.Dest); err == nil && st.Size() > 0 {
		// history is recorded by complete (album-aware)
		if err := m.processArchive(job); err != nil {
			if m.isCancelled() {
				m.complete(job, StatusCancelled)
				return
			}
			m.mu.Lock()
			job.Error = err.Error()
			m.mu.Unlock()
			m.complete(job, StatusFailed)
			return
		}
		m.complete(job, StatusSkipped)
		return
	}
	m.mu.Lock()
	job.Status = StatusRunning
	m.mu.Unlock()
	if m.Events != nil {
		m.Events.JobStarted(job.ID, job.Name)
	}

	progress := func(rec, total int64) bool {
		m.mu.Lock()
		job.Received, job.Total = rec, total
		m.mu.Unlock()
		if m.Events != nil {
			m.Events.JobProgress(job.ID, rec, total)
		}
		return !m.isCancelled()
	}

	site := core.GetSite(post.Site)
	engine := core.Registry.EngineFor(site)
	urls := engine.DownloadURLs(post)
	// verify by whichever algorithm the hash length implies (md5=32 hex;
	// sha1=40). Some engines serve a transcoded derivative (Gelbooru video)
	// whose bytes can't match the recorded hash — they opt out of the check.
	expected := strings.ToLower(post.File.MD5)
	if len(expected) != 32 && len(expected) != 40 && len(expected) != 64 {
		expected = ""
	}
	if expected != "" && !engine.VerifiesHash(post) {
		expected = ""
	}
	if len(urls) == 0 && post.File != nil {
		urls = []string{post.File.URL}
	}
	written, err := m.Client.StreamToFile(m.runCtx, urls, tmp, progress, expected)
	// A site rate limit is not a broken file: release the slot (other sites
	// keep downloading), wait out the zone pause and try again.
	for round := 0; core.IsRateLimited(err) && round < maxRateLimitRounds && !m.isCancelled(); round++ {
		wait := rateLimitBaseWait << round
		if len(urls) > 0 {
			wait = max(wait, core.Throttle.CooldownRemaining(urls[0]))
		}
		wait = min(wait, core.MaxCooldown)
		slog.Info("Загрузка отложена: лимит сайта", "file", job.Name, "site", post.Site,
			"round", round+1, "of", maxRateLimitRounds, "wait_s", wait.Seconds())
		m.mu.Lock()
		job.Status = StatusQueued
		m.mu.Unlock()
		<-sem
		held = false
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			rmTmp()
			m.complete(job, StatusCancelled)
			return
		}
		select {
		case sem <- struct{}{}:
			held = true
		case <-ctx.Done():
			rmTmp()
			m.complete(job, StatusCancelled)
			return
		}
		m.mu.Lock()
		job.Status = StatusRunning
		m.mu.Unlock()
		slog.Debug("Загрузка возобновлена после паузы", "file", job.Name, "site", post.Site, "round", round+1)
		written, err = m.Client.StreamToFile(m.runCtx, urls, tmp, progress, expected)
	}
	dest := job.Dest
	var responseErr *core.ApiError
	if errors.As(err, &responseErr) && responseErr.Status == http.StatusNotFound {
		if preview := pawchivePreviewURL(post); preview != "" {
			// The CDN thumbnail is a JPEG derivative, not the original bytes.
			// A partial original must never be appended to this transfer.
			rmTmp()
			written, err = m.Client.StreamToFile(m.runCtx, []string{preview}, tmp, progress, "")
			if err == nil {
				dest = strings.TrimSuffix(job.Dest, filepath.Ext(job.Dest)) + ".jpg"
			}
		}
	}
	if err == nil && written == 0 {
		err = &core.ApiError{Msg: "пустой файл"}
	}
	if err == core.DownloadCancelled {
		rmTmp()
		m.complete(job, StatusCancelled)
		return
	}
	if err != nil {
		rmTmp()
		m.mu.Lock()
		job.Error = err.Error()
		m.mu.Unlock()
		slog.Warn("Не скачан", "file", job.Name, "site", post.Site, "err", err)
		m.complete(job, StatusFailed)
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		rmTmp()
		m.mu.Lock()
		job.Error = err.Error()
		m.mu.Unlock()
		m.complete(job, StatusFailed)
		return
	}
	if dest != job.Dest {
		m.mu.Lock()
		job.Dest, job.Name, job.Ext = dest, filepath.Base(dest), "jpg"
		m.mu.Unlock()
	}
	if err := m.processArchive(job); err != nil {
		if m.isCancelled() {
			m.complete(job, StatusCancelled)
			return
		}
		m.mu.Lock()
		job.Error = err.Error()
		m.mu.Unlock()
		m.complete(job, StatusFailed)
		return
	}
	m.complete(job, StatusDone)
}

// Match the viewer's image source for this file, never another attachment's
// cover image or a thumbnail used as a video/archive placeholder.
func pawchivePreviewURL(post *core.PostInfo) string {
	if post.Site != "pawchive" || post.File == nil || !core.IMAGE_EXTS[strings.ToLower(post.File.Ext)] {
		return ""
	}
	preview := post.SampleURL
	if preview == "" {
		preview = post.PreviewURL
	}
	if preview == post.File.URL {
		return ""
	}
	return preview
}

func (m *Manager) scanComplete(errText string) {
	m.mu.Lock()
	m.scanDone = true
	done := m.pending == 0
	m.mu.Unlock()
	if done {
		m.finishWave()
	}
}

func (m *Manager) complete(job *DownloadJob, status Status) {
	m.mu.Lock()
	job.Status = status
	post := job.Post
	landed := status == StatusDone || status == StatusSkipped
	record := false
	if status == StatusDone {
		m.bytesDone += post.File.Size
	}
	key := post.Site + "\x00" + post.ID.Key()
	if _, isAlbum := m.albumLeft[key]; isAlbum && post.Extra["standalone_attachment"] != true {
		// a page of a multi-image work: record the artwork only once every
		// page has landed, so a partial album stays "unseen"
		m.albumLeft[key]--
		if landed && (m.albumLanded[key] == nil || !m.albumLanded[key][post.PageIndex]) {
			m.albumOk[key]++
			if m.albumLanded[key] != nil {
				m.albumLanded[key][post.PageIndex] = true
			}
		}
		if m.albumLeft[key] <= 0 {
			ok := m.albumOk[key]
			delete(m.albumOk, key)
			delete(m.albumLeft, key)
			delete(m.albumLanded, key)
			record = ok >= post.PageCount && job.config.DedupMode != "folder"
		}
	} else if landed && job.config.DedupMode != "folder" && post.Extra["standalone_attachment"] != true {
		record = true
	}
	m.mu.Unlock()
	result := job.ResultPath
	if result == "" {
		result = job.Dest
	}
	if landed {
		_ = m.History.AddOutput(post.Site, post.ID, post.PageIndex, result, archive.IsArchive(post.File.Ext), fingerprint(post))
	}
	if record {
		_ = m.History.Add(strings.ToLower(post.File.MD5), post.Site, post.ID,
			result)
	}
	m.mu.Lock()
	if job.finished != nil {
		close(job.finished)
	}
	m.mu.Unlock()
	if m.Events != nil {
		m.Events.JobFinished(job.ID, string(status), job.Error)
	}
	m.mu.Lock()
	m.pending--
	done := m.scanDone && m.pending == 0
	m.mu.Unlock()
	if done {
		m.finishWave()
	}
}

// finishWave fires AllFinished once per wave, then lets engines undo any
// transient account state (DeviantArt unwatches authors it followed).
func (m *Manager) processArchive(job *DownloadJob) error {
	m.mu.Lock()
	job.ResultPath = job.Dest
	m.mu.Unlock()
	p := job.Post
	if !archive.IsArchive(p.File.Ext) || p.Extra["archive_extract"] == false {
		return nil
	}
	target := ""
	if p.Extra["archive_to_artist"] == true {
		target = filepath.Join(job.config.Root, p.Site, Sanitize(ResolveArtist(p, job.config.ArtistTags), "_unknown"))
		if p.PageCount > 1 && job.config.GroupAlbums[p.Site] {
			target = filepath.Dir(job.Dest)
		}
	}
	output, _, err := archive.ExtractContext(m.runCtx, job.Dest, p.Extra["archive_to_artist"] == true, target)
	if err != nil {
		return err
	}
	if p.Extra["archive_delete"] == true {
		if err := os.Remove(job.Dest); err != nil {
			return err
		}
		m.mu.Lock()
		job.ResultPath = output
		m.mu.Unlock()
	}
	return nil
}
func (m *Manager) finishWave() {
	m.mu.Lock()
	if m.waveFinished || m.releasing || !m.scanDone || m.pending != 0 {
		m.mu.Unlock()
		return
	}
	m.releasing = true
	m.waveFinished = true
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Client.SetContext(ctx)
		for _, site := range core.Sites() {
			if e := core.Registry.EngineFor(site); e != nil {
				func() { defer func() { _ = recover() }(); e.Release(m.Client) }()
			}
		}
		m.mu.Lock()
		m.releasing = false
		m.mu.Unlock()
		if m.Events != nil {
			m.Events.AllFinished()
		}
	}()
}

// Shutdown cancels and waits for workers before destruction.
func (m *Manager) Shutdown(timeout time.Duration) {
	m.opMu.Lock()
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.Cancel()
	m.opMu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		// Cancel the release requests as well, then join before the DB closes.
		m.Client.SetContext(m.runCtx)
		<-done
	}
	core.CloseAllHTTP()
}
