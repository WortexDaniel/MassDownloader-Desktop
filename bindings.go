package main

// App bindings — every method the MD3 frontend calls. Ports of the Qt
// controllers' public surface (main_window wiring, dialogs, panels).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"massdownloader/internal/applog"
	"massdownloader/internal/core"
	"massdownloader/internal/downloader"
	"massdownloader/internal/history"
	"massdownloader/internal/imagecache"
	"massdownloader/internal/reverse"
	"massdownloader/internal/videocache"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// -- sites & engines ---------------------------------------------------------

// SiteKeys lists registered site keys in registration order.
func (a *App) SiteKeys() []string { return core.SiteKeys() }

type SiteInfo struct {
	Key            string           `json:"key"`
	Label          string           `json:"label"`
	Domain         string           `json:"domain"`
	Engine         string           `json:"engine"`
	Nick           string           `json:"nick"`
	PageLimit      int              `json:"page_limit"`
	RequiresAuth   bool             `json:"requires_auth"`
	OptionalAuth   bool             `json:"optional_auth"`
	CfProtected    bool             `json:"cf_protected"`
	CfExtraDomains []string         `json:"cf_extra_domains,omitempty"`
	AuthURL        string           `json:"auth_url,omitempty"`
	AuthFields     []core.CredField `json:"auth_fields"`
	Filters        []*core.Filter   `json:"filters"`
	TagPhrase      bool             `json:"tag_phrase"`
}

// Sites lists full site metadata + its filter specs for the sidebar/panel.
func (a *App) Sites() []*SiteInfo {
	out := []*SiteInfo{}
	for _, s := range core.Sites() {
		authFields := s.AuthFields()
		filters := core.FiltersFor(s)
		out = append(out, &SiteInfo{
			Key: s.Key, Label: s.Label, Domain: s.Domain, Engine: s.Engine,
			Nick: s.Nick(), PageLimit: s.PageLimit,
			RequiresAuth: s.RequiresAuth, OptionalAuth: s.OptionalAuth,
			CfProtected: s.CfProtected, CfExtraDomains: s.CfExtraDomains, AuthURL: s.AuthURL,
			AuthFields: authFields, Filters: filters,
			TagPhrase: core.Registry.EngineFor(s).TagPhrase(),
		})
	}
	return out
}

// FilterDefaults returns each filter's default value keyed by filter key.
func (a *App) FilterDefaults(siteKey string) map[string]any {
	site := core.GetSite(siteKey)
	if site == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for _, f := range core.FiltersFor(site) {
		out[f.Key] = f.DefaultVal()
	}
	return out
}

// BuildQuery assembles the booru-style query from per-filter values.
func (a *App) BuildQuery(siteKey string, values map[string]any) string {
	site := core.GetSite(siteKey)
	if site == nil {
		return ""
	}
	var tokens []string
	for _, f := range core.FiltersFor(site) {
		if f.Kind == core.KindAuthors && site.Engine == "pixiv" {
			for _, author := range strList(values[f.Key]) {
				tokens = append(tokens, "user_id:"+strings.TrimPrefix(author, "user_id:"))
			}
			continue
		}
		if f.Kind == core.KindAuthors && site.Engine == "misskey" {
			tokens = append(tokens, misskeyAuthorTokens(values[f.Key])...)
			continue
		}
		if site.Engine == "twitter" && f.Key == "artists" {
			tokens = append(tokens, twitterAuthorTokens(values[f.Key])...)
			continue
		}
		if site.Engine == "furaffinity" && f.Key == "artists" {
			tokens = append(tokens, faAuthorTokens(values[f.Key])...)
			continue
		}
		if site.Engine == "deviantart" && f.Key == "artists" {
			tokens = append(tokens, daAuthorTokens(values[f.Key])...)
			continue
		}
		if f.EnabledIf != "" && values[f.EnabledIf] != true {
			continue
		}
		tokens = append(tokens, f.Tokens(values[f.Key])...)
	}
	return strings.Join(tokens, " ")
}

