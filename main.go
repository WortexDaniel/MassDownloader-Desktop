// Command massdownloader — the Wails application entry point.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"massdownloader/internal/appdir"
	"massdownloader/internal/applog"
	"massdownloader/internal/core"
	"massdownloader/internal/downloader"
	"massdownloader/internal/favorites"
	"massdownloader/internal/folderindex"
	"massdownloader/internal/history"
	"massdownloader/internal/imagecache"
	"massdownloader/internal/library"
	"massdownloader/internal/reverse"
	"massdownloader/internal/settings"
	"massdownloader/internal/sites"
	"massdownloader/internal/store"
	"massdownloader/internal/videocache"
)

//go:embed all:frontend/dist
var assets embed.FS

// App is the binding bridge: every public method is callable from the
// frontend via the generated wails-js bindings.
type App struct {
	ctx                context.Context
	settings           *settings.Settings
	favorites          *favorites.Favorites
	history            *history.History
	manager            *downloader.Manager
	loader             *imagecache.Loader
	videoCache         *videocache.Cache
	dataMu             sync.Mutex
	folderIndex        *folderindex.Index
	mediaMu            sync.Mutex
	localFiles         map[string]string
	feedMu             sync.Mutex
	feed               *feedServer
	libraryWindow      *feedServer
	libraryMu          sync.Mutex
	library            *library.Service
	libraryError       error
	libraryState       library.Progress
	libraryCancel      context.CancelFunc
	libraryWG          sync.WaitGroup
	libraryClosing     bool
	libraryThumbMu     sync.Mutex
	libraryThumbWrites int
	metadata           *core.MetadataCache
	operations         operationRegistry
	feedBuffer         feedBuffer
}

func NewApp() *App {
	return &App{folderIndex: folderindex.New(), localFiles: map[string]string{}, metadata: core.NewMetadataCache()}
}

func (a *App) emit(name string, data ...any) {
	a.emitFeed(name, data...)
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, name, data...)
	}
}

// eventBridge forwards downloader events to the frontend.
type eventBridge struct{ a *App }

func (e *eventBridge) ScanUpdate(scanned, queued, skipped int) {
	e.a.emit("scan:update", scanned, queued, skipped)
}
func (e *eventBridge) ScanFinished(errText string) {
	e.a.emit("scan:finished", errText)
}
func (e *eventBridge) JobQueued(id int, label string) {
	e.a.emit("job:queued", id, label)
}
func (e *eventBridge) JobStarted(id int, label string) {
	e.a.emit("job:started", id, label)
}
func (e *eventBridge) JobProgress(id int, received, total int64) {
	e.a.emit("job:progress", id, received, total)
}
func (e *eventBridge) JobFinished(id int, status, errText string) {
	if status == "done" || status == "skipped" {
		e.a.folderIndex.Invalidate()
	}
	e.a.emit("job:finished", id, status, errText)
}
func (e *eventBridge) AllFinished() { e.a.emit("wave:finished") }

