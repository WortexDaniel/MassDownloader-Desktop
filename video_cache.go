package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"massdownloader/internal/appdir"
	"massdownloader/internal/core"
	"massdownloader/internal/videocache"
)

func (a *App) videos() *videocache.Cache {
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	if a.videoCache == nil {
		mb := videocache.DefaultMB
		if a.settings != nil {
			mb = a.settings.GetInt("video_cache_mb")
		}
		a.videoCache = videocache.New(filepath.Join(appdir.Dir(), "video_tmp"), mb)
	}
	return a.videoCache
}

func videoExtension(ref, hint string) string {
	valid := func(ext string) bool { return ext == "mp4" || ext == "webm" || ext == "mov" || ext == "m4v" }
	if ext := strings.ToLower(hint); valid(ext) {
		return ext
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if u.Scheme == "danbooru-media" {
		return videoExtension(u.Query().Get("primary"), "")
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(u.Path), "."))
	if valid(ext) {
		return ext
	}
	return ""
}

func (a *App) serveVideo(w http.ResponseWriter, r *http.Request, c *core.ApiClient, ref, ext string) {
	file, release, err := a.videos().Open(r.Context(), ref, ext, func(ctx context.Context, path string) error {
		return fetchVideo(ctx, c, ref, path)
	})
	if err != nil {
		if r.Context().Err() == nil {
			http.Error(w, "video unavailable", http.StatusBadGateway)
		}
		return
	}
	defer release()
	st, err := file.Stat()
	if err != nil {
		http.Error(w, "video unavailable", http.StatusBadGateway)
		return
	}
	// ServeContent handles seeking, suffix ranges, HEAD and invalid ranges.
	http.ServeContent(w, r, "video."+ext, st.ModTime(), file)
}

func videoSignature(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	return string(data[:4]) == "\x1a\x45\xdf\xa3" ||
		string(data[4:8]) == "ftyp" || string(data[4:8]) == "moov" || string(data[4:8]) == "mdat"
}

// Download a complete video through the existing authenticated candidate policy.
// Partial/HTML responses never enter the cache. Streams go straight to disk.
func fetchVideo(ctx context.Context, c *core.ApiClient, ref, path string) error {
	var lastErr error
	for _, raw := range c.MediaURLs([]string{ref}) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept-Encoding", "identity")
		for key, value := range c.ImgHeaders(raw) {
			req.Header.Set(key, value)
		}
		resp, err := c.DoMedia(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			slog.Warn("Видео: лимит сайта", "host", req.URL.Host)
			return &core.ApiError{Status: 429, Msg: "HTTP 429 — сайт временно ограничил запросы"}
		}
		reader := bufio.NewReader(resp.Body)
		head, _ := reader.Peek(12)
		if resp.StatusCode != http.StatusOK || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") || !videoSignature(head) {
			resp.Body.Close()
			lastErr = fmt.Errorf("invalid video response (HTTP %d)", resp.StatusCode)
			slog.Debug("Видео: неподходящий ответ, пробую следующий URL", "host", req.URL.Host, "status", resp.StatusCode)
			continue
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			resp.Body.Close()
			return err
		}
		n, copyErr := io.Copy(file, reader)
		closeErr := file.Close()
		resp.Body.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if copyErr != nil {
			lastErr = copyErr
			slog.Warn("Видео: обрыв загрузки в кэш", "host", req.URL.Host, "bytes", n, "err", copyErr)
			continue
		}
		if closeErr != nil {
			return closeErr
		}
		if resp.ContentLength >= 0 && n != resp.ContentLength {
			lastErr = io.ErrUnexpectedEOF
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no video URL")
	}
	return lastErr
}