func strList(v any) []string {
	switch t := v.(type) {
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}

func misskeyAuthorTokens(v any) []string {
	var out []string
	for _, s := range strList(v) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, "user_id:"+s)
		}
	}
	return out
}

func twitterAuthorTokens(v any) []string {
	var out []string
	for _, s := range strList(v) {
		if s = strings.TrimSpace(strings.TrimLeft(s, "@")); s != "" {
			out = append(out, "from:"+s)
		}
	}
	return out
}

func faAuthorTokens(v any) []string {
	var out []string
	for _, s := range strList(v) {
		s = strings.TrimLeft(strings.TrimSpace(s), "@~")
		if s != "" {
			out = append(out, "by:"+s)
		}
	}
	return out
}

func daAuthorTokens(v any) []string {
	var out []string
	for _, s := range strList(v) {
		if s = strings.TrimLeft(strings.TrimSpace(s), "@"); s != "" {
			out = append(out, "by:"+s)
		}
	}
	return out
}

// Autocomplete fetches tag suggestions (include/exclude fields).
func (a *App) Autocomplete(siteKey, query string) ([]core.TagSuggestion, error) {
	c := a.operationClient()
	return c.AutocompleteTags(context.Background(), siteKey, query)
}

// SearchArtists fetches artist suggestions (authors field).
func (a *App) SearchArtists(siteKey, query string) ([]core.TagSuggestion, error) {
	c := a.operationClient()
	return c.SearchArtists(context.Background(), siteKey, query, 20)
}

// SearchPreview runs one page of a search for the preview grid.
func (a *App) SearchPreview(siteKey, query string, page int) ([]*core.PostInfo, error) {
	return a.operationClient().SearchPosts(context.Background(), siteKey, query, 0, page)
}

// Health probes one site's availability (the header indicator).
func (a *App) Health(siteKey string) bool {
	site := core.GetSite(siteKey)
	if site == nil {
		return false
	}
	return core.Registry.EngineFor(site).CheckAvailable(a.operationClient(), site)
}

// PostURL returns the permalink for a post on a site.
func (a *App) PostURL(siteKey string, id string) string {
	site := core.GetSite(siteKey)
	if site == nil {
		return ""
	}
	return site.PostURL(core.FromIDString(id))
}

// -- settings ------------------------------------------------------------------

// GetAllSettings returns the whole settings dict.
func (a *App) GetAllSettings() map[string]any { return a.settings.Snapshot() }

// RefreshMetadata is an explicit fresh-read boundary, shared by both windows.
func (a *App) NetworkStats() map[string]core.NetworkHostStats { return core.NetworkStats() }
func (a *App) RefreshMetadata() {
	if a.metadata != nil {
		a.metadata.Clear()
	}
}
func (a *App) MetadataStats() core.MetadataStats {
	if a.metadata == nil {
		return core.MetadataStats{}
	}
	return a.metadata.Stats()
}

// GetSetting reads one setting.
func (a *App) GetSetting(key string) any { return a.settings.Get(key) }

