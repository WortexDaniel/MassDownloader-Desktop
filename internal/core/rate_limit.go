package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// throttle is shared by every client instance. Its gates are keyed by the
// rate-limit zone (registrable domain): api.rule34.xxx and rule34.xxx, or
// kemono.cr and n2.kemono.cr, sit behind the same per-IP limit, so a 429 from
// one pauses all of them. Only requests to the same zone are spaced.
type throttle struct {
	mu        sync.Mutex
	hostLocks map[string]*hostGate
	overrides map[string]float64
}

type hostGate struct {
	mu            sync.Mutex
	lastReq       time.Time
	lastMedia     time.Time
	cooldownUntil time.Time
	// strikes counts consecutive 429 answers; it escalates the fallback pause
	// (5s, 10s, 20s … 5m20s) when the site gives no Retry-After.
	strikes int
	// penalty multiplies the polite interval after a 429 and decays slowly
	// with successful answers, so the pace stays below the site's real limit.
	penalty    float64
	okRun      int
	lastStrike time.Time
	interval   func() float64
}

var Throttle = &throttle{
	hostLocks: map[string]*hostGate{},
	overrides: map[string]float64{},
}

const (
	// DefaultInterval is seconds between hits to one host => <2 rps (e621).
	DefaultInterval = 0.55
	// MaxCooldown caps any server hint; a hostile Retry-After must not freeze
	// a host until the app restarts.
	MaxCooldown = 15 * time.Minute
	// LongCooldown is the pause after which a request fails immediately with
	// an actionable time instead of blocking a worker or the UI.
	LongCooldown = 60 * time.Second
	maxPenalty   = 8.0
	// successes needed per penalty decay step (×0.8).
	penaltyDecayRun = 10
)

// sharedHostingSuffixes are platforms whose subdomains belong to unrelated
// tenants; their limits are per host, not per registrable domain.
var sharedHostingSuffixes = map[string]bool{
	"cloudfront.net": true, "amazonaws.com": true, "googleusercontent.com": true,
	"b-cdn.net": true, "r2.dev": true, "workers.dev": true, "pages.dev": true,
	"github.io": true, "fastly.net": true, "akamaized.net": true,
	"azureedge.net": true, "cloudflare.net": true, "appspot.com": true,
	"herokuapp.com": true, "netlify.app": true, "vercel.app": true,
}

// zoneOf maps a host (optionally with port) to its rate-limit zone. IP
// literals and localhost keep their port so local test servers stay apart.
func zoneOf(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.TrimSuffix(strings.Trim(name, "[]"), ".")
	if name == "" || name == "localhost" || net.ParseIP(name) != nil {
		return host
	}
	labels := strings.Split(name, ".")
	if len(labels) <= 2 {
		return name
	}
	two := strings.Join(labels[len(labels)-2:], ".")
	if sharedHostingSuffixes[two] {
		return name
	}
	// Two-level public suffixes (co.uk, com.br, ne.jp …).
	second := labels[len(labels)-2]
	if len(labels[len(labels)-1]) == 2 {
		switch second {
		case "co", "com", "net", "org", "ac", "gov", "edu", "ne", "or", "go":
			return strings.Join(labels[len(labels)-3:], ".")
		}
	}
	return two
}

// RateLimitZone exposes zoneOf for logs and diagnostics.
func RateLimitZone(rawURL string) string { return zoneOf(uHost(rawURL)) }

// SetRateLimits installs per-site throttle overrides (seconds between hits to
// that site's host), keyed by site key, re-indexed to per-host. A missing or
// invalid entry falls back to the site's built-in pace.
func SetRateLimits(bySiteKey map[string]float64) {
	ov := map[string]float64{}
	for key, secs := range bySiteKey {
		site := GetSite(key)
		if site == nil || secs <= 0 || math.IsInf(secs, 0) || math.IsNaN(secs) {
			slog.Debug("Лимиты: пропущено значение", "site", key, "seconds", secs)
			continue
		}
		for _, host := range siteAPIHosts(site) {
			ov[host] = secs
		}
	}
	Throttle.mu.Lock()
	Throttle.overrides = ov
	Throttle.mu.Unlock()
	slog.Info("Лимиты запросов применены", "sites", len(bySiteKey), "hosts", len(ov))
}

