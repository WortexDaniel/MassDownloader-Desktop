// Package favorites — starred posts and authors (SQLite app.db) with
// author-subscription unread tracking. Port of app/favorites.py.
//
// Two independent collections:
//
//   - Posts: a full serialized PostInfo per favourite, so the «Избранное» tab
//     can show the thumbnail and reopen the large viewer without a network
//     round-trip. Keyed by (site, id).
//   - Authors: (name, query, site) triples so a starred author reopens the
//     «работы автора» browser exactly as the grid/viewer would. Keyed by
//     (site, query).
//
// In-memory maps are a read cache; every mutation also writes its row eagerly
// so a crash never loses a star.
package favorites

import (
	"encoding/json"
	"sort"
	"time"

	"massdownloader/internal/core"
	"massdownloader/internal/store"
)

const authorKnownLimit = 2000

// AuthorState is the normalised, JSON-safe subscription state. KnownIDs are
// recent posts already accounted for; UnreadIDs are newly discovered posts
// not yet viewed. ViewedIDs preserves explicit acknowledgements independently
// of the bounded baseline; CheckedIDs is the network scan's stopping point.
// Opaque social-network IDs are preserved as strings.
type AuthorState struct {
	Initialized   bool   `json:"initialized"`
	KnownIDs      []any  `json:"known_ids"`
	UnreadIDs     []any  `json:"unread_ids"`
	ViewedIDs     []any  `json:"viewed_ids,omitempty"`
	CheckedIDs    []any  `json:"checked_ids,omitempty"`
	Overflow      bool   `json:"overflow"`
	CheckedAt     int64  `json:"checked_at"`
	FullCheckedAt int64  `json:"full_checked_at,omitempty"`
	ScanIdentity  string `json:"scan_identity,omitempty"`
}

func newAuthorState(value any) AuthorState {
	st := AuthorState{KnownIDs: []any{}, UnreadIDs: []any{}}
	raw, _ := value.(map[string]any)
	if raw == nil {
		return st
	}
	if v, ok := raw["initialized"].(bool); ok {
		st.Initialized = v
	}
	if v, ok := raw["overflow"].(bool); ok {
		st.Overflow = v
	}
	st.CheckedAt = core.AsInt(raw["checked_at"])
	st.FullCheckedAt = core.AsInt(raw["full_checked_at"])
	st.ScanIdentity, _ = raw["scan_identity"].(string)
	appendIDs := func(src any, dst *[]any) {
		list, ok := src.([]any)
		if !ok {
			return
		}
		type marker struct {
			isNum bool
			s     string
		}
		seen := map[marker]bool{}
		for _, item := range list {
			id := normalizeID(item)
			m := marker{id.IsNu, id.String()}
			if id.String() == "" || seen[m] {
				continue
			}
			seen[m] = true
			*dst = append(*dst, packID(id))
		}
	}
	appendIDs(raw["known_ids"], &st.KnownIDs)
	appendIDs(raw["unread_ids"], &st.UnreadIDs)
	appendIDs(raw["viewed_ids"], &st.ViewedIDs)
	appendIDs(raw["checked_ids"], &st.CheckedIDs)
	if len(st.KnownIDs) > authorKnownLimit {
		st.KnownIDs = st.KnownIDs[:authorKnownLimit]
	}
	known := map[string]bool{}
	for _, item := range append(append([]any{}, st.KnownIDs...), st.ViewedIDs...) {
		id := normalizeID(item)
		known[id.Key()] = true
	}
	filtered := st.UnreadIDs[:0]
	for _, item := range st.UnreadIDs {
		id := normalizeID(item)
		if !known[id.Key()] {
			filtered = append(filtered, item)
		}
	}
	st.UnreadIDs = filtered
	return st
}

func normalizeID(v any) core.PostId { return core.NormalizePostId(v) }

// packID renders an id as JSON-safe value: number for numeric ids (matching
// Python's int|str persistence), string otherwise.
func packID(id core.PostId) any {
	if id.IsNu {
		return id.Num
	}
	return id.Str
}