// SetSetting writes one setting and flushes.
func (a *App) SetSetting(key string, value any) error {
	if key == "proxy" {
		data, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("некорректные настройки прокси")
		}
		var config core.ProxyConfig
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("некорректные настройки прокси")
		}
		return a.SetProxySettings(config)
	}
	if key == "ui_motion" {
		mode, ok := value.(string)
		if !ok || (mode != "full" && mode != "minimal" && mode != "off") {
			return fmt.Errorf("unknown animation mode: %v", value)
		}
	}
	if key == "log_level" {
		name, ok := value.(string)
		if _, valid := applog.ParseLevel(name); !ok || !valid || name == "" {
			return fmt.Errorf("неизвестный уровень логирования: %v", value)
		}
		value = strings.ToLower(name)
	}
	if key == "thumb_cache_mb" {
		value = max(int64(64), min(int64(65536), core.AsInt(value)))
	}
	if key == "video_cache_mb" {
		value = videocache.LimitMB(core.AsInt(value))
	}
	a.settings.Set(key, value)
	if key == "filters_state" && a.manager != nil {
		a.configureMediaClient(a.manager.Client)
		if a.loader != nil {
			a.configureMediaClient(a.loader.Client)
		}
	}
	if key == "thumb_cache_mb" {
		if a.loader != nil {
			a.loader.SetCap(core.AsInt(value))
		}
		imagecache.Prune(core.AsInt(value)*1048576, "")
	}
	if key == "video_cache_mb" {
		a.videos().SetCap(core.AsInt(value))
	}
	if key == "threads" {
		a.manager.SetThreads(int(core.AsInt(value)))
	}
	if key == "dedup_mode" {
		a.manager.SetDedupMode(core.Str_(value))
	}
	if key == "skip_seen" {
		a.manager.SetSkipSeen(value == true)
	}
	if key == "log_level" {
		_ = applog.SetLevel(core.Str_(value))
	}
	if err := a.settings.Save(); err != nil {
		slog.Error("Настройки: не удалось сохранить", "key", key, "err", err)
		return err
	}
	if key != "credentials" {
		slog.Debug("Настройки: значение сохранено", "key", key)
	}
	if key == "ui_motion" {
		a.emit("ui:motion", value)
	}
	return nil
}

// SaveSettings flushes everything.
func (a *App) SaveSettings() error { return a.settings.Save() }

// LogLevel is the active minimum log level ("debug", "info", "warn", "error").
func (a *App) LogLevel() string { return applog.Level() }

// NetworkLimits lists zones that are paused or slowed after HTTP 429.
func (a *App) NetworkLimits() map[string]core.ZoneLimit { return core.NetworkLimits() }

// SetRateLimits applies throttle overrides live.
func (a *App) SetRateLimits(bySite map[string]float64) {
	core.SetRateLimits(bySite)
	a.settings.Set("rate_limits", bySite)
	_ = a.settings.Save()
}

// SetCredentials replaces all credentials and pushes them into the client.
func (a *App) setCredentials(creds map[string]map[string]string) error {
	if a.metadata != nil {
		a.metadata.Clear()
	}
	if a.loader != nil {
		a.loader.ClearPending()
	}
	a.manager.Client.SetCredentials(creds)
	a.loader.Client.SetCredentials(creds)
	a.settings.Set("credentials", creds)
	err := a.settings.Save()
	a.emit("credentials:changed")
	return err
}

// GetCredentials returns stored credentials (for the auth dialog prefill).
func (a *App) GetCredentials() map[string]map[string]string {
	return a.manager.Client.Credentials()
}

// -- downloads -------------------------------------------------------------------

type StartRequest struct {
	SiteKey    string   `json:"site_key"`
	Query      string   `json:"query"`
	Root       string   `json:"root"`
	MaxPosts   int      `json:"max_posts"`
	SkipSeen   bool     `json:"skip_seen"`
	ByArtist   bool     `json:"by_artist"`
	DedupMode  string   `json:"dedup_mode"` // db | folder | both
	ArtistTags []string `json:"artist_tags"`
	Threads    int      `json:"threads"`
}

// StartRun launches a mass download run.
func (a *App) startRun(req StartRequest) error {
	a.configureMediaClient(a.manager.Client)
	cfg := downloader.Config{
		SiteKey: req.SiteKey, Query: req.Query, Root: req.Root,
		MaxPosts: req.MaxPosts, SkipSeen: req.SkipSeen,
		ByArtist: req.ByArtist, DedupMode: req.DedupMode,
		GroupAlbums: a.albumGrouping(),
		ArtistTags:  req.ArtistTags, Threads: req.Threads,
		ScanFirst: true, RequireConfirmation: a.settings.GetBool("confirm_download_main"),
	}
	cfg.Threads = int(a.settings.GetInt("threads"))
	return a.manager.Start(cfg)
}

