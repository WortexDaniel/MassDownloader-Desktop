// Shared, rate-limited HTTP client used by every engine.
// Port of app/core/client.py.
//
// Engines are stateless and receive this client on each call; it owns the
// HTTP session, the per-host throttle (e621 asks for <= 2 req/s), per-site
// credentials, and the raw byte transfers used for downloads/thumbnails.
package core

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Version is the app version reported in the User-Agent.
const Version = "3.0.0-go"

// ApiError is a failed API call; Status is the HTTP status when the error is
// a bad response (mirrors client.ApiError).
type ApiError struct {
	Msg    string
	Status int
}

func (e *ApiError) Error() string { return RedactSecrets(e.Msg) }

func apiErr(format string, a ...any) *ApiError {
	return &ApiError{Msg: RedactSecrets(fmt.Sprintf(format, a...))}
}

func apiErrStatus(status int, format string, a ...any) *ApiError {
	return &ApiError{Msg: RedactSecrets(fmt.Sprintf(format, a...)), Status: status}
}

// AuthRequiredError: a site needs API credentials that are missing/invalid.
type AuthRequiredError struct {
	SiteKey string
}

func (e *AuthRequiredError) Error() string {
	return e.SiteKey + ": требуется авторизация (api_key + user_id)"
}

// DownloadCancelled is raised (returned) from a progress callback to abort
// an in-flight transfer.
var DownloadCancelled = errors.New("download cancelled")

// BrowserUA is a real browser UA replayed to image CDNs that reject the
// default bot UA (Sankaku's CDN 403s anything that doesn't look like one).
const BrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// DefaultIntervalFor mirrors ApiClient.default_interval(site).
func DefaultIntervalFor(site *Site) float64 {
	if site.MinInterval > 0 {
		return site.MinInterval
	}
	return DefaultInterval
}

// StopCheck is an optional "should I stop?" hook checked before every API
// round-trip so a slow multi-page scan can be aborted promptly.
type StopCheck func() bool

// CFOverride carries the replayed browser identity for CF-protected hosts.
type CFOverride struct {
	UserAgent   string
	CfClearance string
}

// ApiClient is the shared HTTP session + throttle + credentials owner.
type ApiClient struct {
	HTTP          *http.Client
	UserAgent     string
	Timeout       time.Duration
	MaxRetries    int
	Metadata      *MetadataCache
	FreshMetadata bool

	mu               sync.Mutex
	credentials      map[string]map[string]string
	cfByHost         map[string]CFOverride
	stopCheck        StopCheck
	operationContext context.Context
	danbooruGold     map[string]bool
}

// NewClient builds a client with Python's defaults (30s timeout, 3 retries).
func NewClient() *ApiClient {
	defaultTransportOnce.Do(func() {
		defaultTransport = &http.Transport{
			Proxy:               defaultProxy,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		}
	})
	return &ApiClient{
		HTTP:        &http.Client{Timeout: 30 * time.Second, Transport: defaultTransport},
		UserAgent:   fmt.Sprintf("MassDownloader/%s (by nomyaw)", Version),
		Timeout:     30 * time.Second,
		MaxRetries:  3,
		credentials: map[string]map[string]string{},
		cfByHost:    map[string]CFOverride{},
	}
}

// SetCredentials replaces per-site credentials and rebuilds the CF index:
// cf_clearance/User-Agent are keyed by the owning site's host and its
// explicitly trusted extra hosts. Extra hosts may override either value with
// cf_clearance@host / user_agent@host; missing values inherit the site's pair.
func (c *ApiClient) SetCredentials(creds map[string]map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credentials = map[string]map[string]string{}
	for k, v := range creds {
		c.credentials[k] = cloneStrMap(v)
	}
	c.cfByHost = map[string]CFOverride{}
	for key, cred := range c.credentials {
		ua := strings.TrimSpace(cred["user_agent"])
		cf := strings.TrimSpace(cred["cf_clearance"])
		site := GetSite(key)
		if site == nil { // stale credential for a removed site
			continue
		}
		hosts := append([]string{site.Host()}, site.CfExtraDomains...)
		for _, h := range hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" {
				hostUA, hostCF := ua, cf
				if v := strings.TrimSpace(cred["user_agent@"+h]); v != "" {
					hostUA = v
				}
				if v := strings.TrimSpace(cred["cf_clearance@"+h]); v != "" {
					hostCF = v
				}
				if hostUA != "" || hostCF != "" {
					c.cfByHost[h] = CFOverride{UserAgent: hostUA, CfClearance: hostCF}
				}
			}
		}
	}
}

// Credentials returns the current per-site credentials map.
func (c *ApiClient) Credentials() map[string]map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]map[string]string{}
	for k, v := range c.credentials {
		out[k] = cloneStrMap(v)
	}
	return out
}

// AuthIdentity is a non-reversible partition key; credentials never enter logs
// or cache paths. A cookie/UA change forces fresh authenticated metadata.
func (c *ApiClient) AuthIdentity() string {
	data, _ := json.Marshal(c.Credentials())
	sum := sha256.Sum256(append(data, []byte(c.UserAgent)...))
	return hex.EncodeToString(sum[:])
}

// Clone keeps the shared HTTP pool but never copies a mutex or mutable operation
// context. Engines may attach their fallback cache without changing the caller.
func (c *ApiClient) Clone(ctx context.Context) *ApiClient {
	other := NewClient()
	httpClient := *c.HTTP
	other.HTTP = &httpClient
	other.UserAgent = c.UserAgent
	other.Timeout = c.Timeout
	other.MaxRetries = c.MaxRetries
	other.Metadata = c.Metadata
	other.FreshMetadata = c.FreshMetadata
	other.SetCredentials(c.Credentials())
	other.SetContext(ctx)
	c.mu.Lock()
	other.danbooruGold = map[string]bool{}
	for k, v := range c.danbooruGold {
		other.danbooruGold[k] = v
	}
	other.stopCheck = c.stopCheck
	c.mu.Unlock()
	return other
}

// Cred fetches one field of one site's credentials.
func (c *ApiClient) Cred(siteKey, field string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.credentials[siteKey][field])
}

func cloneStrMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (c *ApiClient) cfOverride(urlStr string) (CFOverride, bool) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return CFOverride{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ov, ok := c.cfByHost[strings.ToLower(u.Host)]
	return ov, ok
}

// applyCFHeaders updates an existing session cookie without duplicating or
// retaining a clearance from the primary host on a secondary media host.
func (c *ApiClient) applyCFHeaders(urlStr string, headers map[string]string) map[string]string {
	cf, ok := c.cfOverride(urlStr)
	if !ok {
		return headers
	}
	if headers == nil {
		headers = map[string]string{}
	}
	if cf.UserAgent != "" {
		headers["User-Agent"] = cf.UserAgent
	}
	if cf.CfClearance != "" {
		cookies := make([]string, 0)
		for _, part := range strings.Split(headers["Cookie"], ";") {
			part = strings.TrimSpace(part)
			if part != "" && !strings.HasPrefix(strings.ToLower(part), "cf_clearance=") {
				cookies = append(cookies, part)
			}
		}
		cookies = append(cookies, "cf_clearance="+cf.CfClearance)
		headers["Cookie"] = strings.Join(cookies, "; ")
	}
	return headers
}

// SetStopCheck installs (or clears with nil) the abort hook.
func (c *ApiClient) SetStopCheck(fn StopCheck) { c.mu.Lock(); c.stopCheck = fn; c.mu.Unlock() }
func (c *ApiClient) SetContext(ctx context.Context) {
	c.mu.Lock()
	c.operationContext = ctx
	c.mu.Unlock()
}
func (c *ApiClient) Context() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.operationContext != nil {
		return c.operationContext
	}
	return context.Background()
}

func (c *ApiClient) checkStop() error {
	c.mu.Lock()
	stop := c.stopCheck
	c.mu.Unlock()
	if stop != nil && stop() {
		return DownloadCancelled
	}
	return nil
}

// AuthParams mirrors client.auth_params: api_key+user_id for sites that
// require auth and have both stored.
func (c *ApiClient) AuthParams(site *Site) map[string]string {
	if !site.RequiresAuth {
		return nil
	}
	cred := c.Credentials()[site.Key]
	if cred["api_key"] != "" && cred["user_id"] != "" {
		return map[string]string{"api_key": cred["api_key"], "user_id": cred["user_id"]}
	}
	return nil
}

