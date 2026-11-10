package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Only this parent process owns the databases, authenticated clients and queue.
// Child windows use an authenticated loopback bridge instead of reopening stores.
type feedServer struct {
	app             *App
	server          *http.Server
	endpoint, token string
	mu              sync.Mutex
	subs            map[chan feedEvent]bool
	children        map[*exec.Cmd]bool
	allowed         map[string]bool
	focusEvent      string
}
type feedEvent struct {
	Name string `json:"name"`
	Data []any  `json:"data"`
}
type feedCall struct {
	Method string            `json:"method"`
	Args   []json.RawMessage `json:"args"`
}
type feedReply struct {
	Value any    `json:"value"`
	Error string `json:"error,omitempty"`
}

var feedMethods = map[string]bool{
	"Sites": true, "FilterDefaults": true, "BuildQuery": true, "GetAllSettings": true, "GetSetting": true,
	"FeedFilters": true, "SaveFeedFilter": true, "DeleteFeedFilter": true, "FeedSources": true, "FeedPage": true, "SaveFeedText": true,
	"SearchPage": true, "SearchArtists": true, "Autocomplete": true, "ResolvePost": true, "ExpandPost": true,
	"DownloadSelection": true, "DownloadStates": true, "LocalFileURL": true, "LocalMedia": true, "RevealPost": true,
	"PostQueued": true,
	"Permalink":  true, "ArtistsForPost": true, "ArtistQuery": true, "FavIs": true, "FavToggle": true,
	"Authors": true, "AuthorToggle": true, "AuthorRemove": true, "MarkAuthorViewed": true, "Thumb": true,
	"GetCredentials": true, "SetCredentials": true,
	"RefreshMetadata": true,
	"RunOperation":    true, "CancelOperation": true,
	"ClipboardSetImage": true,
	"MarkPostViewed":    true, "MarkAllAuthorViewed": true,
}

var libraryMethods = map[string]bool{
	"Sites": true, "GetAllSettings": true, "GetSetting": true, "Logs": true, "ClearLogs": true,
	"LibraryInfo": true, "LibraryFiles": true, "LibraryDuplicates": true, "LibraryPreview": true, "LibraryApply": true,
	"LibraryOperations": true, "LibraryRestore": true, "LibraryIgnore": true, "LibraryFileURL": true, "LibraryReveal": true,
	"LibraryStatus": true, "LibraryScan": true, "LibraryCancel": true,
	"LibrarySelect": true, "LibraryPurge": true,
	"LibraryRescan":    true,
	"LibraryThumbnail": true,
	"LibraryAuthors":   true, "LibraryAuthorCandidates": true, "LibraryIdentities": true, "LibraryIgnoreAuthors": true,
	"LibrarySetPrefs": true, "LibraryPurgeAll": true,
}