// siteAPIHosts lists the hosts that receive a site's API pace.
func siteAPIHosts(s *Site) []string {
	hosts := []string{s.Host()}
	if api := uHost(s.APIBase()); api != "" && api != s.Host() {
		hosts = append(hosts, api)
	}
	return hosts
}

// intervalFor returns the polite spacing for host: user override, else the
// owning Site's MinInterval, else DefaultInterval.
func intervalFor(host string) float64 {
	Throttle.mu.Lock()
	ov := Throttle.overrides[host]
	Throttle.mu.Unlock()
	if ov > 0 {
		return ov
	}
	for _, s := range Sites() {
		if s.MinInterval <= 0 {
			continue
		}
		for _, h := range siteAPIHosts(s) {
			if h == host {
				return s.MinInterval
			}
		}
	}
	return DefaultInterval
}

// mediaIntervalFor is the spacing between media requests for sites whose
// files share the API's per-IP limit (Site.MediaInterval); 0 = unpaced.
func mediaIntervalFor(host string) float64 {
	zone := zoneOf(host)
	for _, s := range Sites() {
		if s.MediaInterval <= 0 {
			continue
		}
		if zoneOf(s.Host()) == zone || (s.StaticDomain != "" && zoneOf(s.StaticDomain) == zone) {
			return s.MediaInterval
		}
	}
	return 0
}

func (t *throttle) gate(host string) *hostGate {
	key := zoneOf(host)
	t.mu.Lock()
	defer t.mu.Unlock()
	g := t.hostLocks[key]
	if g == nil {
		g = &hostGate{penalty: 1}
		t.hostLocks[key] = g
	}
	return g
}

func (t *throttle) wait(urlStr string) {
	_ = t.waitContext(context.Background(), urlStr)
}

// cooldown extends the zone pause without counting a strike.
func (t *throttle) cooldown(urlStr string, delay time.Duration) {
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return
	}
	delay = min(delay, MaxCooldown)
	g := t.gate(u.Host)
	g.mu.Lock()
	defer g.mu.Unlock()
	until := time.Now().Add(delay)
	if until.After(g.cooldownUntil) {
		g.cooldownUntil = until
	}
}

// penalize records a 429 for the URL's zone: one more strike, a doubled
// interval multiplier and a pause. hinted is the server's own wait
// (Retry-After, x-rate-limit-reset …); without it the pause escalates per
// strike. Returns the pause actually applied.
func (t *throttle) penalize(urlStr string, hinted time.Duration, source string) time.Duration {
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return 0
	}
	g := t.gate(u.Host)
	g.mu.Lock()
	g.strikes++
	g.okRun = 0
	g.lastStrike = time.Now()
	g.penalty = min(maxPenalty, max(1, g.penalty)*2)
	delay := hinted
	if delay <= 0 {
		delay = time.Duration(5<<min(g.strikes-1, 6)) * time.Second
		delay += time.Duration(rand.Int64N(int64(delay/5) + 1))
		source = "backoff"
	}
	delay = min(delay, MaxCooldown)
	until := time.Now().Add(delay)
	if until.After(g.cooldownUntil) {
		g.cooldownUntil = until
	}
	strikes, penalty, applied := g.strikes, g.penalty, time.Until(g.cooldownUntil)
	g.mu.Unlock()
	slog.Warn("Сеть: лимит сайта — пауза зоны", "zone", zoneOf(u.Host), "host", u.Host,
		"source", source, "pause_s", roundSeconds(applied), "strikes", strikes,
		"interval_x", penalty, "until", time.Now().Add(applied).Format("15:04:05"))
	return applied
}

