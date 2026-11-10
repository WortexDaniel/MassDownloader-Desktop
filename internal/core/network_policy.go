package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type priorityKey struct{}

const (
	PriorityInteractive = 0
	PriorityNormal      = 2
	PriorityThumbnail   = 3
	PriorityBackground  = 4
)

func WithRequestPriority(ctx context.Context, priority int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, priorityKey{}, priority)
}

type networkWaiter struct {
	ready    chan struct{}
	priority int
	at       time.Time
	granted  bool
}
type networkGate struct {
	active, limit, ceiling, successes int
	queue                             []*networkWaiter
}
type NetworkHostStats struct {
	Requests, RateLimited, Failures, Bytes uint64
	Active, Queued                         int
	WaitMillis, TransferMillis             int64
}
type networkScheduler struct {
	mu    sync.Mutex
	gates map[string]*networkGate
	stats map[string]NetworkHostStats
}

var network = &networkScheduler{gates: map[string]*networkGate{}, stats: map[string]NetworkHostStats{}}

func (n *networkScheduler) dispatch(g *networkGate) {
	for g.active < g.limit && len(g.queue) > 0 {
		best := 0
		score := func(w *networkWaiter) int { return w.priority - int(time.Since(w.at)/(2*time.Second)) }
		for i := 1; i < len(g.queue); i++ {
			if score(g.queue[i]) < score(g.queue[best]) {
				best = i
			}
		}
		w := g.queue[best]
		g.queue = append(g.queue[:best], g.queue[best+1:]...)
		g.active++
		w.granted = true
		close(w.ready)
	}
}
func (n *networkScheduler) acquire(ctx context.Context, key string, ceiling int) (func(), error) {
	w := &networkWaiter{ready: make(chan struct{}), priority: PriorityNormal, at: time.Now()}
	if p, ok := ctx.Value(priorityKey{}).(int); ok {
		w.priority = p
	}
	n.mu.Lock()
	g := n.gates[key]
	if g == nil {
		g = &networkGate{limit: ceiling, ceiling: ceiling}
		n.gates[key] = g
	}
	g.queue = append(g.queue, w)
	n.dispatch(g)
	n.mu.Unlock()
	select {
	case <-ctx.Done():
		n.mu.Lock()
		if w.granted {
			g.active--
		} else {
			for i, v := range g.queue {
				if v == w {
					g.queue = append(g.queue[:i], g.queue[i+1:]...)
					break
				}
			}
		}
		n.dispatch(g)
		n.mu.Unlock()
		return nil, DownloadCancelled
	case <-w.ready:
	}
	n.mu.Lock()
	s := n.stats[key]
	s.WaitMillis += time.Since(w.at).Milliseconds()
	n.stats[key] = s
	n.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { n.mu.Lock(); g.active--; n.dispatch(g); n.mu.Unlock() }) }, nil
}
func (n *networkScheduler) observe(key string, status int, bytes uint64, duration time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	g := n.gates[key]
	s := n.stats[key]
	s.Requests++
	s.Bytes += bytes
	s.TransferMillis += duration.Milliseconds()
	if status == 429 {
		s.RateLimited++
		before := g.limit
		g.limit = max(1, g.limit/2)
		g.successes = 0
		if before != g.limit {
			slog.Debug("Сеть: параллельность снижена", "gate", key, "limit", g.limit, "ceiling", g.ceiling)
		}
	} else if status >= 200 && status < 400 {
		g.successes++
		if g.successes >= 20 && g.limit < g.ceiling {
			g.limit++
			g.successes = 0
			slog.Debug("Сеть: параллельность восстановлена", "gate", key, "limit", g.limit, "ceiling", g.ceiling)
			n.dispatch(g)
		}
	} else {
		s.Failures++
		g.successes = 0
	}
	n.stats[key] = s
}
func NetworkStats() map[string]NetworkHostStats {
	network.mu.Lock()
	defer network.mu.Unlock()
	out := map[string]NetworkHostStats{}
	for key, g := range network.gates {
		s := network.stats[key]
		s.Active = g.active
		s.Queued = len(g.queue)
		out[key] = s
	}
	return out
}

// Timeouts applied after admission: queueing behind the host gate or a zone
// pause never counts against a request's own deadline.
var (
	streamHeaderTimeout = 30 * time.Second
	streamIdleTimeout   = 45 * time.Second
)

// errNetworkTimeout marks a transfer cancelled by its own deadline or idle
// watchdog (not by the caller).
type errNetworkTimeout struct {
	what  string
	after time.Duration
}

func (e *errNetworkTimeout) Error() string {
	return fmt.Sprintf("таймаут сети: %s дольше %s", e.what, e.after)
}
func (e *errNetworkTimeout) Timeout() bool   { return true }
func (e *errNetworkTimeout) Temporary() bool { return true }

type networkBody struct {
	io.ReadCloser
	once     sync.Once
	finish   func(uint64)
	bytes    atomic.Uint64
	timer    *time.Timer
	idle     time.Duration
	cancel   context.CancelFunc
	timedOut *atomic.Bool
	timeout  *errNetworkTimeout
}

func (b *networkBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	b.bytes.Add(uint64(n))
	if n > 0 && b.idle > 0 && b.timer != nil {
		b.timer.Reset(b.idle)
	}
	if e != nil && e != io.EOF && b.timedOut != nil && b.timedOut.Load() {
		return n, b.timeout
	}
	return n, e
}
func (b *networkBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		if b.timer != nil {
			b.timer.Stop()
		}
		if b.cancel != nil {
			b.cancel()
		}
		b.finish(b.bytes.Load())
	})
	return err
}