// PostToDict flattens a PostInfo (+ its PostFile) into JSON-safe primitives,
// mirroring favorites.post_to_dict so both apps read each other's rows.
func PostToDict(p *core.PostInfo) map[string]any {
	d := map[string]any{
		"id":          packID(p.ID),
		"site":        p.Site,
		"file":        nil,
		"preview_url": p.PreviewURL,
		"sample_url":  p.SampleURL,
		"score":       p.Score,
		"fav_count":   p.FavCount,
		"rating":      p.Rating,
		"artists":     toAnyList(p.Artists),
		"tags":        toAnyList(p.Tags),
		"author_id":   p.AuthorID,
		"created_at":  p.CreatedAt,
		"page_count":  p.PageCount,
		"page_index":  p.PageIndex,
		// opaque engine bag — JSON-safe, so a re-opened favourite still
		// expands albums and builds its permalink
		"extra": toAnyMap(p.Extra),
	}
	if p.File != nil {
		d["file"] = map[string]any{
			"url": p.File.URL, "md5": p.File.MD5, "ext": p.File.Ext,
			"size": p.File.Size, "width": p.File.Width, "height": p.File.Height,
		}
	}
	return d
}

// PostFromDict rebuilds a PostInfo from PostToDict output (tolerant of
// missing keys so an older saved file still loads).
func PostFromDict(d map[string]any) *core.PostInfo {
	p := &core.PostInfo{
		ID:         core.NormalizePostId(d["id"]),
		Site:       strOr(d["site"]),
		PreviewURL: strOr(d["preview_url"]),
		SampleURL:  strOr(d["sample_url"]),
		Score:      intOr(d["score"]),
		FavCount:   intOr(d["fav_count"]),
		Rating:     strDflt(d["rating"], "?"),
		Artists:    strSlice(d["artists"]),
		Tags:       strSlice(d["tags"]),
		AuthorID:   strOr(d["author_id"]),
		CreatedAt:  intOr(d["created_at"]),
		PageCount:  int(intDflt(d["page_count"], 1)),
		PageIndex:  int(intOr(d["page_index"])),
		Extra:      mapAny(d["extra"]),
	}
	if fd, ok := d["file"].(map[string]any); ok && fd != nil {
		p.File = &core.PostFile{
			URL:    strOr(fd["url"]),
			MD5:    strOr(fd["md5"]),
			Ext:    strOr(fd["ext"]),
			Size:   intOr(fd["size"]),
			Width:  int(intOr(fd["width"])),
			Height: int(intOr(fd["height"])),
		}
	}
	return p
}

func toAnyList(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func toAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func strOr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	}
	return ""
}

func strDflt(v any, d string) string {
	if s := strOr(v); s != "" {
		return s
	}
	return d
}

func intOr(v any) int64 { return core.AsInt(v) }

func intDflt(v any, d int64) int64 {
	if _, ok := v.(json.Number); ok {
		return core.AsInt(v)
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	}
	return d
}

func strSlice(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, strOr(x))
	}
	return out
}

func mapAny(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

type postKey struct {
	Site string
	ID   string
}

type authorRow struct {
	Name    string      `json:"name"`
	Query   string      `json:"query"`
	Site    string      `json:"site"`
	SavedAt int64       `json:"saved_at"`
	State   AuthorState `json:"-"`
}

// Favorites is the shared-store favourites collection.
type Favorites struct {
	st      *store.Store
	posts   map[postKey]map[string]any
	authors map[postKey]*authorRow // key: site\x00query
}

// New opens/loads from the store at path.
func New(path string) (*Favorites, error) {
	st, err := store.Get(path)
	if err != nil {
		return nil, err
	}
	f := &Favorites{
		st:      st,
		posts:   map[postKey]map[string]any{},
		authors: map[postKey]*authorRow{},
	}
	if err := f.load(); err != nil {
		return nil, err
	}
	return f, nil
}

func pkey(site string, id core.PostId) postKey {
	return postKey{Site: site, ID: id.Key()}
}

func akey(site, query string) postKey {
	return postKey{Site: site, ID: query}
}

// IsPost reports whether post is starred.
func (f *Favorites) IsPost(post *core.PostInfo) bool {
	return f.posts[pkey(post.Site, post.ID)] != nil
}

// AddPost stars a post; false when already present.
func (f *Favorites) AddPost(post *core.PostInfo) bool {
	key := pkey(post.Site, post.ID)
	if _, exists := f.posts[key]; exists {
		return false
	}
	d := PostToDict(post)
	savedAt := time.Now().Unix()
	d["saved_at"] = savedAt
	f.posts[key] = d
	conn, release := f.st.Conn()
	defer release()
	enc, _ := json.Marshal(d)
	_, err := conn.Exec(
		`INSERT OR REPLACE INTO fav_posts (site, post_id, saved_at, data)
		 VALUES (?, ?, ?, ?)`,
		post.Site, post.ID.Key(), savedAt, string(enc))
	return err == nil
}

// RemovePost unstars a post; false when it wasn't starred.
func (f *Favorites) RemovePost(post *core.PostInfo) bool {
	key := pkey(post.Site, post.ID)
	if _, exists := f.posts[key]; !exists {
		return false
	}
	delete(f.posts, key)
	conn, release := f.st.Conn()
	defer release()
	conn.Exec(`DELETE FROM fav_posts WHERE site = ? AND post_id = ?`,
		post.Site, post.ID.Key())
	return true
}

// TogglePost flips favourite state, returning the new state.
func (f *Favorites) TogglePost(post *core.PostInfo) bool {
	if f.IsPost(post) {
		f.RemovePost(post)
		return false
	}
	f.AddPost(post)
	return true
}

// Posts lists all favourite posts, most-recently-starred first.
func (f *Favorites) Posts() []*core.PostInfo {
	type row struct {
		d  map[string]any
		at int64
	}
	rows := make([]row, 0, len(f.posts))
	for _, d := range f.posts {
		rows = append(rows, row{d, intOr(d["saved_at"])})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at > rows[j].at })
	out := make([]*core.PostInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, PostFromDict(r.d))
	}
	return out
}