func newFeedServer(a *App) (*feedServer, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &feedServer{app: a, endpoint: "http://" + listener.Addr().String(), token: randomFeedID(), subs: map[chan feedEvent]bool{}, children: map[*exec.Cmd]bool{}}
	s.server = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second}
	go s.server.Serve(listener)
	return s, nil
}
func (a *App) OpenFeed() error {
	a.feedMu.Lock()
	defer a.feedMu.Unlock()
	if a.feed == nil {
		var err error
		a.feed, err = newFeedServer(a)
		if err != nil {
			return err
		}
	}
	if a.feed.focusExisting() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--feed-window")
	cmd.Env = append(os.Environ(), "MASSDOWNLOADER_FEED_ENDPOINT="+a.feed.endpoint, "MASSDOWNLOADER_FEED_TOKEN="+a.feed.token)
	configureFeedProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	s := a.feed
	s.mu.Lock()
	s.children[cmd] = true
	s.mu.Unlock()
	go func() { _ = cmd.Wait(); s.mu.Lock(); delete(s.children, cmd); s.mu.Unlock() }()
	return nil
}
func (a *App) OpenLibrary() error {
	a.feedMu.Lock()
	defer a.feedMu.Unlock()
	if a.libraryWindow == nil {
		var err error
		a.libraryWindow, err = newFeedServer(a)
		if err != nil {
			return err
		}
		a.libraryWindow.allowed = libraryMethods
		a.libraryWindow.focusEvent = "library:focus"
	}
	s := a.libraryWindow
	if s.focusExisting() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--library-window")
	cmd.Env = append(os.Environ(), "MASSDOWNLOADER_FEED_ENDPOINT="+s.endpoint, "MASSDOWNLOADER_FEED_TOKEN="+s.token)
	configureFeedProcess(cmd)
	if err = cmd.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.children[cmd] = true
	s.mu.Unlock()
	go func() { _ = cmd.Wait(); s.mu.Lock(); delete(s.children, cmd); s.mu.Unlock() }()
	return nil
}
func (s *feedServer) focusExisting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.children) == 0 {
		return false
	}
	for ch := range s.subs {
		select {
		case ch <- feedEvent{Name: s.focusName()}:
		default:
		}
	}
	return true
}
func (s *feedServer) focusName() string {
	if s.focusEvent != "" {
		return s.focusEvent
	}
	return "feed:focus"
}
func (a *App) closeFeed() {
	a.feedMu.Lock()
	s := a.feed
	libraryWindow := a.libraryWindow
	a.feed = nil
	a.libraryWindow = nil
	a.feedMu.Unlock()
	for _, server := range []*feedServer{s, libraryWindow} {
		if server == nil {
			continue
		}
		_ = server.server.Close()
		server.mu.Lock()
		for child := range server.children {
			_ = child.Process.Kill()
		}
		server.mu.Unlock()
	}
}
func (a *App) emitFeed(name string, data ...any) {
	a.feedMu.Lock()
	s := a.feed
	libraryWindow := a.libraryWindow
	a.feedMu.Unlock()
	for _, server := range []*feedServer{s, libraryWindow} {
		if server == nil {
			continue
		}
		server.mu.Lock()
		for ch := range server.subs {
			select {
			case ch <- feedEvent{name, data}:
			default:
			}
		}
		server.mu.Unlock()
	}
}
func (s *feedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Feed-Token")), []byte(s.token)) != 1 || r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/rpc":
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		var call feedCall
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&call); err != nil {
			http.Error(w, "invalid call", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.invokeContext(r.Context(), call))
	case "/events":
		if r.Method != http.MethodGet {
			http.Error(w, "method", 405)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unavailable", 500)
			return
		}
		ch := make(chan feedEvent, 128)
		s.mu.Lock()
		s.subs[ch] = true
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.subs, ch); s.mu.Unlock() }()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		flusher.Flush()
		encoder := json.NewEncoder(w)
		heartbeat := time.NewTicker(20 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case event := <-ch:
				if encoder.Encode(event) != nil {
					return
				}
				flusher.Flush()
			case <-heartbeat.C:
				if encoder.Encode(feedEvent{Name: "feed:heartbeat"}) != nil {
					return
				}
				flusher.Flush()
			}
		}
	case "/media", "/local-media":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method", 405)
			return
		}
		s.app.serveMedia(w, r)
	default:
		http.NotFound(w, r)
	}
}
func (s *feedServer) invoke(call feedCall) (reply feedReply) {
	return s.invokeContext(context.Background(), call)
}
func (s *feedServer) invokeContext(ctx context.Context, call feedCall) (reply feedReply) {
	defer func() {
		if recover() != nil {
			reply = feedReply{Error: "ошибка запроса ленты"}
		}
	}()
	allowed := s.allowed
	if allowed == nil {
		allowed = feedMethods
	}
	if !allowed[call.Method] {
		return feedReply{Error: "метод недоступен"}
	}
	method := reflect.ValueOf(s.app).MethodByName(call.Method)
	if !method.IsValid() || method.Type().NumIn() != len(call.Args) {
		return feedReply{Error: "неверные аргументы"}
	}
	args := make([]reflect.Value, len(call.Args))
	for i, raw := range call.Args {
		value := reflect.New(method.Type().In(i))
		if err := json.Unmarshal(raw, value.Interface()); err != nil {
			return feedReply{Error: err.Error()}
		}
		args[i] = value.Elem()
	}
	if call.Method == "RunOperation" {
		value, err := s.app.runOperation(ctx, args[0].String(), args[1].String(), args[2].Interface().([]json.RawMessage))
		if err != nil {
			return feedReply{Error: err.Error()}
		}
		return feedReply{Value: value}
	}
	if call.Method == "FeedPage" {
		page, err := s.app.feedPage(ctx, args[0].String(), args[1].String(), args[2].Interface())
		if err != nil {
			return feedReply{Error: err.Error()}
		}
		return feedReply{Value: page}
	}
	for _, value := range method.Call(args) {
		if value.Type().Implements(reflect.TypeFor[error]()) {
			if !value.IsNil() {
				reply.Error = value.Interface().(error).Error()
			}
		} else {
			reply.Value = value.Interface()
		}
	}
	return reply
}