// policyTransport admits each round-trip through the per-host gate, waits
// for the zone's pace/pause, then applies its own deadline: timeout covers
// headers and body (API, thumbnails); for streams header bounds the wait for
// headers and idle is a watchdog reset by every received chunk.
type policyTransport struct {
	base    http.RoundTripper
	api     bool
	timeout time.Duration
	header  time.Duration
	idle    time.Duration
}

func (p *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	api := p.api
	class, ceiling := "media", 4
	if api {
		class, ceiling = "api", 2
	}
	key := class + "|" + req.URL.Host
	queued := time.Now()
	release, err := network.acquire(req.Context(), key, ceiling)
	if err != nil {
		return nil, err
	}
	if api {
		err = Throttle.waitContext(req.Context(), req.URL.String())
	} else {
		err = Throttle.waitMedia(req.Context(), req.URL.String())
	}
	if err != nil {
		release()
		return nil, err
	}
	priority := PriorityNormal
	if v, ok := req.Context().Value(priorityKey{}).(int); ok {
		priority = v
	}
	start := time.Now()
	slog.Debug("Сеть: запрос", "class", class, "method", req.Method, "host", req.URL.Host,
		"path", RedactSecrets(req.URL.Path), "priority", priority, "queued_ms", start.Sub(queued).Milliseconds())

	sendCtx, cancel := context.WithCancel(req.Context())
	var timedOut atomic.Bool
	limit, what := p.timeout, "ответ"
	if p.idle > 0 {
		limit, what = p.header, "ожидание заголовков"
	}
	timeout := &errNetworkTimeout{what: what, after: limit}
	var timer *time.Timer
	if limit > 0 {
		timer = time.AfterFunc(limit, func() { timedOut.Store(true); cancel() })
	}
	sent := req.WithContext(sendCtx)
	resp, err := p.base.RoundTrip(sent)
	if err != nil && sendCtx.Err() == nil {
		resp, err = reconnectLocalProxy(sent, p.base, err)
	}
	if err != nil {
		if timer != nil {
			timer.Stop()
		}
		cancel()
		if timedOut.Load() {
			err = timeout
		}
		network.observe(key, 0, 0, time.Since(start))
		release()
		if req.Context().Err() == nil {
			slog.Warn("Сеть: ошибка соединения", "class", class, "host", req.URL.Host,
				"elapsed_ms", time.Since(start).Milliseconds(), "err", RedactSecrets(err.Error()))
		}
		return nil, err
	}
	status := resp.StatusCode
	elapsed := time.Since(start)
	switch {
	case status == 429 || (status == 503 && resp.Header.Get("Retry-After") != ""):
		wait, source, _ := limitHint(resp.Header)
		Throttle.penalize(req.URL.String(), wait, source)
	case status >= 200 && status < 400:
		Throttle.reward(req.URL.String(), start)
		if wait, source, exhausted := limitHint(resp.Header); exhausted {
			// The quota is spent: pause before the site starts answering 429.
			Throttle.cooldown(req.URL.String(), wait)
			slog.Info("Сеть: квота сайта исчерпана — пауза до сброса", "zone", zoneOf(req.URL.Host),
				"source", source, "pause_s", roundSeconds(min(wait, MaxCooldown)))
		}
	}
	if timer != nil && p.idle > 0 {
		timeout.what, timeout.after = "нет данных", p.idle
		timer.Reset(p.idle)
	}
	// Admission lasts until the caller closes the body, including streaming media.
	resp.Body = &networkBody{ReadCloser: resp.Body, timer: timer, idle: p.idle, cancel: cancel, timedOut: &timedOut, timeout: timeout,
		finish: func(bytes uint64) {
			total := time.Since(start)
			network.observe(key, status, bytes, total)
			release()
			slog.Debug("Сеть: ответ", "class", class, "host", req.URL.Host, "status", status,
				"bytes", bytes, "headers_ms", elapsed.Milliseconds(), "total_ms", total.Milliseconds())
		}}
	return resp, nil
}

// doNetwork sends req through the policy transport. client.Timeout becomes
// the per-attempt deadline counted from admission, not from queueing.
func (c *ApiClient) doNetwork(req *http.Request, client *http.Client, api bool) (*http.Response, error) {
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &policyTransport{base: base, api: api, timeout: client.Timeout}
	copy.Timeout = 0
	return copy.Do(req)
}

// doStream is doNetwork for long transfers: no total deadline, only a header
// timeout and an idle watchdog, so a large file is never cut mid-transfer.
func (c *ApiClient) doStream(req *http.Request) (*http.Response, error) {
	copy := *c.HTTP
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &policyTransport{base: base, header: streamHeaderTimeout, idle: streamIdleTimeout}
	copy.Timeout = 0
	return copy.Do(req)
}

// DoMedia streams media (viewer proxy, video cache) under the stream policy.
func (c *ApiClient) DoMedia(req *http.Request) (*http.Response, error) {
	return c.doStream(req)
}

func retryPause(ctx context.Context, d time.Duration) error {
	return waitContext(ctx, d+time.Duration(rand.Int64N(max(1, int64(d/4)))))
}

// isNetworkTimeout reports a deadline or idle-watchdog expiry.
func isNetworkTimeout(err error) bool {
	var t *errNetworkTimeout
	return errors.As(err, &t)
}
