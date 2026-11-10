package core

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

type ProxyConfig struct {
	Mode     string `json:"mode"`
	Type     string `json:"type"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func DefaultProxyConfig() ProxyConfig {
	return ProxyConfig{Mode: "system", Type: "socks5", Host: "127.0.0.1", Port: 10808}
}

var activeProxyConfig atomic.Pointer[ProxyConfig]

func CurrentProxyConfig() ProxyConfig {
	if config := activeProxyConfig.Load(); config != nil {
		return *config
	}
	return DefaultProxyConfig()
}

func NormalizeProxyConfig(config ProxyConfig) (ProxyConfig, error) {
	config.Mode = strings.ToLower(strings.TrimSpace(config.Mode))
	config.Type = strings.ToLower(strings.TrimSpace(config.Type))
	config.Host = strings.Trim(strings.TrimSpace(config.Host), "[]")
	config.Username = strings.TrimSpace(config.Username)
	if config.Mode == "" {
		config.Mode = "system"
	}
	if config.Type == "" {
		config.Type = "socks5"
	}
	if config.Mode != "disabled" && config.Mode != "system" && config.Mode != "custom" {
		return config, fmt.Errorf("неизвестный режим прокси")
	}
	if config.Type != "http" && config.Type != "socks5" {
		return config, fmt.Errorf("выберите HTTP или SOCKS5")
	}
	if config.Mode == "custom" {
		if config.Host == "" || strings.ContainsAny(config.Host, " /\\@?#\t\r\n") || (strings.Contains(config.Host, ":") && net.ParseIP(config.Host) == nil) {
			return config, fmt.Errorf("укажите адрес прокси без протокола и порта")
		}
		if config.Port < 1 || config.Port > 65535 {
			return config, fmt.Errorf("порт прокси должен быть от 1 до 65535")
		}
	}
	return config, nil
}

func SetProxyConfig(config ProxyConfig) error {
	config, err := NormalizeProxyConfig(config)
	if err != nil {
		return err
	}
	activeProxyConfig.Store(&config)
	if defaultTransport != nil {
		defaultTransport.CloseIdleConnections()
	}
	return nil
}

// Every existing client reads the selected mode at the next request. Custom
// and disabled modes override OS settings; system mode follows the browser.
func defaultProxy(req *http.Request) (*url.URL, error) {
	config := CurrentProxyConfig()
	switch config.Mode {
	case "disabled":
		return nil, nil
	case "custom":
		if proxyBypassed(req.URL, "") {
			return nil, nil
		}
		proxy := &url.URL{Scheme: config.Type, Host: net.JoinHostPort(config.Host, fmt.Sprint(config.Port))}
		if config.Username != "" || config.Password != "" {
			proxy.User = url.UserPassword(config.Username, config.Password)
		}
		return proxy, nil
	default:
		return platformProxy(req)
	}
}

// Feed clients deliberately use MaxRetries=1 to avoid retrying HTTP 429 for
// every author. TCP reconnection is separate: it must still recover a brief
// v2rayN restart for feed, search and media without changing the route.
func reconnectLocalProxy(req *http.Request, base http.RoundTripper, cause error) (*http.Response, error) {
	transport, ok := base.(*http.Transport)
	if !ok || transport.Proxy == nil || req.Context().Err() != nil {
		return nil, cause
	}
	proxy, err := transport.Proxy(req)
	if err != nil || proxy == nil || !retryableLocalProxyFailure(req, proxy, cause) {
		return nil, cause
	}
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return nil, cause
	}
	// A fresh pool discards connections to the old proxy process. Freeze the
	// chosen endpoint so a temporary ProxyEnable change cannot cause a bypass.
	retryTransport := transport.Clone()
	retryTransport.Proxy = http.ProxyURL(proxy)
	defer retryTransport.CloseIdleConnections()
	for _, delay := range []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second} {
		if err := waitContext(req.Context(), delay); err != nil {
			return nil, err
		}
		retry := req.Clone(req.Context())
		if req.Body != nil && req.Body != http.NoBody {
			retry.Body, err = req.GetBody()
			if err != nil {
				return nil, cause
			}
		}
		resp, err := retryTransport.RoundTrip(retry)
		if err == nil {
			return resp, nil
		}
		cause = err
		if !retryableLocalProxyFailure(req, proxy, cause) {
			return nil, cause
		}
	}
	return nil, fmt.Errorf("Прокси %s недоступен: %w", proxy.Host, cause)
}

func retryableLocalProxyFailure(req *http.Request, proxy *url.URL, cause error) bool {
	host := proxy.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	if localProxyDialFailed(proxy, cause) {
		return true
	}
	// A failed read may occur after a request reached the destination. Only
	// replay read-only HTTP methods in this case; POST is safe only on dial.
	if req.Method != "GET" && req.Method != "HEAD" {
		return false
	}
	if errors.Is(cause, io.EOF) || errors.Is(cause, io.ErrUnexpectedEOF) {
		return true
	}
	for err := cause; err != nil; err = errors.Unwrap(err) {
		if op, ok := err.(*net.OpError); ok && (op.Op == "read" || op.Op == "write") {
			if address, ok := op.Addr.(*net.TCPAddr); ok && address.IP.IsLoopback() && fmt.Sprint(address.Port) == proxyPort(proxy) && (host == "localhost" || address.IP.Equal(ip)) {
				return true
			}
		}
	}
	return false
}

func proxyPort(proxy *url.URL) string {
	port := proxy.Port()
	if port == "" {
		switch proxy.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return port
}

func localProxyDialFailed(proxy *url.URL, cause error) bool {
	port := proxyPort(proxy)
	host := proxy.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	for err := cause; err != nil; err = errors.Unwrap(err) {
		op, ok := err.(*net.OpError)
		if !ok || op.Op != "dial" {
			continue
		}
		address, ok := op.Addr.(*net.TCPAddr)
		if ok && address.IP.IsLoopback() && fmt.Sprint(address.Port) == port {
			return host == "localhost" || address.IP.Equal(ip)
		}
	}
	return false
}

// Windows stores either a single HTTP CONNECT proxy or a semicolon-separated
// per-protocol list (http=...;https=...;socks=...). HTTPS targets still normally
// use an http:// proxy: TLS starts after CONNECT, not on the proxy connection.
func windowsProxyURL(req *http.Request, enabled bool, server, bypass string) (*url.URL, error) {
	if !enabled || strings.TrimSpace(server) == "" || proxyBypassed(req.URL, bypass) {
		return nil, nil
	}
	address := strings.TrimSpace(server)
	protocol := "http"
	if strings.Contains(address, "=") {
		address = ""
		var socks string
		for _, item := range strings.Split(server, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(item), "=")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case req.URL.Scheme:
				address = strings.TrimSpace(value)
			case "socks":
				socks = strings.TrimSpace(value)
			}
		}
		if address == "" {
			address, protocol = socks, "socks5"
		}
	}
	if address == "" {
		return nil, nil
	}
	if !strings.Contains(address, "://") {
		address = protocol + "://" + address
	}
	proxy, err := url.Parse(address)
	if err != nil || proxy.Hostname() == "" || proxy.Path != "" || proxy.RawQuery != "" || proxy.Fragment != "" {
		return nil, fmt.Errorf("некорректный адрес системного прокси Windows")
	}
	switch proxy.Scheme {
	case "http", "https", "socks5", "socks5h":
		return proxy, nil
	default:
		return nil, fmt.Errorf("неподдерживаемый протокол системного прокси Windows")
	}
}

func proxyBypassed(target *url.URL, bypass string) bool {
	host := strings.ToLower(target.Hostname())
	if host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		return true // local media/feed servers must remain reachable
	}
	for _, pattern := range strings.Split(bypass, ";") {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if pattern == "<local>" {
			if !strings.Contains(host, ".") && !strings.Contains(host, ":") {
				return true
			}
			continue
		}
		// Windows bypass entries use * and ? wildcards, optionally with a port.
		matchHost := host
		if strings.Contains(pattern, ":") {
			matchHost = strings.ToLower(target.Host)
		}
		expr := regexp.QuoteMeta(pattern)
		expr = strings.ReplaceAll(strings.ReplaceAll(expr, `\*`, ".*"), `\?`, ".")
		if regexp.MustCompile("^" + expr + "$").MatchString(matchHost) {
			return true
		}
	}
	return false
}
