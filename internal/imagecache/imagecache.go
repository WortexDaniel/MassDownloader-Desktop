// Package imagecache — asynchronous thumbnail/preview loader with a bounded
// on-disk cache. Port of app/image_cache.py (Qt signals -> callbacks).
//
// Images are fetched through a bounded worker pool and delivered back via a
// callback. Raw bytes are cached on disk (keyed by URL sha1) so re-browsing
// is instant. The cache is size-capped (thumb_cache_mb, 512 MB default) and
// evicted least-recently-used: every hit bumps the file's mtime and a
// background sweep trims the oldest files back under the cap.
package imagecache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"massdownloader/internal/appdir"
	"massdownloader/internal/core"
)

// pruneEvery triggers a sweep after this many fresh files are written, so a
// long browsing session stays bounded without sweeping on every request.
const pruneEvery = 200

type counter struct {
	mu     sync.Mutex
	writes int
}

var writes counter

func noteWrite() bool {
	writes.mu.Lock()
	defer writes.mu.Unlock()
	writes.writes++
	if writes.writes >= pruneEvery {
		writes.writes = 0
		return true
	}
	return false
}

// isImage reports whether data begins with a magic-byte prefix of an image
// format the boorus serve. A preview fetch that isn't an image (e.g.
// Gelbooru's refererless hotlink-block HTML page served with HTTP 200) is
// refused so it never poisons the cache nor blanks the grid.
func isImage(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	if data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff { // JPEG
		return true
	}
	if string(data[:8]) == "\x89PNG\r\n\x1a\n" { // PNG
		return true
	}
	p6 := string(data[:6])
	if p6 == "GIF87a" || p6 == "GIF89a" { // GIF
		return true
	}
	if string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" { // WebP
		return true
	}
	if string(data[4:8]) == "ftyp" && (string(data[8:12]) == "avif" ||
		string(data[8:12]) == "avis") { // AVIF (Sankaku previews)
		return true
	}
	if data[0] == 'B' && data[1] == 'M' { // BMP
		return true
	}
	return false
}

// CacheDir is APP_DIR/thumbs, created on demand.
func CacheDir() string {
	d := filepath.Join(appdir.Dir(), "thumbs")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// CacheCapBytes is the configured ceiling in bytes (floor 64 MB).
func CacheCapBytes(getMB func() int) int64 {
	mb := getMB()
	if mb <= 0 {
		mb = 512
	}
	if mb < 64 {
		mb = 64
	}
	return int64(mb) * 1024 * 1024
}

// Prune evicts the oldest cache files until the folder is under maxBytes
// (0 -> configured cap). Returns bytes freed. Also clears stray .part temp
// files left by interrupted writes. Safe from any thread: files mid-write
// hold the newest mtime (never picked first).
func Prune(maxBytes int64, dir string) int64 {
	if maxBytes <= 0 {
		maxBytes = CacheCapBytes(func() int { return 512 })
	}
	if dir == "" {
		dir = CacheDir()
	}
	type entry struct {
		path  string
		size  int64
		mtime int64
	}
	var entries []entry
	var total int64
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		if filepath.Ext(f.Name()) == ".part" {
			if time.Since(info.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(dir, f.Name()))
			}
			continue
		}
		entries = append(entries, entry{filepath.Join(dir, f.Name()),
			info.Size(), info.ModTime().UnixNano()})
		total += info.Size()
	}
	if total <= maxBytes {
		return 0
	}
	// trim to 90% of the cap so we don't re-sweep on every new file
	target := int64(float64(maxBytes) * 0.9)
	var freed int64
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mtime < entries[j].mtime
	})
	for _, e := range entries { // oldest first
		if total-freed <= target {
			break
		}
		if err := os.Remove(e.path); err == nil {
			freed += e.size
		}
	}
	return freed
}

