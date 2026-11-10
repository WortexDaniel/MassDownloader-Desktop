package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"massdownloader/internal/core"
	"strconv"
	"strings"
	"sync"
	"time"
)

type feedBufferEntry struct {
	page                  PreviewPage
	site, query, identity string
	used                  time.Time
	bytes                 int
	finished              bool
}
type feedBuffer struct {
	mu      sync.Mutex
	entries map[string]feedBufferEntry
	bytes   int
}

func (b *feedBuffer) clear() { b.mu.Lock(); defer b.mu.Unlock(); b.entries = nil; b.bytes = 0 }
func (b *feedBuffer) read(cursor, site, query, identity string) (PreviewPage, error) {
	slog.Debug("Лента: порция из буфера", "site", site, "cursor", cursor)
	parts := strings.Split(cursor, ":")
	if len(parts) != 3 {
		return PreviewPage{}, fmt.Errorf("некорректный курсор ленты")
	}
	offset, err := strconv.Atoi(parts[2])
	if err != nil || offset < 10 || offset%10 != 0 {
		return PreviewPage{}, fmt.Errorf("некорректный курсор ленты")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[parts[1]]
	if !ok || time.Since(entry.used) > 10*time.Minute || entry.site != site || entry.query != query || entry.identity != identity {
		return PreviewPage{}, fmt.Errorf("страница ленты истекла или изменился запрос; обновите ленту")
	}
	if offset >= len(entry.page.Posts) {
		return PreviewPage{}, fmt.Errorf("некорректная страница ленты")
	}
	entry.used = time.Now()
	entry.finished = entry.finished || offset+10 >= len(entry.page.Posts)
	b.entries[parts[1]] = entry
	return feedChunk(entry.page, parts[1], offset), nil
}
func feedChunk(page PreviewPage, token string, offset int) PreviewPage {
	end := min(offset+10, len(page.Posts))
	out := PreviewPage{Next: page.Next, HasMore: page.HasMore, Posts: make([]*core.PostInfo, 0, end-offset)}
	for _, p := range page.Posts[offset:end] {
		out.Posts = append(out.Posts, p.Clone())
	}
	if end < len(page.Posts) {
		out.Next = fmt.Sprintf("fb:%s:%d", token, end)
		out.HasMore = true
	}
	return out
}
func (b *feedBuffer) store(page PreviewPage, site, query, identity string) (PreviewPage, error) {
	if len(page.Posts) <= 10 {
		return page, nil
	}
	data, err := json.Marshal(page)
	if err != nil {
		return PreviewPage{}, err
	}
	if len(data) > 8<<20 {
		return PreviewPage{}, fmt.Errorf("страница ленты слишком велика")
	}
	var immutable PreviewPage
	if err = core.DecodeJSON(data, &immutable); err != nil {
		return PreviewPage{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[string]feedBufferEntry{}
	}
	for key, entry := range b.entries {
		if time.Since(entry.used) > 10*time.Minute {
			b.bytes -= entry.bytes
			delete(b.entries, key)
		}
	}
	// Never evict a live cursor and silently reconstruct a different page.
	if len(b.entries) >= 512 || b.bytes+len(data) > 32<<20 {
		return PreviewPage{}, fmt.Errorf("слишком много активных страниц ленты; обновите ленту")
	}
	token := randomFeedID()
	b.entries[token] = feedBufferEntry{page: immutable, site: site, query: query, identity: identity, used: time.Now(), bytes: len(data)}
	b.bytes += len(data)
	return feedChunk(immutable, token, 0), nil
}

// Receiving the engine cursor proves the last buffered chunk was acknowledged.
// Keep retryable chunks until then, and free completed pages before fetching more.
func (b *feedBuffer) acknowledge(site, query, identity string, cursor any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, entry := range b.entries {
		if entry.finished && entry.site == site && entry.query == query && entry.identity == identity && fmt.Sprint(entry.page.Next) == fmt.Sprint(cursor) {
			b.bytes -= entry.bytes
			delete(b.entries, key)
		}
	}
}
func feedFetchLimit(site *core.Site) int {
	switch site.Engine {
	case "e621", "danbooru", "moebooru", "gelbooru", "gelbooru_com", "gelbooru_legacy", "sankaku":
		return min(30, site.PageLimit)
	}
	return min(10, site.PageLimit)
}
