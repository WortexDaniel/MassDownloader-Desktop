//go:build !windows

package core

import (
	"net/http"
	"net/url"
)

func platformProxy(req *http.Request) (*url.URL, error) {
	return http.ProxyFromEnvironment(req)
}