// Request is one throttled, retried HTTP round-trip. 429/5xx retry; 403
// fails fast (auth/Cloudflare). body may be nil; contentType selects
// form/json/multipart encodings the caller prepared. A nil ctx defaults to
// context.Background() (engines pass nil for fire-and-forget calls).
func (c *ApiClient) Request(ctx context.Context, method, urlStr string,
	params url.Values, body io.Reader, contentType string,
	headers map[string]string) (*http.Response, []byte, error) {
	// Freeze auth before deriving a cache key or launching shared work.
	if ctx == nil {
		ctx = c.Context()
	}
	c = c.Clone(ctx)
	if err := c.checkStop(); err != nil {
		return nil, nil, err
	}
	if ctx == nil {
		ctx = c.Context()
	}
	fullURL := urlStr
	if len(params) > 0 {
		sep := "?"
		if strings.Contains(fullURL, "?") {
			sep = "&"
		}
		fullURL += sep + params.Encode()
	}
	ttl := metadataTTL(method, fullURL)
	if c.Metadata == nil || c.FreshMetadata || ttl == 0 {
		return c.requestUncached(ctx, method, urlStr, params, body, contentType, headers)
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = io.ReadAll(body)
		if err != nil {
			return nil, nil, err
		}
	}
	keyHeaders := map[string]string{}
	for k, v := range headers {
		keyHeaders[k] = v
	}
	keyHeaders["Content-Type"] = contentType
	key := c.metadataKey(method, fullURL, payload, keyHeaders)
	fetched := false
	encoded, err := c.Metadata.Fetch(ctx, key, ttl, func(workCtx context.Context) ([]byte, error) {
		fetched = true
		shared := c.Clone(workCtx)
		shared.SetStopCheck(nil) // shared cancellation belongs to all consumers
		resp, data, err := shared.requestUncached(workCtx, method, urlStr, params, bytes.NewReader(payload), contentType, headers)
		if err != nil {
			encoded, _ := json.Marshal(cachedHTTPResponse{Data: data})
			return encoded, err
		}
		value := cachedHTTPResponse{Data: data, Header: resp.Header.Clone(), Status: resp.StatusCode}
		encoded, _ := json.Marshal(value)
		if !validMetadataResponse(fullURL, data) {
			return encoded, errMetadataUncacheable
		}
		return encoded, nil
	})
	if !fetched && err == nil {
		slog.Debug("Сеть: ответ из кэша метаданных", "method", method, "url", redactURL(fullURL))
	}
	if err != nil && !errors.Is(err, errMetadataUncacheable) {
		var value cachedHTTPResponse
		_ = json.Unmarshal(encoded, &value)
		return nil, value.Data, err
	}
	var value cachedHTTPResponse
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, nil, err
	}
	return &http.Response{StatusCode: value.Status, Header: value.Header, Body: io.NopCloser(bytes.NewReader(value.Data))}, value.Data, nil
}
func (c *ApiClient) requestUncached(ctx context.Context, method, urlStr string,
	params url.Values, body io.Reader, contentType string, headers map[string]string) (*http.Response, []byte, error) {

	if ctx == nil {
		ctx = c.Context()
	}

	if params != nil && len(params) > 0 {
		sep := "?"
		if strings.Contains(urlStr, "?") {
			sep = "&"
		}
		urlStr = urlStr + sep + params.Encode()
	}

	// replay a pasted cf_clearance cookie + matching UA for this host
	var reqHeaders = map[string]string{}
	for k, v := range headers {
		reqHeaders[k] = v
	}
	reqHeaders = c.applyCFHeaders(urlStr, reqHeaders)

	var lastErr error
	var bodyData []byte
	if body != nil {
		var err error
		bodyData, err = io.ReadAll(body)
		if err != nil {
			return nil, nil, err
		}
	}
	for attempt := 0; attempt < max(1, c.MaxRetries); attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, DownloadCancelled
		}
		if err := c.checkStop(); err != nil {
			return nil, nil, err
		}
		if left := Throttle.CooldownRemaining(urlStr); left > LongCooldown {
			// Waiting minutes would freeze the feed or a worker; report when
			// the zone opens instead.
			slog.Info("Сеть: запрос отклонён до конца паузы зоны", "zone", RateLimitZone(urlStr),
				"pause_s", roundSeconds(left), "url", redactURL(urlStr))
			return nil, nil, rateLimitedError(urlStr)
		}
		var requestBody io.Reader
		if bodyData != nil {
			requestBody = bytes.NewReader(bodyData)
		}
		req, err := http.NewRequestWithContext(ctx, method, urlStr, requestBody)
		if err != nil {
			return nil, nil, apiErr("bad request: %v", err)
		}
		req.Header.Set("User-Agent", c.UserAgent)
		for k, v := range reqHeaders {
			req.Header.Set(k, v)
		}
		if contentType != "" && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", contentType)
		}
		httpClient := *c.HTTP
		httpClient.Timeout = c.Timeout
		resp, err := c.doNetwork(req, &httpClient, true)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, DownloadCancelled
			}
			lastErr = err
			slog.Warn("retry", "err", RedactSecrets(err.Error()), "url", redactURL(urlStr),
				"attempt", attempt+1)
			if attempt+1 < max(1, c.MaxRetries) {
				if err := retryPause(ctx, time.Duration(800*(attempt+1))*time.Millisecond); err != nil {
					return nil, nil, err
				}
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (256<<20)+1))
		resp.Body.Close()
		if len(data) > 256<<20 {
			return nil, nil, apiErr("response exceeds 256 MB")
		}
		if readErr != nil {
			lastErr = readErr
			slog.Warn("retry", "err", RedactSecrets(readErr.Error()), "url", redactURL(urlStr),
				"attempt", attempt+1)
			if attempt+1 < max(1, c.MaxRetries) {
				if err := retryPause(ctx, time.Duration(800*(attempt+1))*time.Millisecond); err != nil {
					return nil, nil, err
				}
			}
			continue
		}
		if resp.StatusCode == 429 {
			// The transport already penalised the zone (strike, pace, pause).
			lastErr = rateLimitedError(urlStr)
			left := Throttle.CooldownRemaining(urlStr)
			if attempt+1 >= max(1, c.MaxRetries) || left > LongCooldown {
				slog.Warn("HTTP 429 — запрос не выполнен", "host", uHost(urlStr), "attempt", attempt+1,
					"pause_s", roundSeconds(left), "url", redactURL(urlStr))
				return nil, data, lastErr
			}
			slog.Info("HTTP 429 — повтор после паузы", "host", uHost(urlStr), "attempt", attempt+1, "pause_s", roundSeconds(left))
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = apiErrStatus(resp.StatusCode, "HTTP %d", resp.StatusCode)
			slog.Warn("HTTP — повтор", "status", resp.StatusCode,
				"url", redactURL(urlStr), "attempt", attempt+1)
			if attempt+1 < max(1, c.MaxRetries) {
				if err := retryPause(ctx, time.Duration(1200*(attempt+1))*time.Millisecond); err != nil {
					return nil, nil, err
				}
			}
			continue
		}
		if resp.StatusCode == 403 {
			if host := uHost(urlStr); host == "rule34.xxx" || host == "api.rule34.xxx" {
				lower := strings.ToLower(string(data))
				if strings.Contains(lower, "<title>rule34.xxx captcha</title>") {
					return nil, data, apiErrStatus(403, "403 (%s) — Rule34 требует CAPTCHA; доступ к API заблокирован защитой сайта", host)
				}
				if strings.Contains(lower, "you have been blocked") {
					return nil, data, apiErrStatus(403, "403 (%s) — Rule34 заблокировал доступ с текущего подключения", host)
				}
			}
			if strings.Contains(strings.ToLower(string(data)), "you cannot access the site from russia") {
				return nil, data, apiErrStatus(403,
					"403 (%s) — сайт блокирует доступ из России (RKN/DDoS-Guard). Используйте доступное соединение через VPN или прокси.", uHost(urlStr))
			}
			if strings.EqualFold(resp.Header.Get("CF-Mitigated"), "challenge") {
				return nil, data, cfChallengeError(urlStr)
			}
			// причина не обязательно Cloudflare (приватные посты, гео-блок,
			// битые куки) — называем хост и вероятное лечение
			return nil, data, apiErrStatus(403,
				"403 (%s) — доступ запрещён. Чаще всего это Cloudflare: "+
					"откройте окно авторизации сайта и вставьте cf_clearance "+
					"+ User-Agent из браузера. Проверьте также куки/доступ аккаунта.",
				uHost(urlStr))
		}
		if resp.StatusCode == 401 {
			return nil, data, apiErrStatus(401,
				"401 — сайт отклонил авторизацию. Проверьте api_key и user_id "+
					"(скопируйте их целиком из настроек аккаунта — ключ обычно "+
					"длинный). Часть сайтов (например gelbooru.com) также "+
					"закрывает API для VPN и некоторых стран.")
		}
		if resp.StatusCode != 200 {
			return nil, data, apiErrStatus(resp.StatusCode, "HTTP %d",
				resp.StatusCode)
		}
		resp.Body = io.NopCloser(bytes.NewReader(data))
		return resp, data, nil
	}
	if lastErr == nil {
		lastErr = apiErr("Запрос не удался")
	}
	slog.Error("Запрос не удался", "url", redactURL(urlStr), "err", RedactSecrets(lastErr.Error()))
	var statusErr *ApiError
	if errors.As(lastErr, &statusErr) && statusErr.Status != 0 {
		return nil, nil, apiErrStatus(statusErr.Status, "Запрос не удался: %v", lastErr)
	}
	return nil, nil, apiErr("Запрос не удался: %v", lastErr)
}

func uHost(urlStr string) string {
	if u, err := url.Parse(urlStr); err == nil {
		return u.Host
	}
	return ""
}

func cfChallengeError(urlStr string) error {
	return apiErrStatus(403, "Cloudflare challenge на %s: проверьте cf_clearance и User-Agent для этого домена в авторизации сайта", uHost(urlStr))
}