// Size is the current footprint as (bytes, fileCount), ignoring .part files.
func Size(dir string) (int64, int) {
	if dir == "" {
		dir = CacheDir()
	}
	var total int64
	count := 0
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) == ".part" {
			continue
		}
		if info, err := f.Info(); err == nil {
			total += info.Size()
			count++
		}
	}
	return total, count
}

// Clear deletes every cached file; returns bytes freed.
func Clear(dir string) int64 {
	if dir == "" {
		dir = CacheDir()
	}
	var freed int64
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if info, err := f.Info(); err == nil {
			if os.Remove(filepath.Join(dir, f.Name())) == nil {
				freed += info.Size()
			}
		}
	}
	return freed
}

// Loader owns a bounded pool and emits loaded(url, bytes) per request.
type Loader struct {
	Client *core.ApiClient
	Loaded func(url string, data []byte)
	Failed func(url, errText string)

	dir        string
	sem        chan struct{}
	wg         sync.WaitGroup
	mu         sync.Mutex
	inflight   map[string]context.Context
	shutdown   bool
	ctx        context.Context
	cancel     context.CancelFunc
	capMB      atomic.Int64
	transfers  *core.MetadataCache
	generation uint64
}

// NewLoader builds a loader with maxThreads workers (Python default 8).
func NewLoader(maxThreads int) *Loader {
	return NewLoaderWithCap(maxThreads, 512)
}

// NewLoaderWithCap applies the saved limit before the startup eviction.
func NewLoaderWithCap(maxThreads int, capMB int64) *Loader {
	if maxThreads < 1 {
		maxThreads = 8
	}
	c := core.NewClient()
	c.Timeout = 25 * timeSeconds
	c.MaxRetries = 2
	ctx, cancel := context.WithCancel(context.Background())
	l := &Loader{
		Client:   c,
		sem:      make(chan struct{}, maxThreads),
		inflight: map[string]context.Context{},
		dir:      CacheDir(),
		ctx:      ctx, cancel: cancel,
		transfers: core.NewMetadataCache(),
	}
	l.SetCap(capMB)
	// trim any overflow left by previous sessions, off the caller's thread
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		Prune(l.capMB.Load()*1024*1024, l.dir)
	}()
	return l
}

const timeSeconds = 1e9 // ns

// Request loads url (cache-first), delivering asynchronously. Duplicate
// in-flight requests are dropped like Python's _inflight set.
func (l *Loader) Request(url string) {
	l.mu.Lock()
	if l.shutdown || url == "" || l.inflight[url] != nil {
		l.mu.Unlock()
		return
	}
	ctx := l.ctx
	l.inflight[url] = ctx
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		defer func() {
			l.mu.Lock()
			if l.inflight[url] == ctx {
				delete(l.inflight, url)
			}
			l.mu.Unlock()
		}()
		data, err := l.GetWithClient(core.WithRequestPriority(ctx, core.PriorityThumbnail), l.Client, url)
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err != nil {
			if url != "" && l.Failed != nil {
				l.Failed(url, err.Error())
			}
			return
		}
		if data == nil {
			if l.Failed != nil {
				l.Failed(url, "empty")
			}
			return
		}
		if l.Loaded != nil {
			l.Loaded(url, data)
		}
	}()
}

func (l *Loader) cachePath(url string) string {
	return l.cachePathFor(l.Client, url)
}
func (l *Loader) cachePathFor(c *core.ApiClient, url string) string {
	h := sha1.Sum([]byte(url + "\x00" + c.AuthIdentity()))
	return filepath.Join(l.dir, hex.EncodeToString(h[:])+".img")
}

// load returns cached bytes when present and a real image; else fetches and
// caches. A poisoned entry (an HTML hotlink page cached by an older build)
// is dropped so the next fetch replaces it — the cache self-heals.
func (l *Loader) load(ctx context.Context, url string) ([]byte, error) {
	return l.GetWithClient(ctx, l.Client, url)
}

