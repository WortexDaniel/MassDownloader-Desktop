package applog

import (
	"context"
	"fmt"
	"log/slog"
	"massdownloader/internal/core"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Entry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

var entryMu sync.Mutex
var entries = []Entry{}
var listener func(Entry)

func Subscribe(fn func(Entry)) { entryMu.Lock(); listener = fn; entryMu.Unlock() }
func Entries() []Entry         { entryMu.Lock(); defer entryMu.Unlock(); return append([]Entry{}, entries...) }
func ClearEntries()            { entryMu.Lock(); entries = []Entry{}; entryMu.Unlock() }

type safeHandler struct{ slog.Handler }

func (h safeHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, core.RedactSecrets(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool { clean.AddAttrs(safeAttr(a)); return true })
	err := h.Handler.Handle(ctx, clean)
	message := clean.Message
	clean.Attrs(func(a slog.Attr) bool { message += " " + a.Key + "=" + a.Value.String(); return true })
	entry := Entry{r.Time.Format("2006-01-02 15:04:05"), r.Level.String(), message}
	entryMu.Lock()
	entries = append(entries, entry)
	if len(entries) > 10000 {
		entries = entries[len(entries)-10000:]
	}
	fn := listener
	entryMu.Unlock()
	if fn != nil {
		fn(entry)
	}
	return err
}
func safeAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	k := strings.ToLower(a.Key)
	if redactKeys[k] || k == "cf_clearance" || k == "password" || k == "authorization" || k == "phpsessid" {
		return slog.String(a.Key, "<скрыто>")
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		out := make([]slog.Attr, len(attrs))
		for i, v := range attrs {
			out[i] = safeAttr(v)
		}
		a.Value = slog.GroupValue(out...)
	} else {
		a.Value = slog.StringValue(core.RedactSecrets(a.Value.String()))
	}
	return a
}
func (h safeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = safeAttr(a)
	}
	return safeHandler{h.Handler.WithAttrs(out)}
}
func (h safeHandler) WithGroup(name string) slog.Handler {
	return safeHandler{h.Handler.WithGroup(name)}
}

type rotatingWriter struct{}

func (rotatingWriter) Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	if file == nil {
		return len(p), nil
	}
	if st, err := file.Stat(); err == nil && st.Size()+int64(len(p)) > 5*1024*1024 {
		file.Close()
		for i := 4; i >= 1; i-- {
			src := filepath.Join(logDir, fmt.Sprintf("app.%d.log", i))
			dst := filepath.Join(logDir, fmt.Sprintf("app.%d.log", i+1))
			_ = os.Remove(dst)
			_ = os.Rename(src, dst)
		}
		_ = os.Remove(filepath.Join(logDir, "app.1.log"))
		_ = os.Rename(LogPath(), filepath.Join(logDir, "app.1.log"))
		var err error
		file, err = os.OpenFile(LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return 0, err
		}
	}
	return file.Write(p)
}