// Cancel stops the active run.
func (a *App) Cancel() { a.manager.Cancel() }

func (a *App) ConfirmRun(runID int, accept bool) bool { return a.manager.ConfirmRun(runID, accept) }

// RetryFailed retries only failed files from the current downloads list.
func (a *App) retryFailed() (int, error) {
	a.configureMediaClient(a.manager.Client)
	return a.manager.RetryFailed()
}

// IsRunning reports whether work is in flight.
func (a *App) IsRunning() bool { return a.manager.IsRunning() }

// Summary counts jobs per status.
func (a *App) Summary() map[string]int { return a.manager.Summary() }

// DownloadSelection enqueues specific posts (grid selection / viewer).
func (a *App) downloadSelection(posts []*core.PostInfo, root string,
	byArtist bool, artistTags []string) int {
	a.configureMediaClient(a.manager.Client)
	for _, p := range posts {
		a.manager.Client.ApplyMediaPolicy(p)
	}
	a.manager.SetDedupMode(a.settings.GetString("dedup_mode"))
	a.manager.SetSkipSeen(a.settings.GetBool("skip_seen"))
	count := a.manager.DownloadPosts(posts, root, byArtist, artistTags, a.albumGrouping())
	a.emit("history:changed")
	return count
}

// Resolve local download settings for every selected source, including the feed.
func (a *App) albumGrouping() map[string]bool {
	grouped := map[string]bool{}
	filters, _ := a.settings.Get("filters_state").(map[string]any)
	for key, value := range filters {
		engine := core.Registry.EngineFor(core.GetSite(key))
		values, _ := value.(map[string]any)
		if engine != nil && engine.SupportsAlbums() && values["group_albums"] == true {
			grouped[key] = true
		}
	}
	return grouped
}

// ResetHistory wipes the download history («сбросить историю»).
func (a *App) ResetHistory() error {
	err := a.history.ForgetAll()
	if err == nil {
		a.folderIndex.Invalidate()
		a.emit("history:changed")
	}
	return err
}

// HistoryCount is the number of recorded downloads.
func (a *App) HistoryCount() int { return a.history.Count() }

// -- thumbnails --------------------------------------------------------------------

// Thumb requests a thumbnail; the result arrives via the thumbs:loaded event.
func (a *App) Thumb(url string) {
	a.loader.Request(url)
}

// CacheStats reports (bytes, count) of the thumb cache and video tmp dir.
func (a *App) CacheStats() map[string]int64 {
	b, n := imagecache.Size("")
	vt, vn, loading := a.videos().Stats()
	return map[string]int64{"thumb_bytes": b, "thumb_count": int64(n),
		"video_tmp_bytes": vt, "video_tmp_count": int64(vn), "video_loading": int64(loading),
		"video_limit_mb": videocache.LimitMB(a.settings.GetInt("video_cache_mb")),
		"limit_mb":       max(64, a.settings.GetInt("thumb_cache_mb"))}
}

// ClearThumbs deletes the thumbnail cache.
func (a *App) ClearThumbs() int64 {
	if a.loader != nil {
		return a.loader.ClearCache()
	}
	return imagecache.Clear("")
}

// ClearVideoTmp deletes temp video files.
func (a *App) ClearVideoTmp() int64 {
	return a.videos().Clear()
}

// PruneThumbs trims the cache to its cap right now.
func (a *App) PruneThumbs(maxMB int64) int64 {
	return imagecache.Prune(max(64, min(65536, maxMB))*1024*1024, "")
}

// -- favourites ---------------------------------------------------------------------

type FavPostIn struct {
	ID   string         `json:"id"`
	Site string         `json:"site"`
	Data map[string]any `json:"data"`
}