// startup opens this installation's settings and stores, wires the registry
// and starts the download manager.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	applog.Subscribe(func(e applog.Entry) { a.emit("log:entry", e) })
	if err := applog.Init(appdir.Dir() + "/logs"); err != nil {
		fmt.Fprintln(os.Stderr, "log init:", err)
	}
	settings.Migrate()
	sites.Register()
	reverse.RegisterProviders()

	st, err := settings.New("")
	if err != nil {
		applog.Write("ERROR", "settings: "+err.Error())
		os.Exit(1)
	}
	a.settings = st
	if err := applog.SetLevel(st.GetString("log_level")); err != nil {
		applog.Write("WARN", err.Error())
	}
	if err := core.SetProxyConfig(a.GetProxySettings()); err != nil {
		applog.Write("ERROR", "proxy: "+err.Error())
		_ = core.SetProxyConfig(core.DefaultProxyConfig())
	}

	appDB := appdir.Dir() + "/app.db"
	if a.favorites, err = favorites.New(appDB); err != nil {
		applog.Write("ERROR", "favorites: "+err.Error())
		os.Exit(1)
	}
	if a.history, err = history.Open(appdir.Dir() + "/history.db"); err != nil {
		applog.Write("ERROR", "history: "+err.Error())
		os.Exit(1)
	}
	a.manager = downloader.NewManager(a.history, &eventBridge{a})
	if _, err := a.libraryService(); err != nil {
		applog.Write("ERROR", "Библиотека: "+err.Error())
	}
	a.manager.Client.Metadata = a.metadata
	a.manager.SetDedupMode(st.GetString("dedup_mode"))
	a.manager.SetSkipSeen(st.GetBool("skip_seen"))
	if n := int(st.GetInt("threads")); n > 0 {
		a.manager.SetThreads(n)
	}
	a.loader = imagecache.NewLoaderWithCap(8, st.GetInt("thumb_cache_mb"))
	a.loader.Client.Metadata = a.metadata
	a.videos()
	a.loader.Loaded = func(u string, data []byte) { a.emit("thumb:loaded", u, data) }
	a.loader.Failed = func(u, err string) { a.emit("thumb:failed", u, err) }
	a.loader.SetCap(st.GetInt("thumb_cache_mb"))
	a.configureMediaClient(a.manager.Client)
	a.configureMediaClient(a.loader.Client)
	if data, err := json.Marshal(st.Get("credentials")); err == nil {
		var creds map[string]map[string]string
		if json.Unmarshal(data, &creds) == nil {
			a.manager.Client.SetCredentials(creds)
			a.loader.Client.SetCredentials(creds)
		}
	}
	if rl, ok := st.Get("rate_limits").(map[string]any); ok {
		ov := map[string]float64{}
		for k, v := range rl {
			if t, ok := v.(float64); ok {
				ov[k] = t
			}
		}
		core.SetRateLimits(ov)
	}
	applog.Write("INFO", fmt.Sprintf("Приложение готово: %d сайтов, %d движков", len(core.Sites()), core.EngineCount()))
}

// windowBackground is the theme's --bg (frontend/src/theme.css). WebView2 paints
// it before the page loads, so a window never flashes white on start or resize.
var windowBackground = &options.RGBA{R: 15, G: 19, B: 24, A: 255}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--feed-window" || os.Args[1] == "--library-window") {
		feed := &FeedWindow{endpoint: os.Getenv("MASSDOWNLOADER_FEED_ENDPOINT"), token: os.Getenv("MASSDOWNLOADER_FEED_TOKEN"), client: &http.Client{}}
		title, width, height, minWidth := "Лента — MassDownloader", 980, 850, 650
		profile := "webview-feed"
		if os.Args[1] == "--library-window" {
			feed.kind = "library"
			title = "Библиотека — MassDownloader"
			width = 1240
			height = 850
			minWidth = 850
			profile = "webview-library"
		}
		if feed.endpoint == "" || feed.token == "" {
			os.Exit(1)
		}
		err := wails.Run(&options.App{
			Title: title, Width: width, Height: height, MinWidth: minWidth, MinHeight: 500,
			BackgroundColour: windowBackground,
			AssetServer:      &assetserver.Options{Assets: assets, Handler: feed}, OnStartup: feed.startup,
			OnDomReady: feed.show,
			OnShutdown: func(context.Context) {
				if feed.cancel != nil {
					feed.cancel()
				}
			},
			Bind: []interface{}{feed}, Windows: &windows.Options{Theme: windows.Dark, WebviewUserDataPath: filepath.Join(appdir.Dir(), profile)}, Frameless: true,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	app := NewApp()
	err := wails.Run(&options.App{
		Title:            "MassDownloader",
		Width:            1280,
		Height:           800,
		MinWidth:         780,
		MinHeight:        560,
		BackgroundColour: windowBackground,
		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: mediaHandler{app},
		},
		OnStartup: app.startup,
		OnShutdown: func(_ context.Context) {
			app.closeLibrary()
			app.operations.close()
			if app.metadata != nil {
				app.metadata.Close()
			}
			app.closeFeed()
			if app.videoCache != nil {
				app.videoCache.Close()
			}
			if app.loader != nil {
				app.loader.Shutdown()
			}
			if app.manager != nil {
				app.manager.Shutdown(5 * time.Second)
			}
			applog.Close()
			if app.history != nil {
				app.history.Close()
			}
			store.CloseAll()
		},
		Bind: []interface{}{app},
		Windows: &windows.Options{
			Theme:               windows.Dark,
			WebviewUserDataPath: filepath.Join(appdir.Dir(), "webview"),
		},
		Frameless: true,
	})
	if err != nil {
		println("Error:", err.Error())
		os.Exit(1)
	}
}
