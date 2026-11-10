// Package videocache stores complete viewer videos on disk with an LRU cap.
package videocache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const DefaultMB int64 = 2048

func LimitMB(mb int64) int64 {
	if mb <= 0 {
		return DefaultMB
	}
	return max(64, min(65536, mb))
}

type flight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	err     error
	cleared bool
}

type Cache struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	dir     string
	cap     int64
	pending map[string]*flight
	readers map[string]int
	discard map[string]bool
	closed  bool
}

func New(dir string, mb int64) *Cache {
	c := &Cache{dir: dir, cap: LimitMB(mb) * 1048576, pending: map[string]*flight{}, readers: map[string]int{}, discard: map[string]bool{}}
	_ = os.MkdirAll(dir, 0755)
	// No transfers exist yet; partial files from an interrupted run are stale.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".part") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	c.pruneLocked()
	return c
}

// Open deduplicates downloads and pins the returned file until release.
// fetch must produce a complete, validated file. Cancelled/failed transfers
// never become cache entries. Neither the full video nor its ranges enter RAM.
func (c *Cache) Open(ctx context.Context, ref, ext string, fetch func(context.Context, string) error) (*os.File, func(), error) {
	key := fmt.Sprintf("%x.%s", sha256.Sum256([]byte(ref)), ext)
	path := filepath.Join(c.dir, key)
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, nil, context.Canceled
		}
		if file, release, err := c.openLocked(path); err == nil {
			c.mu.Unlock()
			return file, release, nil
		}
		if job := c.pending[key]; job != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-job.done:
			}
			if job.cleared {
				return nil, nil, context.Canceled
			}
			if job.err != nil && job.err != context.Canceled {
				return nil, nil, job.err
			}
			continue
		}
		workCtx, cancel := context.WithCancel(ctx)
		job := &flight{done: make(chan struct{}), cancel: cancel}
		c.pending[key] = job
		c.wg.Add(1)
		c.mu.Unlock()

		tmp, err := os.CreateTemp(c.dir, "video-*.part")
		var name string
		if err == nil {
			name = tmp.Name()
			err = tmp.Close()
			if err == nil {
				err = fetch(workCtx, name)
			}
		}
		c.mu.Lock()
		if workCtx.Err() != nil {
			err = workCtx.Err()
		}
		if err == nil {
			err = os.Rename(name, path)
		}
		if name != "" {
			_ = os.Remove(name)
		}
		job.err = err
		delete(c.pending, key)
		close(job.done)
		cancel()
		c.wg.Done()
		if err != nil {
			c.mu.Unlock()
			return nil, nil, err
		}
		// Pin the completed file before pruning so even oversized videos play.
		file, release, err := c.openLocked(path)
		c.pruneLocked()
		c.mu.Unlock()
		return file, release, err
	}
}

func (c *Cache) openLocked(path string) (*os.File, func(), error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		file.Close()
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("invalid cached video")
	}
	c.readers[path]++
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	var once sync.Once
	return file, func() {
		once.Do(func() {
			file.Close()
			c.mu.Lock()
			defer c.mu.Unlock()
			c.readers[path]--
			if c.readers[path] == 0 {
				delete(c.readers, path)
				if c.discard[path] {
					_ = os.Remove(path)
					delete(c.discard, path)
				}
			}
			c.pruneLocked()
		})
	}, nil
}

// Stats includes partial download bytes, but counts only completed videos.
func (c *Cache) Stats() (bytes int64, count, loading int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, _ := os.ReadDir(c.dir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if st, err := e.Info(); err == nil && st.Mode().IsRegular() {
			bytes += st.Size()
			if !strings.HasSuffix(e.Name(), ".part") {
				count++
			}
		}
	}
	return bytes, count, len(c.pending)
}

// Clear cancels transfers and removes inactive files. Open playback files stay
// pinned; no producer can commit a cancelled download after this call.
func (c *Cache) Clear() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, job := range c.pending {
		job.cleared = true
		job.cancel()
	}
	var freed int64
	entries, _ := os.ReadDir(c.dir)
	for _, e := range entries {
		path := filepath.Join(c.dir, e.Name())
		if e.IsDir() {
			continue
		}
		if c.readers[path] > 0 {
			c.discard[path] = true
			continue
		}
		if st, err := e.Info(); err == nil && st.Mode().IsRegular() {
			if os.Remove(path) == nil {
				freed += st.Size()
			}
		}
	}
	return freed
}

func (c *Cache) SetCap(mb int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cap = LimitMB(mb) * 1048576
	return c.pruneLocked()
}

func (c *Cache) pruneLocked() int64 {
	type entry struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []entry
	var total, freed int64
	entries, _ := os.ReadDir(c.dir)
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		if st, err := e.Info(); err == nil && st.Mode().IsRegular() {
			path := filepath.Join(c.dir, e.Name())
			total += st.Size()
			if c.readers[path] == 0 {
				files = append(files, entry{path, st.Size(), st.ModTime()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	for _, f := range files {
		if total-freed <= c.cap {
			break
		}
		if os.Remove(f.path) == nil {
			freed += f.size
		}
	}
	return freed
}

func (c *Cache) Close() {
	c.mu.Lock()
	c.closed = true
	for _, job := range c.pending {
		job.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
}
