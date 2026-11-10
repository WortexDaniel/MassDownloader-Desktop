package main

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"massdownloader/internal/core"
	"massdownloader/internal/reverse"
)

// traceCall logs a binding's start at DEBUG and returns the matching finish
// logger: DEBUG on success or cancellation, WARN on failure. attrs must never
// carry credentials; values still pass through the log redaction.
func traceCall(name string, attrs ...any) func(err error, result ...any) {
	start := time.Now()
	slog.Debug(name+": старт", attrs...)
	return func(err error, result ...any) {
		args := append(append([]any{}, attrs...), result...)
		args = append(args, "elapsed_ms", time.Since(start).Milliseconds())
		switch {
		case err == nil:
			slog.Debug(name+": готово", args...)
		case errors.Is(err, core.DownloadCancelled) || errors.Is(err, context.Canceled):
			slog.Debug(name+": отменено", args...)
		default:
			slog.Warn(name+": ошибка", append(args, "err", err)...)
		}
	}
}

func (a *App) searchPage(ctx context.Context, siteKey, query string, cursor any) (PreviewPage, error) {
	done := traceCall("Поиск", "site", siteKey, "query", query, "cursor", cursor)
	page, err := a.runSearchPage(ctx, siteKey, query, cursor)
	done(err, "posts", len(page.Posts), "has_more", page.HasMore)
	return page, err
}

func (a *App) feedPage(ctx context.Context, siteKey, query string, cursor any) (PreviewPage, error) {
	done := traceCall("Лента: страница", "site", siteKey, "query", query, "cursor", cursor)
	page, err := a.runFeedPage(ctx, siteKey, query, cursor)
	done(err, "posts", len(page.Posts), "has_more", page.HasMore)
	return page, err
}

func (a *App) resolvePost(ctx context.Context, p *core.PostInfo) (*core.PostInfo, error) {
	attrs := []any{}
	if p != nil {
		attrs = append(attrs, "site", p.Site, "id", p.ID.String())
	}
	done := traceCall("Пост: загрузка деталей", attrs...)
	out, err := a.runResolvePost(ctx, p)
	done(err)
	return out, err
}

func (a *App) expandPost(ctx context.Context, post *core.PostInfo) []*core.PostInfo {
	done := traceCall("Пост: страницы альбома", "site", post.Site, "id", post.ID.String())
	out := a.runExpandPost(ctx, post)
	done(nil, "pages", len(out))
	return out
}

// StartRun starts a scan+download wave.
func (a *App) StartRun(req StartRequest) error {
	slog.Info("Загрузка: запуск", "site", req.SiteKey, "query", req.Query, "max_posts", req.MaxPosts,
		"threads", req.Threads, "dedup", req.DedupMode, "by_artist", req.ByArtist)
	err := a.startRun(req)
	if err != nil {
		slog.Warn("Загрузка: запуск отклонён", "site", req.SiteKey, "err", err)
	}
	return err
}

// DownloadSelection queues explicitly chosen posts.
func (a *App) DownloadSelection(posts []*core.PostInfo, root string, byArtist bool, artistTags []string) int {
	queued := a.downloadSelection(posts, root, byArtist, artistTags)
	slog.Info("Загрузка: выбранные посты", "posts", len(posts), "queued", queued, "by_artist", byArtist)
	return queued
}

// RetryFailed re-queues the failed jobs of the last waves.
func (a *App) RetryFailed() (int, error) {
	n, err := a.retryFailed()
	if err != nil {
		slog.Warn("Загрузка: повтор ошибок не запущен", "err", err)
	} else {
		slog.Info("Загрузка: повтор ошибок", "jobs", n)
	}
	return n, err
}

// ReverseSearch runs one provider against raw image bytes.
func (a *App) ReverseSearch(providerKey, filename string, image []byte) ([]reverse.Match, error) {
	done := traceCall("Обратный поиск", "provider", providerKey, "bytes", len(image))
	out, err := a.reverseSearch(providerKey, filename, image)
	done(err, "matches", len(out))
	return out, err
}

// SetCredentials replaces all credentials; only site keys are logged.
func (a *App) SetCredentials(creds map[string]map[string]string) error {
	keys := make([]string, 0, len(creds))
	for k := range creds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	err := a.setCredentials(creds)
	if err != nil {
		slog.Warn("Авторизация: не сохранена", "err", err)
	} else {
		slog.Info("Авторизация обновлена", "sites", keys)
	}
	return err
}

// SetProxySettings applies and stores the proxy; the password is never logged.
func (a *App) SetProxySettings(config core.ProxyConfig) error {
	err := a.setProxySettings(config)
	if err != nil {
		slog.Warn("Прокси: настройки отклонены", "mode", config.Mode, "err", err)
	} else {
		slog.Info("Прокси: настройки применены", "mode", config.Mode, "type", config.Type,
			"host", config.Host, "port", config.Port, "auth", config.Username != "")
	}
	return err
}

// PostByID fetches one post by its site ID.
func (a *App) PostByID(siteKey, id string) (*core.PostInfo, error) {
	done := traceCall("Пост по ID", "site", siteKey, "id", id)
	out, err := a.postByID(siteKey, id)
	done(err)
	return out, err
}