// RedactSecrets strips credential-looking query/header values from error
// strings before they reach logs (mirrors app.log.redact_secrets).
var secretPair = regexp.MustCompile(`(?i)(\b(?:api_key|user_id|login|passwd|password|auth_token|token|access_token|refresh_token|ct0|cf_clearance|PHPSESSID)\s*[=:]\s*)([^\s&;,"'<>]+)`)
var secretHeader = regexp.MustCompile(`(?i)(\b(?:cookie|authorization)\s*[:=]\s*)[^\r\n]+`)
var secretURLUser = regexp.MustCompile(`(?i)((?:https?|socks5h?)://)[^/\s"'<>]+@`)

func RedactSecrets(s string) string {
	s = secretURLUser.ReplaceAllString(s, "${1}<скрыто>@")
	s = secretHeader.ReplaceAllString(s, "${1}<скрыто>")
	s = secretPair.ReplaceAllString(s, "${1}<скрыто>")
	if u, err := url.Parse(s); err == nil && u.RawQuery != "" {
		q := u.Query()
		changed := false
		for _, key := range []string{"api_key", "user_id", "login", "passwd", "password", "ct0", "cf_clearance", "PHPSESSID",
			"auth_token", "token", "access_token", "refresh_token"} {
			if q.Get(key) != "" {
				q.Set(key, "<скрыто>")
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return s
}

func redactURL(s string) string { return RedactSecrets(s) }

// GetJSON does a throttled GET returning parsed JSON. A zero-byte 200 body
// (some Gelbooru-compatible APIs) parses to nil, not an error.
func (c *ApiClient) GetJSON(ctx context.Context, urlStr string,
	params url.Values, headers map[string]string) (any, error) {

	resp, data, err := c.Request(ctx, "GET", urlStr, params, nil, "", headers)
	if err != nil {
		return nil, err
	}
	_ = resp
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var out any
	if err := DecodeJSON(trimmed, &out); err != nil {
		return nil, apiErr("Некорректный JSON: %v", err)
	}
	return out, nil
}

// PostJSON does a throttled POST (form/multipart/JSON body) returning parsed
// JSON — Misskey's /api/notes/* endpoints use jsonBody.
func (c *ApiClient) PostJSON(ctx context.Context, urlStr string,
	params url.Values, body io.Reader, contentType string,
	headers map[string]string) (any, error) {

	resp, data, err := c.Request(ctx, "POST", urlStr, params, body,
		contentType, headers)
	if err != nil {
		return nil, err
	}
	_ = resp
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var out any
	if err := DecodeJSON(trimmed, &out); err != nil {
		return nil, apiErr("Некорректный JSON: %v", err)
	}
	return out, nil
}

// GetText fetches raw text (HTML/JS) — X's client bundle scrape.
func (c *ApiClient) GetText(ctx context.Context, urlStr string,
	params url.Values, headers map[string]string) (string, error) {

	_, data, err := c.Request(ctx, "GET", urlStr, params, nil, "", headers)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// PostText is the throttled multipart/form POST returning raw response text —
// reverse-image providers answering HTML (IQDB).
func (c *ApiClient) PostText(ctx context.Context, urlStr string,
	params url.Values, body io.Reader, contentType string,
	headers map[string]string) (string, error) {

	_, data, err := c.Request(ctx, "POST", urlStr, params, body,
		contentType, headers)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// IsReachable is a lightweight availability probe for the site-health
// indicator: any HTTP response below 500 counts (a 403/401 still means the
// site is up, just gated). Bypasses throttle/retry on purpose.
func (c *ApiClient) IsReachable(urlStr string) bool {
	cf, _ := c.cfOverride(urlStr)
	headers := map[string]string{}
	if cf.UserAgent != "" {
		headers["User-Agent"] = cf.UserAgent
	}
	if cf.CfClearance != "" {
		headers["Cookie"] = "cf_clearance=" + cf.CfClearance
	}
	for _, method := range []string{"HEAD", "GET"} {
		ctx, cancel := context.WithTimeout(c.Context(), 6*time.Second)
		req, err := http.NewRequestWithContext(ctx, method, urlStr, nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("User-Agent", c.UserAgent)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.doNetwork(req, c.HTTP, true)
		cancel()
		if err != nil {
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if method == "HEAD" && (resp.StatusCode == 403 ||
			resp.StatusCode == 405 || resp.StatusCode == 501) {
			continue // server dislikes HEAD — confirm with a GET
		}
		return resp.StatusCode < 500
	}
	return false
}

// -- endpoints (delegate to the site's engine) -----------------------------

// Site looks a site up by key.
func (c *ApiClient) Site(key string) *Site { return GetSite(key) }

// SplitImagesOnly pulls the «только изображения» flag out of a query,
// returning the query without the token and whether it was set.
func SplitImagesOnly(tags string) (string, bool) {
	parts := strings.Fields(tags)
	on := false
	keep := parts[:0]
	for _, p := range parts {
		if p == IMAGES_ONLY_TOKEN {
			on = true
		} else {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " "), on
}

// DropAnim removes video/animation posts; if the dropped tail held a keyset
// cursor (Extra["_next"], X/Sankaku), it carries onto the surviving last post
// so pagination isn't lost.
func DropAnim(posts []*PostInfo) []*PostInfo {
	kept := make([]*PostInfo, 0, len(posts))
	for _, p := range posts {
		if p.File == nil || !ANIM_EXTS[p.File.Ext] {
			kept = append(kept, p)
		}
	}
	return preservePostCursor(posts, kept)
}

func preservePostCursor(posts, kept []*PostInfo) []*PostInfo {
	if len(posts) > 0 && len(kept) > 0 && posts[len(posts)-1] != nil && kept[len(kept)-1] != posts[len(posts)-1] {
		last := posts[len(posts)-1]
		if nxt, ok := last.Extra["_next"]; ok && nxt != nil {
			if kept[len(kept)-1].Extra == nil {
				kept[len(kept)-1].Extra = map[string]any{}
			}
			kept[len(kept)-1].Extra["_next"] = nxt
		}
	}
	return kept
}

// SearchPosts runs one page of a site search honouring images-only.
func (c *ApiClient) SearchPosts(ctx context.Context, siteKey, tags string,
	limit int, page any) ([]*PostInfo, error) {
	if ctx != nil {
		c = c.Clone(ctx)
	}

	site := GetSite(siteKey)
	if site == nil {
		return nil, apiErr("unknown site %q", siteKey)
	}
	if limit <= 0 || limit > site.PageLimit {
		limit = site.PageLimit
	}
	tags, imagesOnly := SplitImagesOnly(tags)
	tags, hideText := SplitHideText(tags)
	engine := Registry.EngineFor(site)
	posts, err := engine.FetchPosts(c, site, tags, limit, page)
	if err != nil {
		return nil, err
	}
	if imagesOnly {
		posts = DropAnim(posts)
	}
	if hideText {
		posts = DropTextPosts(posts)
	}
	return posts, nil
}

// IterPosts walks the whole result set, paginating the way the engine wants,
// yielding posts on the channel until exhausted or stopped.
func (c *ApiClient) IterPosts(ctx context.Context, siteKey, tags string,
	maxPosts int, shouldStop StopCheck,
	yield func(*PostInfo) bool) error {
	if ctx != nil {
		c = c.Clone(ctx)
	}

	site := GetSite(siteKey)
	if site == nil {
		return apiErr("unknown site %q", siteKey)
	}
	engine := Registry.EngineFor(site)
	tags, imagesOnly := SplitImagesOnly(tags)
	var cursor any = 1
	seenCursors := map[string]bool{}
	seenPosts := map[string]bool{}
	yielded := 0
	c.mu.Lock()
	prevStop := c.stopCheck
	c.mu.Unlock()
	c.SetStopCheck(shouldStop)
	defer c.SetStopCheck(prevStop)
	for {
		if shouldStop != nil && shouldStop() {
			return nil
		}
		cursorKey := fmt.Sprint(cursor)
		if seenCursors[cursorKey] {
			return apiErr("источник повторил курсор поиска")
		}
		seenCursors[cursorKey] = true
		page, err := FetchEnginePage(c, engine, site, tags, site.PageLimit, cursor)
		batch := page.Posts
		if err != nil {
			if errors.Is(err, DownloadCancelled) {
				return nil
			}
			return err
		}
		if len(batch) == 0 && !page.HasMore {
			return nil
		}
		// advance the cursor off the raw batch (the dropped tail may carry it)
		nextCursor := page.Next
		if imagesOnly {
			batch = DropAnim(batch)
		}
		for _, p := range batch {
			if p == nil || p.ID.String() == "" {
				return apiErr("некорректный пост в ответе поиска")
			}
			if seenPosts[p.Site+"\x00"+p.ID.Key()] {
				continue
			}
			seenPosts[p.Site+"\x00"+p.ID.Key()] = true
			if shouldStop != nil && shouldStop() {
				return nil
			}
			if !yield(p) {
				return nil
			}
			yielded++
			if maxPosts > 0 && yielded >= maxPosts {
				return nil
			}
		}
		if nextCursor == nil || fmt.Sprint(nextCursor) == "" || fmt.Sprint(nextCursor) == fmt.Sprint(cursor) {
			return nil
		}
		cursor = nextCursor
	}
}

// AutocompleteTags fetches tag suggestions for include/exclude fields.
func (c *ApiClient) AutocompleteTags(ctx context.Context, siteKey,
	query string) ([]TagSuggestion, error) {
	if ctx != nil {
		c = c.Clone(ctx)
	}

	site := GetSite(siteKey)
	if site == nil {
		return nil, apiErr("unknown site %q", siteKey)
	}
	return Registry.EngineFor(site).Autocomplete(c, site,
		strings.ToLower(strings.TrimSpace(query)))
}

// SearchArtists fetches artist suggestions for the authors field.
func (c *ApiClient) SearchArtists(ctx context.Context, siteKey, query string,
	limit int) ([]TagSuggestion, error) {
	if ctx != nil {
		c = c.Clone(ctx)
	}

	site := GetSite(siteKey)
	if site == nil {
		return nil, apiErr("unknown site %q", siteKey)
	}
	return Registry.EngineFor(site).SearchArtists(c, site,
		strings.ToLower(strings.TrimSpace(query)), limit)
}

// -- raw bytes --------------------------------------------------------------

func looksLikeImage(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	if bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}) {
		return true
	}
	if bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return true
	}
	if bytes.HasPrefix(data, []byte("GIF87a")) ||
		bytes.HasPrefix(data, []byte("GIF89a")) {
		return true
	}
	if bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return true
	}
	if bytes.Equal(data[4:8], []byte("ftyp")) &&
		(bytes.Equal(data[8:12], []byte("avif")) || bytes.Equal(data[8:12], []byte("avis"))) {
		return true
	}
	if bytes.HasPrefix(data, []byte("BM")) {
		return true
	}
	return false
}

// ImgHeaders builds per-CDN headers for raw image fetches. Port of
// ApiClient._img_headers with every special case.
func (c *ApiClient) ImgHeaders(urlStr string) (headers map[string]string) {
	defer func() { headers = c.applyCFHeaders(urlStr, headers) }()
	if strings.Contains(urlStr, "pximg.net") {
		// Pixiv's CDN hotlink-blocks without a pixiv Referer
		return map[string]string{"Referer": "https://www.pixiv.net/"}
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		return map[string]string{}
	}
	host := strings.ToLower(u.Host)

	// Danbooru's CDN: same-site Referer; trusted-host CF identity is added below.
	if host == "cdn.donmai.us" || strings.HasSuffix(host, ".cdn.donmai.us") {
		return map[string]string{"Referer": "https://danbooru.donmai.us/"}
	}
	// Kemono/Coomer hosts sit behind the same Cloudflare as the API.
	for _, siteHost := range []string{"kemono.cr", "coomer.st", "pawchive.pw", "cum.st"} {
		if host == siteHost || strings.HasSuffix(host, "."+siteHost) {
			return map[string]string{"Referer": "https://" + siteHost + "/"}
		}
	}
	// DeviantArt original downloads are session-gated: pasted Cookie +
	// browser-ish UA. The wixmp CDN itself needs no special headers.
	if host == "www.deviantart.com" || strings.HasSuffix(host, ".deviantart.com") {
		h := map[string]string{
			"User-Agent": BrowserUA,
			"Referer":    "https://www.deviantart.com/",
		}
		if cookie := c.Cred("deviantart", "cookie"); cookie != "" {
			h["Cookie"] = cookie
		}
		return h
	}
	// FurAffinity media hosts sit behind the same Cloudflare as the site.
	if host == "furaffinity.net" || strings.HasSuffix(host, ".furaffinity.net") {
		h := map[string]string{
			"User-Agent": BrowserUA,
			"Referer":    "https://www.furaffinity.net/",
		}
		cookie := c.Cred("furaffinity", "cookie")
		cf := c.Cred("furaffinity", "cf_clearance")
		if cf != "" && !strings.Contains(cookie, "cf_clearance=") {
			if cookie != "" {
				cookie = cookie + "; cf_clearance=" + cf
			} else {
				cookie = "cf_clearance=" + cf
			}
		}
		if ua := c.Cred("furaffinity", "user_agent"); ua != "" {
			h["User-Agent"] = ua
		}
		if cookie != "" {
			h["Cookie"] = cookie
		}
		return h
	}
	// Sankaku shared CDN hosts 403 the bot UA; do NOT send a Referer —
	// s.sankakucomplex.com treats a chan Referer as hotlinking and silently
	// swaps in a 12 KB placeholder PNG (HTTP 200!) for any file.
	if host == "sankakucomplex.com" ||
		strings.HasSuffix(host, ".sankakucomplex.com") {
		return map[string]string{"User-Agent": BrowserUA}
	}
	// Gelbooru-family CDNs hotlink-protect: a refererless GET returns the
	// site's HTML index instead of the image.
	if ref := HotlinkReferer(host); ref != "" {
		return map[string]string{"Referer": ref}
	}
	return map[string]string{}
}

// HotlinkReferer returns the same-site Referer for a Gelbooru-family image
// host, else "". Matched off the registry so sites added later are covered.
func HotlinkReferer(host string) string {
	for _, s := range Sites() {
		if strings.HasPrefix(s.Engine, "gelbooru") {
			if host == s.Domain || strings.HasSuffix(host, "."+s.Domain) {
				return "https://" + s.Domain + "/"
			}
		}
	}
	return ""
}

// FetchBytes fetches raw image bytes, handling Danbooru's lazy
// danbooru-media:// references (primary first, media_assets fallback).
func (c *ApiClient) FetchBytes(ctx context.Context, urlStr string) ([]byte, error) {
	if !strings.HasPrefix(urlStr, "danbooru-media:") {
		return c.fetchImageURL(ctx, urlStr, false)
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, apiErr("bad media ref: %v", err)
	}
	q := u.Query()
	primary := firstOf(q, "primary", "fallback")
	if q.Get("force_assets") == "1" {
		primary = ""
	}
	var lastErr error
	if primary != "" {
		data, err := c.fetchImageURL(ctx, primary, true)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	urls := c.ResolveMediaURLs(urlStr, false)
	if len(urls) == 0 {
		if lastErr != nil {
			return nil, apiErr("Fetch failed: %v", lastErr)
		}
		return nil, apiErr("Danbooru media asset has no usable image URL")
	}
	for _, candidate := range urls {
		data, err := c.fetchImageURL(ctx, candidate, true)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, apiErr("Fetch failed: %v", lastErr)
}

func firstOf(v url.Values, keys ...string) string {
	for _, k := range keys {
		if v.Get(k) != "" {
			return v.Get(k)
		}
	}
	return ""
}

func (c *ApiClient) fetchImageURL(ctx context.Context, urlStr string,
	requireImage bool) ([]byte, error) {

	if ctx == nil {
		ctx = c.Context()
	}
	hdr := c.ImgHeaders(urlStr)
	var lastErr error
	for attempt := 0; attempt < max(1, c.MaxRetries); attempt++ {
		if err := c.checkStop(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
		if err != nil {
			return nil, apiErr("bad url: %v", err)
		}
		req.Header.Set("User-Agent", c.UserAgent)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		httpClient := *c.HTTP
		httpClient.Timeout = c.Timeout
		resp, err := c.doNetwork(req, &httpClient, false)
		if err != nil {
			if ctx.Err() != nil {
				return nil, DownloadCancelled
			}
			lastErr = err
			slog.Debug("Изображение: повтор после ошибки сети", "host", uHost(urlStr), "attempt", attempt+1, "err", RedactSecrets(err.Error()))
			if attempt+1 < max(1, c.MaxRetries) && retryPause(ctx, time.Duration(600*(attempt+1))*time.Millisecond) != nil {
				return nil, DownloadCancelled
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (256<<20)+1))
		resp.Body.Close()
		if ctx.Err() != nil {
			return nil, DownloadCancelled
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if len(data) > 256<<20 {
			return nil, apiErr("image exceeds 256 MB")
		}
		if err := c.checkStop(); err != nil {
			return nil, err
		}
		if resp.StatusCode == 200 {
			if requireImage && !looksLikeImage(data) {
				return nil, apiErr("Invalid image response for %s", urlStr)
			}
			return data, nil
		}
		lastErr = apiErrStatus(resp.StatusCode, "HTTP %d for %s", resp.StatusCode, RedactSecrets(urlStr))
		if resp.StatusCode == 403 && strings.EqualFold(resp.Header.Get("CF-Mitigated"), "challenge") {
			lastErr = cfChallengeError(urlStr)
		}
		if resp.StatusCode == 429 {
			lastErr = rateLimitedError(urlStr)
			if Throttle.CooldownRemaining(urlStr) > LongCooldown {
				return nil, lastErr
			}
		}
		if resp.StatusCode != 429 && resp.StatusCode != 500 &&
			resp.StatusCode != 502 && resp.StatusCode != 503 {
			slog.Debug("Изображение: ответ без повтора", "host", uHost(urlStr), "status", resp.StatusCode)
			return nil, lastErr
		}
		slog.Debug("Изображение: повтор", "host", uHost(urlStr), "status", resp.StatusCode, "attempt", attempt+1)
		if attempt+1 < max(1, c.MaxRetries) && resp.StatusCode != 429 && retryPause(ctx, time.Duration(1000*(attempt+1))*time.Millisecond) != nil {
			return nil, DownloadCancelled
		}
	}
	return nil, apiErr("Fetch failed: %v", lastErr)
}

// StreamToFile streams url (or candidates tried in order; a 404 falls
// through) to dest, hashing incrementally. progress(received, total) may
// abort by returning false (DownloadCancelled). expectedHash: md5 (32 hex)
// or sha1 (40). Hash mismatch and truncated transfer are transfer errors and
// take the normal retry path. Returns bytes written.
func (c *ApiClient) StreamToFile(ctx context.Context, urls []string, dest string, progress func(int64, int64) bool, expectedHash string) (int64, error) {
	if ctx == nil {
		ctx = c.Context()
	}
	var lastErr error
	started := time.Now()
	candidates := c.MediaURLs(urls)
	for candidate, u := range candidates {
		// A partial belongs to one URL; never append a different derivative.
		if candidate > 0 {
			_ = os.Remove(dest)
			slog.Debug("Загрузка: следующий вариант URL", "candidate", candidate, "host", uHost(u))
		}
		// Resumes that made progress (idle watchdog, dropped connection) do
		// not use up attempts, up to a bound.
		progressRetries := 0
		for attempt := 0; attempt < max(1, c.MaxRetries); attempt++ {
			if ctx.Err() != nil || c.checkStop() != nil {
				return 0, DownloadCancelled
			}
			var offset int64
			if info, err := os.Stat(dest); err == nil {
				offset = info.Size()
			}
			req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err != nil {
				lastErr = err
				break
			}
			req.Header.Set("User-Agent", c.UserAgent)
			req.Header.Set("Accept-Encoding", "identity")
			for k, v := range c.ImgHeaders(u) {
				req.Header.Set(k, v)
			}
			if offset > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			}
			// Keep the injected transport/session (also used by the offline
			// tests). Streams have no total deadline: header timeout + idle watchdog.
			resp, err := c.doStream(req)
			if err != nil {
				if ctx.Err() != nil {
					return 0, DownloadCancelled
				}
				lastErr = err
				slog.Warn("Загрузка: ошибка соединения", "host", uHost(u), "attempt", attempt+1,
					"offset", offset, "timeout", isNetworkTimeout(err), "err", RedactSecrets(err.Error()))
				if attempt+1 < max(1, c.MaxRetries) {
					if err := retryPause(ctx, time.Duration(800*(attempt+1))*time.Millisecond); err != nil {
						return 0, err
					}
				}
				continue
			}
			if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
				resp.Body.Close()
				_ = os.Remove(dest)
				lastErr = apiErr("HTTP 416: partial range is stale")
				continue
			}
			if resp.StatusCode != 200 && resp.StatusCode != 206 {
				if resp.StatusCode == 403 && strings.EqualFold(resp.Header.Get("CF-Mitigated"), "challenge") {
					lastErr = cfChallengeError(u)
				} else {
					lastErr = apiErrStatus(resp.StatusCode, "HTTP %d for %s", resp.StatusCode, RedactSecrets(u))
				}
				resp.Body.Close()
				if resp.StatusCode == 404 {
					slog.Debug("Загрузка: 404, пробую следующий URL", "host", uHost(u))
					break
				}
				if resp.StatusCode == 403 {
					slog.Debug("Загрузка: 403", "host", uHost(u))
					break
				}
				if resp.StatusCode == http.StatusTooManyRequests {
					lastErr = rateLimitedError(u)
					// A long pause frees the download slot: the manager re-queues.
					if left := Throttle.CooldownRemaining(u); left > LongCooldown || attempt+1 >= max(1, c.MaxRetries) {
						slog.Warn("Загрузка: лимит сайта", "host", uHost(u), "pause_s", roundSeconds(left))
						return 0, lastErr
					}
					continue
				}
				slog.Warn("Загрузка: ответ сервера", "host", uHost(u), "status", resp.StatusCode, "attempt", attempt+1)
				if resp.StatusCode >= 500 && attempt+1 < max(1, c.MaxRetries) {
					if err := retryPause(ctx, time.Duration(1200*(attempt+1))*time.Millisecond); err != nil {
						return 0, err
					}
				}
				continue
			}
			// Resuming is useful only when the server honours ranges.
			resumable := resp.StatusCode == http.StatusPartialContent || strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
			if resp.StatusCode == 200 {
				offset = 0
			} else {
				var start, end, total int64
				if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil || start != offset || end < start || total <= end {
					resp.Body.Close()
					_ = os.Remove(dest)
					lastErr = apiErr("invalid Content-Range")
					continue
				}
			}
			var hasher hash.Hash
			switch len(expectedHash) {
			case 32:
				hasher = md5.New()
			case 40:
				hasher = sha1.New()
			case 64:
				hasher = sha256.New()
			}
			if offset > 0 && hasher != nil {
				f, err := os.Open(dest)
				if err != nil {
					resp.Body.Close()
					return 0, err
				}
				_, err = io.Copy(hasher, f)
				f.Close()
				if err != nil {
					resp.Body.Close()
					return 0, err
				}
			}
			flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
			if offset > 0 {
				flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				resp.Body.Close()
				return 0, err
			}
			f, err := os.OpenFile(dest, flags, 0644)
			if err != nil {
				resp.Body.Close()
				return 0, err
			}
			total := resp.ContentLength
			if total >= 0 {
				total += offset
			} else {
				total = 0
			}
			written := offset
			buf := make([]byte, 65536)
			var transferErr error
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					wn, writeErr := f.Write(buf[:n])
					written += int64(wn)
					if writeErr != nil {
						transferErr = writeErr
						break
					}
					if hasher != nil {
						_, _ = hasher.Write(buf[:wn])
					}
				}
				if ctx.Err() != nil || c.checkStop() != nil || (progress != nil && !progress(written, total)) {
					transferErr = DownloadCancelled
					break
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					transferErr = readErr
					break
				}
			}
			closeErr := f.Close()
			resp.Body.Close()
			if transferErr == nil {
				transferErr = closeErr
			}
			if transferErr == DownloadCancelled {
				return written, DownloadCancelled
			}
			if transferErr != nil {
				lastErr = transferErr
				slog.Warn("Загрузка: обрыв, докачка", "host", uHost(u), "received", written, "total", total,
					"timeout", isNetworkTimeout(transferErr), "err", RedactSecrets(transferErr.Error()))
				if resumable && written > offset && progressRetries < 20 {
					progressRetries++
					attempt--
				}
				continue
			}
			if total > 0 && written != total {
				lastErr = apiErr("обрыв передачи: получено %d из %d байт", written, total)
				slog.Warn("Загрузка: неполный ответ, докачка", "host", uHost(u), "received", written, "total", total)
				if resumable && written > offset && progressRetries < 20 {
					progressRetries++
					attempt--
				}
				continue
			}
			if hasher != nil && !strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), expectedHash) {
				_ = os.Remove(dest)
				lastErr = apiErr("хеш не совпадает — файл повреждён")
				slog.Warn("Загрузка: хеш не совпал, повтор с нуля", "host", uHost(u), "attempt", attempt+1)
				continue
			}
			if written == 0 {
				lastErr = apiErr("пустой файл")
				continue
			}
			slog.Debug("Загрузка: файл получен", "host", uHost(u), "bytes", written, "resumed_from", offset,
				"elapsed_ms", time.Since(started).Milliseconds())
			return written, nil
		}
	}
	if lastErr == nil {
		lastErr = apiErr("нет доступного URL оригинала")
	}
	var responseErr *ApiError
	if errors.As(lastErr, &responseErr) {
		return 0, apiErrStatus(responseErr.Status, "Download failed: %v", lastErr)
	}
	return 0, apiErr("Download failed: %v", lastErr)
}

// MediaURLs yields concrete transfer candidates. A successful primary stops
// iteration before any media_assets request, matching Python's generator.
// The same sequence is used by downloads and the viewer's streaming proxy.
func (c *ApiClient) MediaURLs(refs []string) iter.Seq2[int, string] {
	return func(yield func(int, string) bool) {
		index := 0
		emit := func(raw string) bool {
			more := yield(index, raw)
			index++
			return more
		}
		for _, ref := range refs {
			if !strings.HasPrefix(ref, "danbooru-media:") {
				if !emit(ref) {
					return
				}
				continue
			}
			u, err := url.Parse(ref)
			if err != nil {
				continue
			}
			if primary := firstOf(u.Query(), "primary", "fallback"); primary != "" && u.Query().Get("force_assets") != "1" {
				if !emit(primary) {
					return
				}
			}
			for _, raw := range c.ResolveMediaURLs(ref, false) {
				if !emit(raw) {
					return
				}
			}
		}
	}
}

// ResolveMediaURLs resolves a lazy Danbooru image reference to ordered CDN
// candidates. Called only by raw image transfer, never while searching. The
// asset MD5 must match before any variant is accepted.
func (c *ApiClient) ResolveMediaURLs(ref string, includePrimary bool) []string {
	u, err := url.Parse(ref)
	if err != nil || !strings.HasPrefix(ref, "danbooru-media:") {
		if ref != "" && includePrimary {
			return []string{ref}
		}
		return nil
	}
	q := u.Query()
	primary := firstOf(q, "primary", "fallback")
	if q.Get("force_assets") == "1" {
		primary = ""
	}
	expectedMd5 := strings.ToLower(q.Get("md5"))
	siteKey := q.Get("site")
	if siteKey == "" {
		siteKey = "danbooru"
	}
	mediaSite := GetSite(siteKey)
	if mediaSite == nil {
		return primaryOrNil(primary, includePrimary)
	}
	mediaEngine := Registry.EngineFor(mediaSite)
	if mediaEngine == nil || !mediaEngine.SupportsMediaAssets() {
		return primaryOrNil(primary, includePrimary)
	}
	postId := atoiOr0(u.Host)
	if postId <= 0 {
		return primaryOrNil(primary, includePrimary)
	}
	endpointId := postId
	if a := q.Get("asset"); a != "" {
		endpointId = atoiOr0(a)
		if endpointId <= 0 {
			return primaryOrNil(primary, includePrimary)
		}
	}
	kind := strings.Trim(u.Path, "/")
	var wanted []string
	switch kind {
	case "preview":
		wanted = []string{"180x180", "360x360", "720x720", "sample", "original"}
	case "sample":
		wanted = []string{"sample", "720x720", "360x360", "original", "180x180"}
	case "original":
		wanted = []string{"original"}
	default:
		return primaryOrNil(primary, includePrimary)
	}
	asset, err := c.GetJSON(c.Context(),
		fmt.Sprintf("%s/media_assets/%d.json", mediaSite.Base(), endpointId),
		nil, nil)
	if err != nil {
		return primaryOrNil(primary, includePrimary)
	}
	assetMap, _ := asset.(map[string]any)
	assetMd5 := ""
	if assetMap != nil {
		assetMd5 = strings.ToLower(str(assetMap["md5"]))
	}
	if expectedMd5 != "" && assetMd5 != expectedMd5 {
		return primaryOrNil(primary, includePrimary)
	}
	variants := map[string]string{}
	if assetMap != nil {
		if list, ok := assetMap["variants"].([]any); ok {
			for _, v := range list {
				vm, _ := v.(map[string]any)
				if vm == nil {
					continue
				}
				vt := str(vm["type"])
				vu := str(vm["url"])
				if vt != "" && vu != "" {
					variants[vt] = vu
				}
			}
		}
	}
	var resolved []string
	seen := map[string]bool{}
	for _, vt := range wanted {
		vu := variants[vt]
		if vu != "" && !seen[vu] {
			seen[vu] = true
			resolved = append(resolved, vu)
		}
	}
	if includePrimary && primary != "" && !seen[primary] {
		resolved = append(resolved, primary)
	}
	return resolved
}

func primaryOrNil(primary string, include bool) []string {
	if include && primary != "" {
		return []string{primary}
	}
	return nil
}