type FeedWindow struct {
	endpoint, token string
	ctx             context.Context
	cancel          context.CancelFunc
	client          *http.Client
	kind            string
}

func (f *FeedWindow) Mode() string {
	if f.kind == "library" {
		return "library"
	}
	return "feed"
}
func (f *FeedWindow) PickDirectory() (string, error) {
	return wruntime.OpenDirectoryDialog(f.ctx, wruntime.OpenDialogOptions{Title: "Выбрать папку библиотеки"})
}

func (f *FeedWindow) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, f.endpoint+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("X-Feed-Token", f.token)
	return f.client.Do(r)
}
func (f *FeedWindow) Call(method string, args []json.RawMessage) (any, error) {
	body, _ := json.Marshal(feedCall{method, args})
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Minute)
	defer cancel()
	resp, err := f.request(ctx, http.MethodPost, "/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("главное окно недоступно: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ошибка ленты: HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Value json.RawMessage `json:"value"`
		Error string          `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, err
	}
	if reply.Error != "" {
		return nil, fmt.Errorf("%s", reply.Error)
	}
	// RawMessage preserves 64-bit IDs through the second WebView binding boundary.
	return reply.Value, nil
}
func (f *FeedWindow) startup(ctx context.Context) {
	f.ctx, f.cancel = context.WithCancel(ctx)
	go f.events()
}
func (f *FeedWindow) show(ctx context.Context) {
	wruntime.WindowShow(ctx)
}
func (f *FeedWindow) events() {
	failures := 0
	for f.ctx.Err() == nil {
		resp, err := f.request(f.ctx, http.MethodGet, "/events", nil)
		if err == nil && resp.StatusCode == 200 {
			failures = 0
			decoder := json.NewDecoder(resp.Body)
			for {
				var event feedEvent
				if decoder.Decode(&event) != nil {
					break
				}
				if event.Name == "feed:focus" || event.Name == "library:focus" {
					f.show(f.ctx)
				} else if event.Name != "feed:heartbeat" {
					wruntime.EventsEmit(f.ctx, event.Name, event.Data...)
				}
			}
		}
		if resp != nil {
			resp.Body.Close()
		}
		failures++
		if failures >= 2 && f.ctx.Err() == nil {
			wruntime.Quit(f.ctx)
			return
		}
		select {
		case <-f.ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
func (f *FeedWindow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/media" && r.URL.Path != "/local-media" {
		http.NotFound(w, r)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, f.endpoint+r.URL.RequestURI(), nil)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	request.Header.Set("X-Feed-Token", f.token)
	for _, key := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := r.Header.Get(key); value != "" {
			request.Header.Set(key, value)
		}
	}
	resp, err := f.client.Do(request)
	if err != nil {
		http.Error(w, "Главное окно недоступно", 502)
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
