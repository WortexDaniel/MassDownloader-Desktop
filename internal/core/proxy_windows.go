package core

import (
	"net/http"
	"net/url"

	winregistry "golang.org/x/sys/windows/registry"
)

// Read the current user's manual browser proxy on every new connection, so
// changes made by v2rayN or Windows take effect without restarting the app.
// PAC-only configurations are left direct; this handles the manual system
// proxy used by this installation without changing any Windows settings.
func platformProxy(req *http.Request) (*url.URL, error) {
	key, err := winregistry.OpenKey(winregistry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, winregistry.QUERY_VALUE)
	if err != nil {
		return nil, nil
	}
	defer key.Close()
	enabled, _, _ := key.GetIntegerValue("ProxyEnable")
	server, _, _ := key.GetStringValue("ProxyServer")
	bypass, _, _ := key.GetStringValue("ProxyOverride")
	return windowsProxyURL(req, enabled != 0, server, bypass)
}