// FavToggle flips a post's favourite state; returns the new state.
func (a *App) FavToggle(post *core.PostInfo) bool {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	on := a.favorites.TogglePost(post)
	a.emit("favorites:changed")
	return on
}

// FavIs reports whether a post is starred.
func (a *App) FavIs(site, id string) bool {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	return a.favorites.IsPost(&core.PostInfo{ID: core.FromIDString(id), Site: site})
}

// FavPosts lists starred posts newest-first.
func (a *App) FavPosts() []*core.PostInfo {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	return a.favorites.Posts()
}

// FavRemovePost unstaries by key.
func (a *App) FavRemovePost(site, id string) bool {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	removed := a.favorites.RemovePost(&core.PostInfo{
		ID: core.FromIDString(id), Site: site})
	a.emit("favorites:changed")
	return removed
}

// AuthorToggle flips an author's favourite state.
func (a *App) AuthorToggle(name, query, site string) bool {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	on := a.favorites.ToggleAuthor(name, query, site)
	a.emit("favorites:changed")
	return on
}

// Authors lists starred authors with unread counts.
func (a *App) Authors() []map[string]any {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	return a.favorites.Authors()
}

// AuthorRemove unstaries an author.
func (a *App) AuthorRemove(site, query string) bool {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	removed := a.favorites.RemoveAuthor(site, query)
	a.emit("favorites:changed")
	return removed
}

// CheckNewPosts scans an author's feed and returns the unread count.
func (a *App) CheckNewPosts(siteKey, query string, maxPosts int) (int, error) {
	return a.checkNewPosts(context.Background(), siteKey, query, maxPosts)
}
func (a *App) checkNewPosts(ctx context.Context, siteKey, query string, maxPosts int) (int, error) {
	ctx = core.WithRequestPriority(ctx, core.PriorityBackground)
	site := core.GetSite(siteKey)
	if site == nil || query == "" {
		return 0, fmt.Errorf("bad author check request")
	}
	a.dataMu.Lock()
	state := a.favorites.AuthorSubscription(siteKey, query)
	a.dataMu.Unlock()
	c := a.operationClient()
	c.SetContext(ctx)
	identity := c.AuthIdentity()
	c.FreshMetadata = true // subscription counts always reconcile live metadata
	c.MaxRetries = 1       // a 429 pauses the whole zone; other authors must not retry into it
	started := time.Now()
	slog.Debug("Подписки: проверка автора", "site", siteKey, "query", query, "initialized", state.Initialized)
	scan, err := scanAuthor(c, site, query, maxPosts, state, identity)
	if err != nil {
		if !errors.Is(err, core.DownloadCancelled) {
			slog.Warn("Подписки: проверка автора не удалась", "site", siteKey, "query", query, "err", err)
		}
		return 0, err
	}
	slog.Debug("Подписки: автор проверен", "site", siteKey, "query", query, "ids", len(scan.ids),
		"complete", scan.complete, "full", scan.full, "elapsed_ms", time.Since(started).Milliseconds())
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	if ctx.Err() != nil {
		return 0, core.DownloadCancelled
	}
	unread, ok, err := a.favorites.RecordAuthorCheck(siteKey, query, scan.ids, scan.complete, scan.full, identity)
	if err != nil {
		return 0, err
	}
	if !ok {
		// author was unstarred mid-flight; treat as zero but not an error
		return 0, nil
	}
	a.emit("authors:changed")
	return unread, nil
}

// AuthorBatchResult: Unread holds authors settled by one OR query (no new
// posts since their checkpoint); Pending still need CheckNewPosts.
type AuthorBatchResult struct {
	Unread  map[string]int `json:"unread"`
	Pending []string       `json:"pending"`
}