// PostCount is the number of starred posts.
func (f *Favorites) PostCount() int { return len(f.posts) }

// IsAuthor reports whether (site, query) is starred.
func (f *Favorites) IsAuthor(site, query string) bool {
	return f.authors[akey(site, query)] != nil
}

// AddAuthor stars an author; false on empty query or duplicate.
func (f *Favorites) AddAuthor(name, query, site string) bool {
	key := akey(site, query)
	if query == "" {
		return false
	}
	if _, exists := f.authors[key]; exists {
		return false
	}
	row := &authorRow{Name: name, Query: query, Site: site,
		SavedAt: time.Now().Unix(), State: newAuthorState(nil)}
	f.authors[key] = row
	conn, release := f.st.Conn()
	defer release()
	enc, _ := json.Marshal(row.State)
	_, err := conn.Exec(
		`INSERT OR REPLACE INTO fav_authors
		 (site, query, name, saved_at, state) VALUES (?, ?, ?, ?, ?)`,
		site, query, name, row.SavedAt, string(enc))
	return err == nil
}

// RemoveAuthor unstaries an author.
func (f *Favorites) RemoveAuthor(site, query string) bool {
	key := akey(site, query)
	if _, exists := f.authors[key]; !exists {
		return false
	}
	delete(f.authors, key)
	conn, release := f.st.Conn()
	defer release()
	conn.Exec(`DELETE FROM fav_authors WHERE site = ? AND query = ?`,
		site, query)
	return true
}

// ToggleAuthor flips author state, returning the new state.
func (f *Favorites) ToggleAuthor(name, query, site string) bool {
	if f.IsAuthor(site, query) {
		f.RemoveAuthor(site, query)
		return false
	}
	return f.AddAuthor(name, query, site)
}

// Authors lists all starred authors, most-recently-starred first. Each item
// carries name/query/site/saved_at keys like the Python dict.
func (f *Favorites) Authors() []map[string]any {
	rows := make([]*authorRow, 0, len(f.authors))
	for _, r := range f.authors {
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].SavedAt > rows[j].SavedAt
	})
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"name": r.Name, "query": r.Query, "site": r.Site,
			"saved_at": r.SavedAt,
			"unread":   len(r.State.UnreadIDs),
			"overflow": r.State.Overflow,
		})
	}
	return out
}

// AuthorCount is the number of starred authors.
func (f *Favorites) AuthorCount() int { return len(f.authors) }

// -- subscriptions -----------------------------------------------------------

// AuthorSubscription snapshots one favourite author's unread-tracking state.
func (f *Favorites) AuthorSubscription(site, query string) AuthorState {
	row := f.authors[akey(site, query)]
	if row == nil {
		return newAuthorState(nil)
	}
	return newAuthorState(stateToAny(row.State))
}

