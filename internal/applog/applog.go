// Package applog — rotating file log + redaction of secrets.
// Port of app/log.py (the parts the core needs; the viewer is frontend).
package applog

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// level is the minimum record level written to app.log and the log view.
// INFO by default: per-request network traces are DEBUG.
var level = func() *slog.LevelVar { v := new(slog.LevelVar); v.Set(slog.LevelInfo); return v }()

// Levels lists accepted setting values, most verbose first.
var Levels = []string{"debug", "info", "warn", "error"}

// ParseLevel maps a setting value to a slog level.
func ParseLevel(name string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// SetLevel changes the minimum level live; unknown names are rejected.
func SetLevel(name string) error {
	lvl, ok := ParseLevel(name)
	if !ok {
		return fmt.Errorf("неизвестный уровень логирования: %q", name)
	}
	if level.Level() != lvl {
		// Logged at the more verbose of both levels so the switch is visible.
		old := level.Level()
		level.Set(min(old, lvl))
		slog.Info("Уровень логирования изменён", "from", strings.ToLower(old.String()), "to", strings.ToLower(lvl.String()))
		level.Set(lvl)
	}
	return nil
}

// Level is the current minimum level as a setting value.
func Level() string { return strings.ToLower(level.Level().String()) }

var (
	mu         sync.Mutex
	file       *os.File
	logDir     string
	redactKeys = map[string]bool{
		"api_key": true, "user_id": true, "login": true, "passwd": true,
		"auth_token": true, "token": true, "ct0": true, "cookie": true,
	}
)

// Init opens data/logs/app.log (archiving yesterday's file to app.N.log).
func Init(dir string) error {
	mu.Lock()
	defer mu.Unlock()
	logDir = dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "app.log")
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		file = f
	} else {
		return err
	}
	handler := safeHandler{slog.NewTextHandler(rotatingWriter{}, &slog.HandlerOptions{Level: level})}
	slog.SetDefault(slog.New(handler))
	return nil
}

// Close flushes and closes the log file.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		file.Close()
		file = nil
	}
}

// LogPath is the active app.log path.
func LogPath() string { return filepath.Join(logDir, "app.log") }

// RedactSecrets strips credential-looking values from a URL/error string so
// query credentials such as Gelbooru's api_key never reach logs.
func RedactSecrets(s string) string {
	if u, err := url.Parse(s); err == nil && u.RawQuery != "" {
		q := u.Query()
		changed := false
		for key := range q {
			if redactKeys[key] && q.Get(key) != "" {
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

// Write appends one line directly (used by the panic handler).
func Write(level, msg string) {
	lvl := slog.LevelInfo
	switch strings.ToUpper(level) {
	case "ERROR":
		lvl = slog.LevelError
	case "WARN", "WARNING":
		lvl = slog.LevelWarn
	case "DEBUG":
		lvl = slog.LevelDebug
	}
	slog.Log(context.Background(), lvl, RedactSecrets(msg))
}