// reward records a successful answer to a request started at started. Only
// requests sent after the last 429 clear strikes; the interval multiplier
// decays by 20% every penaltyDecayRun successes.
func (t *throttle) reward(urlStr string, started time.Time) {
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return
	}
	g := t.gate(u.Host)
	g.mu.Lock()
	defer g.mu.Unlock()
	if started.Before(g.lastStrike) {
		return
	}
	g.strikes = 0
	if g.penalty <= 1 {
		return
	}
	g.okRun++
	if g.okRun >= penaltyDecayRun {
		g.okRun = 0
		g.penalty = max(1, g.penalty*0.8)
		if g.penalty < 1.05 {
			g.penalty = 1
		}
		slog.Debug("Сеть: темп зоны восстанавливается", "zone", zoneOf(u.Host), "interval_x", math.Round(g.penalty*100)/100)
	}
}

// CooldownRemaining is the time left in the URL zone's pause (0 if none).
func (t *throttle) CooldownRemaining(urlStr string) time.Duration {
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return 0
	}
	g := t.gate(u.Host)
	g.mu.Lock()
	defer g.mu.Unlock()
	return max(0, time.Until(g.cooldownUntil))
}

// ZoneLimit is one zone's adaptive state for diagnostics.
type ZoneLimit struct {
	Strikes       int     `json:"strikes"`
	IntervalX     float64 `json:"interval_x"`
	CooldownUntil string  `json:"cooldown_until,omitempty"`
}

// NetworkLimits reports zones that are paused or still slowed down.
func NetworkLimits() map[string]ZoneLimit {
	Throttle.mu.Lock()
	gates := make(map[string]*hostGate, len(Throttle.hostLocks))
	for k, g := range Throttle.hostLocks {
		gates[k] = g
	}
	Throttle.mu.Unlock()
	out := map[string]ZoneLimit{}
	for zone, g := range gates {
		g.mu.Lock()
		if g.penalty > 1 || g.strikes > 0 || time.Now().Before(g.cooldownUntil) {
			z := ZoneLimit{Strikes: g.strikes, IntervalX: math.Round(max(1, g.penalty)*100) / 100}
			if time.Now().Before(g.cooldownUntil) {
				z.CooldownUntil = g.cooldownUntil.Format(time.RFC3339)
			}
			out[zone] = z
		}
		g.mu.Unlock()
	}
	return out
}

func rateLimitDelay(header string, attempt int) time.Duration {
	if d, ok := retryAfter(header); ok {
		return d
	}
	return time.Duration(5*(1<<min(attempt, 6))) * time.Second
}

// retryAfter parses Retry-After as delta-seconds or an HTTP-date.
func retryAfter(header string) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(header, 64); err == nil && secs > 0 && !math.IsInf(secs, 0) && !math.IsNaN(secs) {
		if secs >= float64(math.MaxInt64)/float64(time.Second) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if date, err := http.ParseTime(header); err == nil && time.Until(date) > 0 {
		return time.Until(date), true
	}
	return 0, false
}

// limitHint reads the server's own wait: Retry-After, X/Twitter
// x-rate-limit-*, IETF RateLimit-* and the common X-RateLimit-* family.
// exhausted reports a quota at zero on an otherwise successful answer.
func limitHint(h http.Header) (wait time.Duration, source string, exhausted bool) {
	if h == nil {
		return 0, "", false
	}
	if d, ok := retryAfter(h.Get("Retry-After")); ok {
		return d, "Retry-After", false
	}
	for _, pair := range [][3]string{
		{"x-rate-limit-remaining", "x-rate-limit-reset", "epoch"},
		{"X-RateLimit-Remaining", "X-RateLimit-Reset", "auto"},
		{"RateLimit-Remaining", "RateLimit-Reset", "delta"},
	} {
		remaining := strings.TrimSpace(h.Get(pair[0]))
		if remaining == "" {
			continue
		}
		left, err := strconv.ParseFloat(remaining, 64)
		if err != nil || left > 0 {
			continue
		}
		reset, err := strconv.ParseFloat(strings.TrimSpace(h.Get(pair[1])), 64)
		if err != nil || reset <= 0 {
			continue
		}
		var d time.Duration
		switch {
		case pair[2] == "delta", pair[2] == "auto" && reset < 1e9:
			d = time.Duration(reset * float64(time.Second))
		default:
			d = time.Until(time.Unix(int64(reset), 0))
		}
		if d > 0 {
			return d, pair[1], true
		}
	}
	return 0, "", false
}