// GetWithClient lets the authenticated media proxy and thumbnail events share
// disk entries and transfers without sharing mutable operation contexts.
func (l *Loader) GetWithClient(ctx context.Context, c *core.ApiClient, url string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c = c.Clone(ctx)
	key := url + "\x00" + c.AuthIdentity()
	return l.transfers.Fetch(ctx, key, 0, func(workCtx context.Context) ([]byte, error) {
		l.mu.Lock()
		if l.shutdown {
			l.mu.Unlock()
			return nil, core.DownloadCancelled
		}
		l.wg.Add(1)
		generation := l.generation
		l.mu.Unlock()
		defer l.wg.Done()
		select {
		case l.sem <- struct{}{}:
		case <-workCtx.Done():
			return nil, core.DownloadCancelled
		}
		defer func() { <-l.sem }()
		return l.loadClient(workCtx, c, url, generation)
	})
}
func (l *Loader) loadClient(ctx context.Context, c *core.ApiClient, url string, generation uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := l.cachePathFor(c, url)
	if data, err := os.ReadFile(path); err == nil {
		if isImage(data) {
			// mark recently used so the LRU sweep keeps hot thumbnails
			now := timeNow()
			_ = os.Chtimes(path, now, now)
			return data, nil
		}
		_ = os.Remove(path)
	} else if err != nil && !os.IsNotExist(err) && !isPathErr(err) {
		slog.Debug("cache read", "err", err)
	}
	// fetch + cache; a non-image response (block/HTML page) comes back
	// uncached as nil, so a transient hotlink block is a clean miss rather
	// than a poisoned cache entry
	data, err := c.FetchBytes(ctx, url)
	if err != nil {
		if ctx.Err() == nil {
			slog.Debug("Миниатюра: не загружена", "url", core.RedactSecrets(url), "rate_limited", core.IsRateLimited(err), "err", err)
		}
		return nil, err
	}
	if !isImage(data) {
		slog.Debug("Миниатюра: ответ не изображение", "url", core.RedactSecrets(url), "bytes", len(data))
		return nil, fmt.Errorf("invalid image response")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Generations can overlap during cancellation; use a private temp file.
	if tmp, e := os.CreateTemp(l.dir, "thumb-*.part"); e == nil {
		name := tmp.Name()
		_, e = tmp.Write(data)
		closeErr := tmp.Close()
		l.mu.Lock()
		if e == nil && closeErr == nil && ctx.Err() == nil && l.generation == generation {
			_ = os.Rename(name, path)
		}
		l.mu.Unlock()
		_ = os.Remove(name)
	}
	if noteWrite() {
		Prune(l.capMB.Load()*1024*1024, l.dir)
	}
	return data, nil
}

func (l *Loader) ClearCache() int64 {
	slog.Info("Кэш миниатюр: очистка")
	l.ClearPending()
	l.transfers.Clear()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.generation++
	return Clear(l.dir)
}

func isPathErr(err error) bool { return false }

// ClearPending cancels queued thumbnails (in-flight ones observe stop).
func (l *Loader) ClearPending() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shutdown {
		return
	}
	l.cancel()
	l.transfers.Clear()
	l.ctx, l.cancel = context.WithCancel(context.Background())
	l.inflight = map[string]context.Context{}
}

// Shutdown cancels queued thumbnails and joins active tasks synchronously.
func (l *Loader) Shutdown() {
	l.mu.Lock()
	if l.shutdown {
		l.mu.Unlock()
		return
	}
	l.shutdown = true
	l.transfers.Close()
	l.cancel()
	l.mu.Unlock()
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	<-done
	l.Client.CloseIdle()
}

func (l *Loader) SetCap(mb int64) {
	if mb <= 0 {
		mb = 512
	}
	l.capMB.Store(max(64, mb))
}
func (l *Loader) Bytes(url string) ([]byte, error) {
	l.mu.Lock()
	if l.shutdown {
		l.mu.Unlock()
		return nil, core.DownloadCancelled
	}
	ctx := l.ctx
	l.wg.Add(1)
	l.mu.Unlock()
	defer l.wg.Done()
	return l.load(ctx, url)
}