// CheckAuthorsBatch settles subscription checks for many authors of one site
// with one request per batch where the engine supports OR queries; it never
// records new posts itself — any doubt sends the author to CheckNewPosts.
func (a *App) CheckAuthorsBatch(siteKey string, queries []string) (AuthorBatchResult, error) {
	return a.checkAuthorsBatch(context.Background(), siteKey, queries)
}
func (a *App) checkAuthorsBatch(ctx context.Context, siteKey string, queries []string) (AuthorBatchResult, error) {
	started := time.Now()
	result := AuthorBatchResult{Unread: map[string]int{}, Pending: []string{}}
	site := core.GetSite(siteKey)
	if site == nil {
		return result, fmt.Errorf("unknown site %q", siteKey)
	}
	e := core.Registry.EngineFor(site)
	batcher, ok := e.(batchAuthorEngine)
	ordered, ordOK := e.(orderedAuthorEngine)
	if !ok || !ordOK || batcher.AuthorBatchSize() < 2 {
		result.Pending = append(result.Pending, queries...)
		slog.Debug("Подписки: пакетная проверка недоступна для сайта", "site", siteKey, "authors", len(queries))
		return result, nil
	}
	ctx = core.WithRequestPriority(ctx, core.PriorityBackground)
	c := a.operationClient()
	c.SetContext(ctx)
	c.FreshMetadata = true
	c.MaxRetries = 1 // a 429 pauses the zone; the caller stops this host
	identity := c.AuthIdentity()
	var items []authorProbeItem
	seen := map[string]bool{}
	for _, query := range queries {
		if seen[query] {
			continue
		}
		seen[query] = true
		tag := authorProbeTag(query)
		a.dataMu.Lock()
		state := a.favorites.AuthorSubscription(siteKey, query)
		a.dataMu.Unlock()
		checked := checkpointIDs(state)
		maxID := maxNumericID(checked)
		clean, _ := core.SplitHideText(query)
		clean, _ = core.SplitImagesOnly(clean)
		if tag == "" || !state.Initialized || state.ScanIdentity != identity || maxID <= 0 || !ordered.AuthorScanDescending(clean) {
			result.Pending = append(result.Pending, query)
			continue
		}
		items = append(items, authorProbeItem{query: query, tag: tag, maxID: maxID, checked: checked})
	}
	size := batcher.AuthorBatchSize()
	requests := 0
	for start := 0; start < len(items); start += size {
		if ctx.Err() != nil {
			return result, core.DownloadCancelled
		}
		batch := items[start:min(start+size, len(items))]
		requests++
		clean, err := probeAuthorBatch(c, e, site, batch)
		if err != nil {
			if ctx.Err() != nil {
				return result, core.DownloadCancelled
			}
			slog.Warn("Подписки: пакетная проверка не удалась", "site", siteKey, "authors", len(batch), "err", err)
			if core.IsRateLimited(err) {
				for _, it := range items[start:] {
					result.Pending = append(result.Pending, it.query)
				}
				return result, err
			}
			for _, it := range batch {
				result.Pending = append(result.Pending, it.query)
			}
			continue
		}
		for _, it := range batch {
			if !clean[it.query] {
				result.Pending = append(result.Pending, it.query)
				continue
			}
			a.dataMu.Lock()
			unread, ok, err := a.favorites.RecordAuthorCheck(siteKey, it.query, it.checked, true, false, identity)
			a.dataMu.Unlock()
			if err != nil || !ok {
				result.Pending = append(result.Pending, it.query)
				continue
			}
			result.Unread[it.query] = unread
		}
	}
	if len(result.Unread) > 0 {
		a.emit("authors:changed")
	}
	slog.Info("Подписки: пакетная проверка", "site", siteKey, "authors", len(queries), "batched", len(items),
		"settled", len(result.Unread), "pending", len(result.Pending), "requests", requests,
		"elapsed_ms", time.Since(started).Milliseconds())
	return result, nil
}

func packIDAny(id core.PostId) any {
	if id.IsNu {
		return id.Num
	}
	return id.Str
}

