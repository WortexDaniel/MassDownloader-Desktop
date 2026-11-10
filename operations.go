package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"massdownloader/internal/core"
	"sync"
	"time"
)

type operationState struct {
	cancel context.CancelFunc
	at     time.Time
}
type operationRegistry struct {
	mu     sync.Mutex
	states map[string]operationState
	closed bool
}

func (r *operationRegistry) start(parent context.Context, id string) (context.Context, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == "" || len(id) > 200 {
		return nil, nil, fmt.Errorf("invalid operation id")
	}
	if r.closed {
		return nil, nil, core.DownloadCancelled
	}
	if r.states == nil {
		r.states = map[string]operationState{}
	}
	for key, s := range r.states {
		if s.cancel == nil && time.Since(s.at) > 2*time.Minute {
			delete(r.states, key)
		}
	}
	if _, ok := r.states[id]; ok {
		return nil, nil, core.DownloadCancelled
	}
	if len(r.states) >= 4096 {
		return nil, nil, fmt.Errorf("too many operations")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	r.states[id] = operationState{cancel, time.Now()}
	return ctx, func() { cancel(); r.mu.Lock(); delete(r.states, id); r.mu.Unlock() }, nil
}
func (r *operationRegistry) cancel(id string) {
	if id == "" || len(id) > 200 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.states == nil {
		r.states = map[string]operationState{}
	}
	if s, ok := r.states[id]; ok {
		if s.cancel != nil {
			s.cancel()
		}
	} else if len(r.states) < 4096 {
		r.states[id] = operationState{at: time.Now()}
	}
}
func (r *operationRegistry) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for _, s := range r.states {
		if s.cancel != nil {
			s.cancel()
		}
	}
}
func (a *App) CancelOperation(id string) { a.operations.cancel(id) }
func (a *App) RunOperation(id, method string, args []json.RawMessage) (any, error) {
	return a.runOperation(a.ctx, id, method, args)
}
func operationArg[T any](args []json.RawMessage, n int) (T, error) {
	var v T
	err := json.Unmarshal(args[n], &v)
	return v, err
}
func (a *App) runOperation(parent context.Context, id, method string, args []json.RawMessage) (any, error) {
	counts := map[string]int{"SearchPage": 3, "FeedPage": 3, "ResolvePost": 1, "ExpandPost": 1, "Autocomplete": 2, "SearchArtists": 2, "CheckNewPosts": 3, "CheckAuthorsBatch": 2, "Health": 1}
	count, ok := counts[method]
	if !ok || len(args) != count {
		return nil, fmt.Errorf("operation unavailable or invalid arguments")
	}
	ctx, finish, err := a.operations.start(parent, id)
	if err != nil {
		return nil, err
	}
	defer finish()
	ctx = core.WithRequestPriority(ctx, core.PriorityInteractive)
	started := time.Now()
	slog.Debug("Операция: старт", "id", id, "method", method)
	defer func() {
		slog.Debug("Операция: завершена", "id", id, "method", method, "elapsed_ms", time.Since(started).Milliseconds(),
			"cancelled", ctx.Err() != nil)
	}()
	var value any
	if method == "CheckAuthorsBatch" {
		site, e1 := operationArg[string](args, 0)
		queries, e2 := operationArg[[]string](args, 1)
		if e1 != nil || e2 != nil {
			return nil, fmt.Errorf("invalid arguments")
		}
		value, err = a.checkAuthorsBatch(ctx, site, queries)
		if ctx.Err() != nil {
			return nil, core.DownloadCancelled
		}
		return value, err
	}
	if method == "ResolvePost" || method == "ExpandPost" {
		p, e := operationArg[*core.PostInfo](args, 0)
		if e != nil || p == nil {
			return nil, fmt.Errorf("invalid post")
		}
		if method == "ResolvePost" {
			value, err = a.resolvePost(ctx, p)
		} else {
			value = a.expandPost(ctx, p)
		}
	} else {
		site, e := operationArg[string](args, 0)
		if e != nil {
			return nil, e
		}
		c := a.operationClient()
		c.SetContext(ctx)
		if method == "Health" {
			s := core.GetSite(site)
			if s == nil {
				return nil, fmt.Errorf("unknown site")
			}
			value = core.Registry.EngineFor(s).CheckAvailable(c, s)
		} else {
			query, e := operationArg[string](args, 1)
			if e != nil {
				return nil, e
			}
			switch method {
			case "SearchPage", "FeedPage":
				cursor, e := operationArg[any](args, 2)
				if e != nil {
					return nil, e
				}
				if method == "FeedPage" {
					value, err = a.feedPage(ctx, site, query, cursor)
				} else {
					value, err = a.searchPage(ctx, site, query, cursor)
				}
			case "CheckNewPosts":
				limit, e := operationArg[int](args, 2)
				if e != nil {
					return nil, e
				}
				value, err = a.checkNewPosts(ctx, site, query, limit)
			case "Autocomplete":
				value, err = c.AutocompleteTags(ctx, site, query)
			case "SearchArtists":
				value, err = c.SearchArtists(ctx, site, query, 20)
			}
		}
	}
	if ctx.Err() != nil {
		return nil, core.DownloadCancelled
	}
	return value, err
}