func stateToAny(s AuthorState) any {
	b, _ := json.Marshal(map[string]any{
		"initialized":     s.Initialized,
		"known_ids":       s.KnownIDs,
		"unread_ids":      s.UnreadIDs,
		"viewed_ids":      s.ViewedIDs,
		"checked_ids":     s.CheckedIDs,
		"overflow":        s.Overflow,
		"checked_at":      s.CheckedAt,
		"full_checked_at": s.FullCheckedAt,
		"scan_identity":   s.ScanIdentity,
	})
	var out any
	core.DecodeJSON(b, &out)
	return out
}

// AuthorKnownIDs merges known+unread into one exclusion set.
func (f *Favorites) AuthorKnownIDs(site, query string) map[string]bool {
	state := f.AuthorSubscription(site, query)
	out := map[string]bool{}
	for _, item := range append(append(append([]any{}, state.KnownIDs...), state.UnreadIDs...), state.ViewedIDs...) {
		out[normalizeID(item).Key()] = true
	}
	return out
}

// RecordAuthorScan merges a background author-feed scan and returns its
// unread count. The first successful scan establishes the baseline and
// therefore reports zero. Later scans append only genuinely unknown IDs to
// the unread set. ok=false means the author was removed mid-flight.
func (f *Favorites) RecordAuthorScan(site, query string, postIDs []any,
	complete bool) (unread int, ok bool) {
	return f.recordAuthorScan(site, query, postIDs, complete, true)
}
func (f *Favorites) recordAuthorScan(site, query string, postIDs []any, complete, persist bool) (unread int, ok bool) {

	key := akey(site, query)
	row := f.authors[key]
	if row == nil {
		return 0, false
	}
	state := newAuthorState(stateToAny(row.State))
	type marker struct {
		isNum bool
		s     string
	}
	var ids []any
	seen := map[marker]bool{}
	for _, item := range postIDs {
		id := normalizeID(item)
		m := marker{id.IsNu, id.String()}
		if id.String() == "" || seen[m] {
			continue
		}
		seen[m] = true
		ids = append(ids, packID(id))
	}
	if !state.Initialized {
		if len(ids) > authorKnownLimit {
			ids = ids[:authorKnownLimit]
		}
		// Media viewed before the first check remains acknowledged too.
		ids = append(ids, state.KnownIDs...)
		state.KnownIDs = newAuthorState(map[string]any{"known_ids": ids}).KnownIDs
		state.UnreadIDs = []any{}
		state.Initialized = true
		state.Overflow = !complete
	} else {
		known := map[string]bool{}
		for _, item := range append(append(append([]any{}, state.KnownIDs...), state.UnreadIDs...), state.ViewedIDs...) {
			known[normalizeID(item).Key()] = true
		}
		var fresh []any
		for _, item := range ids {
			if !known[normalizeID(item).Key()] {
				fresh = append(fresh, item)
			}
		}
		state.UnreadIDs = append(fresh, state.UnreadIDs...)
		state.Overflow = state.Overflow || !complete
	}
	state.CheckedAt = time.Now().Unix()
	row.State = state
	if persist {
		f.writeAuthorState(site, query, state)
	}
	return len(state.UnreadIDs), true
}

// RecordAuthorCheck persists the scan marker only after a successful state write.
// A failed/cancelled network scan never calls this method.
func (f *Favorites) RecordAuthorCheck(site, query string, ids []any, complete, full bool, identity string) (int, bool, error) {
	row := f.authors[akey(site, query)]
	if row == nil {
		return 0, false, nil
	}
	previous := row.State
	unread, ok := f.recordAuthorScan(site, query, ids, complete, false)
	state := row.State
	if complete {
		// Only a successful network scan advances its stopping point. An
		// individual view must never hide unseen posts below the viewed ID.
		state.CheckedIDs = newAuthorState(map[string]any{"known_ids": ids}).KnownIDs
	}
	if full && complete {
		state.FullCheckedAt = time.Now().Unix()
		state.Overflow = false
	}
	state.ScanIdentity = identity
	if err := f.writeAuthorState(site, query, state); err != nil {
		row.State = previous
		return 0, false, err
	}
	row.State = state
	return unread, ok, nil
}

// MarkAuthorViewed clears unread posts and accounts for displayed ones.
func (f *Favorites) MarkAuthorViewed(site, query string, postIDs []any) bool {
	changed, _ := f.markAuthorViewed(site, query, postIDs)
	return changed
}

func (f *Favorites) ClearAuthorUnread(site, query string) (bool, error) {
	return f.markAuthorViewed(site, query, nil)
}