// MarkAuthorViewed clears unread state after opening the author's works.
func (a *App) MarkAuthorViewed(site, query string, postIDs []any) bool {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	return a.favorites.MarkAuthorViewed(site, query, postIDs)
}

// -- reverse search ---------------------------------------------------------------

type ProviderInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Blurb string `json:"blurb"`
	OK    bool   `json:"ok"`
	Why   string `json:"why"`
}

// ReverseProviders lists providers with their availability.
func (a *App) ReverseProviders() []ProviderInfo {
	getSetting := func(k string) string {
		switch v := a.settings.Get(k).(type) {
		case string:
			return v
		case float64:
			return fmt.Sprint(int64(v))
		}
		return ""
	}
	out := []ProviderInfo{}
	for _, p := range reverse.Providers() {
		ok, why := p.Available(getSetting)
		out = append(out, ProviderInfo{p.Key(), p.Label(), p.Blurb(), ok, why})
	}
	return out
}

// ReverseSearch runs one provider against raw image bytes.
func (a *App) reverseSearch(providerKey, filename string, image []byte) (
	[]reverse.Match, error) {

	var prov reverse.Provider
	for _, p := range reverse.Providers() {
		if p.Key() == providerKey {
			prov = p
			break
		}
	}
	if prov == nil {
		return nil, fmt.Errorf("unknown provider %q", providerKey)
	}
	getSetting := func(k string) string {
		switch v := a.settings.Get(k).(type) {
		case string:
			return v
		case float64:
			return fmt.Sprint(int64(v))
		}
		return ""
	}
	return prov.Search(a.operationClient(), image, filename, getSetting, nil)
}

// ResolveHit maps a reverse-search URL to (site_key, post_id).
func (a *App) ResolveHit(rawURL string) (string, string) {
	key, pid := reverse.FindSitePost(rawURL)
	return key, pid.String()
}

// PostByID loads one post fully (reverse-search hit → PostInfo).
func (a *App) postByID(siteKey, id string) (*core.PostInfo, error) {
	site := core.GetSite(siteKey)
	if site == nil {
		return nil, fmt.Errorf("unknown site %q", siteKey)
	}
	engine := core.Registry.EngineFor(site)
	if engine == nil {
		return nil, fmt.Errorf("no engine for %q", siteKey)
	}
	return engine.PostByID(a.operationClient(), site, core.FromIDString(id))
}

// PrepareView resolves a post's media for the viewer (FA/DA lazy pages);
// true when URLs changed so the frontend reloads the image.
func (a *App) PrepareView(post *core.PostInfo) bool {
	site := core.GetSite(post.Site)
	if site == nil {
		return false
	}
	engine := core.Registry.EngineFor(site)
	if engine == nil {
		return false
	}
	changed, _ := engine.PrepareView(a.operationClient(), post)
	return changed
}

// ExpandPost splits albums into per-file posts (download path).
func (a *App) ExpandPost(post *core.PostInfo) []*core.PostInfo {
	return a.expandPost(context.Background(), post)
}
func (a *App) runExpandPost(ctx context.Context, post *core.PostInfo) []*core.PostInfo {
	site := core.GetSite(post.Site)
	if site == nil {
		return nil
	}
	engine := core.Registry.EngineFor(site)
	if engine == nil {
		return nil
	}
	c := a.operationClient()
	c.SetContext(ctx)
	return engine.ExpandPost(c, post.Clone())
}

// -- shell helpers -----------------------------------------------------------------

// PickDirectory shows the native folder picker and returns the chosen path.
func (a *App) PickDirectory() (string, error) {
	return wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "Выберите папку загрузки",
	})
}

// RevealDownloadRoot opens the configured download root in Explorer.
func (a *App) RevealDownloadRoot() error {
	root := a.settings.GetString("download_root")
	if root == "" {
		return nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	return exec.Command("explorer", abs).Start()
}

var _ = history.History{}
