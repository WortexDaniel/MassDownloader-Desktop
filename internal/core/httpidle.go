package core

import (
	"net/http"
	"sync"
	"time"
)

// idleHTTP tracks shared transports so Shutdown can drop keep-alives
// promptly (mirrors Python process teardown closing the requests session).
var (
	idleMu     sync.Mutex
	sharedHTTP = map[*http.Transport]bool{}
)

// RegisterHTTP marks a client's transport for shutdown cleanup.
func RegisterHTTP(c *http.Client) {
	if c == nil {
		return
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		return
	}
	idleMu.Lock()
	sharedHTTP[tr] = true
	idleMu.Unlock()
}

// CloseAllHTTP closes idle connections of registered transports.
func CloseAllHTTP() {
	idleMu.Lock()
	defer idleMu.Unlock()
	for tr := range sharedHTTP {
		tr.CloseIdleConnections()
	}
}

// CloseIdle closes this client's idle keep-alive connections.
func (c *ApiClient) CloseIdle() {
	if tr, ok := c.HTTP.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// Ensure default transport registration on client creation.
func init() {
	// NewClient uses http.DefaultTransport-like settings; wrap so CloseIdle
	// always has a concrete *http.Transport.
	defaultTransportOnce.Do(func() {
		defaultTransport = &http.Transport{
			Proxy:               defaultProxy,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		}
	})
}

var (
	defaultTransport     *http.Transport
	defaultTransportOnce sync.Once
)