func (f *Favorites) markAuthorViewed(site, query string, postIDs []any) (bool, error) {
	key := akey(site, query)
	row := f.authors[key]
	if row == nil {
		return false, nil
	}
	state := newAuthorState(stateToAny(row.State))
	before, _ := json.Marshal(state)
	combined := append(append([]any{}, state.KnownIDs...), state.UnreadIDs...)
	combined = append(combined, postIDs...)
	type marker struct {
		isNum bool
		s     string
	}
	var deduped []any
	seen := map[marker]bool{}
	for _, item := range combined {
		id := normalizeID(item)
		m := marker{id.IsNu, id.String()}
		if id.String() == "" || seen[m] {
			continue
		}
		seen[m] = true
		deduped = append(deduped, packID(id))
	}
	if len(deduped) > authorKnownLimit {
		// Preserve the scan baseline. Later acknowledgements already survive
		// independently in ViewedIDs and must not evict historical IDs.
		deduped = deduped[:authorKnownLimit]
	}
	state.Initialized = true
	state.KnownIDs = deduped
	// Explicit acknowledgements survive the bounded scan baseline.
	state.ViewedIDs = append(state.ViewedIDs, state.UnreadIDs...)
	state.ViewedIDs = append(state.ViewedIDs, postIDs...)
	state.ViewedIDs = newAuthorState(map[string]any{"viewed_ids": state.ViewedIDs}).ViewedIDs
	state.UnreadIDs = []any{}
	state.Overflow = false
	after, _ := json.Marshal(state)
	changed := string(before) != string(after)
	if changed {
		if err := f.writeAuthorState(site, query, state); err != nil {
			return false, err
		}
	}
	row.State = state
	return changed, nil
}

func (f *Favorites) writeAuthorState(site, query string, state AuthorState) error {
	conn, release := f.st.Conn()
	defer release()
	enc, _ := json.Marshal(state)
	_, err := conn.Exec(`UPDATE fav_authors SET state = ? WHERE site = ? AND query = ?`,
		string(enc), site, query)
	return err
}

func (f *Favorites) load() error {
	conn, release := f.st.Conn()
	defer release()
	prows, err := conn.Query(`SELECT site, post_id, saved_at, data FROM fav_posts`)
	if err != nil {
		return err
	}
	for prows.Next() {
		var site, data string
		var pid any
		var savedAt sqlInt
		if err := prows.Scan(&site, &pid, &savedAt, &data); err != nil {
			continue
		}
		var d map[string]any
		if err := core.DecodeJSON([]byte(data), &d); err != nil || d == nil {
			continue
		}
		if _, ok := d["saved_at"]; !ok {
			d["saved_at"] = int64(savedAt)
		}
		f.posts[pkey(site, core.NormalizePostId(pid))] = d
	}
	prows.Close()

	arows, err := conn.Query(
		`SELECT site, query, name, saved_at, state FROM fav_authors`)
	if err != nil {
		return err
	}
	defer arows.Close()
	for arows.Next() {
		var site, query, name, stateJSON string
		var savedAt sqlInt
		if err := arows.Scan(&site, &query, &name, &savedAt, &stateJSON); err != nil {
			continue
		}
		var raw any
		core.DecodeJSON([]byte(orEmpty(stateJSON)), &raw)
		f.authors[akey(site, query)] = &authorRow{
			Name: name, Query: query, Site: site,
			SavedAt: int64(savedAt), State: newAuthorState(raw),
		}
	}
	return nil
}

func orEmpty(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

// Save rewrites both tables from in-memory state — mutations already persist
// eagerly, this is the belt-and-suspenders full flush after bulk edits.
func (f *Favorites) Save() error {
	conn, release := f.st.Conn()
	defer release()
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM fav_posts`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM fav_authors`); err != nil {
		tx.Rollback()
		return err
	}
	for key, d := range f.posts {
		enc, _ := json.Marshal(d)
		if _, err := tx.Exec(
			`INSERT INTO fav_posts (site, post_id, saved_at, data)
			 VALUES (?, ?, ?, ?)`,
			key.Site, key.ID, intOr(d["saved_at"]), string(enc)); err != nil {
			tx.Rollback()
			return err
		}
	}
	for key, r := range f.authors {
		enc, _ := json.Marshal(r.State)
		if _, err := tx.Exec(
			`INSERT INTO fav_authors (site, query, name, saved_at, state)
			 VALUES (?, ?, ?, ?, ?)`,
			key.Site, key.ID, r.Name, r.SavedAt, string(enc)); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