func roundSeconds(d time.Duration) float64 { return math.Round(d.Seconds()*10) / 10 }

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return DownloadCancelled
	case <-timer.C:
		return nil
	}
}

// waitContext spaces API requests to the zone by the host's interval times
// the zone's penalty and honours its pause.
func (t *throttle) waitContext(ctx context.Context, urlStr string) error {
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return nil
	}
	g := t.gate(u.Host)
	start := time.Now()
	for {
		if ctx.Err() != nil {
			return DownloadCancelled
		}
		g.mu.Lock()
		spacing := time.Duration(intervalFor(u.Host) * max(1, g.penalty) * float64(time.Second))
		next := g.lastReq.Add(spacing)
		if g.cooldownUntil.After(next) {
			next = g.cooldownUntil
		}
		delay := time.Until(next)
		if delay <= 0 {
			g.lastReq = time.Now()
			g.mu.Unlock()
			if waited := time.Since(start); waited >= time.Second {
				slog.Debug("Сеть: ожидание интервала API", "host", u.Host, "waited_s", roundSeconds(waited))
			}
			return nil
		}
		g.mu.Unlock()
		if err := waitContext(ctx, delay); err != nil {
			return err
		}
	}
}

// waitMedia honours the zone pause and, for sites with MediaInterval, spaces
// media requests (scaled by the zone penalty).
func (t *throttle) waitMedia(ctx context.Context, raw string) error {
	host := uHost(raw)
	if host == "" {
		return nil
	}
	g := t.gate(host)
	interval := mediaIntervalFor(host)
	start := time.Now()
	for {
		if ctx.Err() != nil {
			return DownloadCancelled
		}
		g.mu.Lock()
		next := g.cooldownUntil
		if interval > 0 {
			if paced := g.lastMedia.Add(time.Duration(interval * max(1, g.penalty) * float64(time.Second))); paced.After(next) {
				next = paced
			}
		}
		delay := time.Until(next)
		if delay <= 0 {
			if interval > 0 {
				g.lastMedia = time.Now()
			}
			g.mu.Unlock()
			if waited := time.Since(start); waited >= time.Second {
				slog.Debug("Сеть: ожидание паузы медиа", "host", host, "waited_s", roundSeconds(waited))
			}
			return nil
		}
		g.mu.Unlock()
		if err := waitContext(ctx, delay); err != nil {
			return err
		}
	}
}

// waitCooldown is kept for callers that need only the zone pause.
func (t *throttle) waitCooldown(ctx context.Context, raw string) error {
	g := t.gate(uHost(raw))
	for {
		if ctx.Err() != nil {
			return DownloadCancelled
		}
		g.mu.Lock()
		delay := time.Until(g.cooldownUntil)
		g.mu.Unlock()
		if delay <= 0 {
			return nil
		}
		if err := waitContext(ctx, delay); err != nil {
			return err
		}
	}
}

// rateLimitedError is the 429 surfaced to callers; the text keeps "HTTP 429"
// (the frontend matches it) and names when the zone opens again.
func rateLimitedError(urlStr string) *ApiError {
	left := Throttle.CooldownRemaining(urlStr)
	zone := RateLimitZone(urlStr)
	if left <= 0 {
		return apiErrStatus(429, "HTTP 429 — %s временно ограничил запросы; повторите позднее", zone)
	}
	return apiErrStatus(429, "HTTP 429 — %s ограничил запросы до %s (≈%s); повторите после паузы",
		zone, time.Now().Add(left).Format("15:04:05"), humanWait(left))
}

func humanWait(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d с", int(math.Ceil(d.Seconds())))
	}
	return fmt.Sprintf("%d мин", int(math.Ceil(d.Minutes())))
}

// IsRateLimited reports an HTTP 429 anywhere in err's chain.
func IsRateLimited(err error) bool {
	var apiErr *ApiError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests
}
