// Package core — shared data models, constants and small helpers used by
// every engine. Port of app/core/models.py.
package core

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PostId is the canonical post identifier: numeric booru IDs (int64) or
// opaque string platform IDs (Misskey notes). The zero value means "unset".
type PostId struct {
	Str  string
	Num  int64
	IsNu bool // true -> use Num
}

// IDs cross the WebView boundary as strings to preserve 64-bit social IDs.
// Accept Python's scalar IDs and the object shape used by early Go builds.
func (p PostId) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }
func (p *PostId) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*p = FromIDString(s)
		return nil
	}
	if string(data) == "null" {
		*p = S("")
		return nil
	}
	if len(data) > 0 && data[0] == '{' {
		type legacy PostId
		var v legacy
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*p = PostId(v)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	i, err := n.Int64()
	if err != nil {
		return err
	}
	*p = I(i)
	return nil
}

// I makes an int-backed PostId.
func I(n int64) PostId { return PostId{Num: n, IsNu: true} }

// S makes a string-backed PostId.
func S(s string) PostId { return PostId{Str: s} }

// String renders the id for queries, file names and logs.
func (p PostId) String() string {
	if p.IsNu {
		return strconv.FormatInt(p.Num, 10)
	}
	return p.Str
}

// Int returns the numeric value when the id is numeric, else 0.
func (p PostId) Int() int64 {
	if p.IsNu {
		return p.Num
	}
	return 0
}

// Key is a stable map/DB key for the id.
func (p PostId) Key() string { return p.String() }

// Less orders ids: numerics by value, strings lexicographically; numerics
// sort before strings (matches normalize_post_id semantics of Python).
func (p PostId) Less(q PostId) bool {
	if p.IsNu && q.IsNu {
		return p.Num < q.Num
	}
	if p.IsNu != q.IsNu {
		return p.IsNu
	}
	return p.Str < q.Str
}

const (
	MAX_PAGE_LIMIT = 320
)

var IMAGE_EXTS = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true, "avif": true,
}

var ANIM_EXTS = map[string]bool{
	"webm": true, "mp4": true, "swf": true,
}

// IMAGES_ONLY_TOKEN is the flag token emitted by the per-site «Только
// изображения» BoolFilter. The client strips it from the query and drops
// video/animated posts from the results — see ApiClient.SearchPosts / IterPosts.
const IMAGES_ONLY_TOKEN = "type:image"

// CredField is one optional credential field offered by the auth dialog.
type CredField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// CF_FIELDS are optional credential fields offered for cf_protected sites.
// They live in the same per-site credentials dict as API keys; the client
// replays them as a "Cookie: cf_clearance=…" + User-Agent override on requests
// to that site's host, getting past Cloudflare's "Just a moment…" challenge.
// Both are pasted from a browser that has already solved it — the User-Agent
// must be the exact one that obtained the cookie (Cloudflare binds the
// clearance token to the UA).
var CF_FIELDS = []CredField{
	{"cf_clearance", "cf_clearance (cookie)"},
	{"user_agent", "User-Agent браузера"},
}

// Site is a bag of metadata + capability flags for one booru.
//
// Engine names the protocol module that knows how to talk to it. The
// capability flags drive the UI directly: a site with HasFaves=false greys
// out the faves filter, RatingWords switches the rating token to
// rating:explicit form, RequiresAuth triggers the credential prompt.
// Adding a site never touches the UI — it only sets these flags.
type Site struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Domain string `json:"domain"`
	// APIDomain separates JSON endpoints from the public website when needed.
	APIDomain string `json:"api_domain,omitempty"`

	StaticDomain string `json:"static_domain,omitempty"`
	Engine       string `json:"engine"`     // which engine handles this site
	PageLimit    int    `json:"page_limit"` // max posts per API page

	// MinInterval is the minimum seconds between API hits to this site's host
	// (0 -> use the client default). The throttle is per-host, so each site
	// sets its own polite rate without affecting the others — e621 wants <=2
	// rps, while a logged-in Pixiv session tolerates browser-like bursts.
	MinInterval float64 `json:"min_interval,omitempty"`

	// MediaInterval spaces media requests (files, thumbnails) for sites whose
	// file servers share the API's per-IP limit (kemono, pawchive …).
	// 0 keeps media unpaced: a dedicated CDN needs no API interval.
	MediaInterval float64 `json:"media_interval,omitempty"`

	RequiresAuth bool `json:"requires_auth"` // needs credentials to search

	// OptionalAuth: credentials improve the experience but are not needed to
	// search — the auth dialog is offered (never forced) and its fields don't
	// gate Save. DeviantArt: a pasted browser Cookie unlocks original-file
	// downloads.
	OptionalAuth bool `json:"optional_auth"`

	// CfProtected: this site may sit behind Cloudflare's interactive
	// challenge ("Just a moment…"). When true the auth dialog offers two
	// optional extras — cf_clearance + User-Agent pasted from a browser —
	// that the client replays to slip past the 403. Unlike RequiresAuth this
	// never gates searches: the site still works whenever Cloudflare isn't
	// challenging. See CF_FIELDS.
	CfProtected bool `json:"cf_protected"`

	// CfExtraDomains are additional, explicitly trusted hosts that receive
	// the same Cloudflare clearance and matching User-Agent. Kept opt-in
	// because a site's media CDN may belong to an unrelated third party.
	CfExtraDomains []string `json:"cf_extra_domains,omitempty"`

	AuthURL   string      `json:"auth_url,omitempty"` // where the user gets credentials
	AuthFie   []CredField `json:"auth_fields"`        // fields the auth dialog asks for
	Filters   FilterList  `json:"-"`                  // nil -> engine's filter set
	PostURLTm string      `json:"post_url_template,omitempty"`
}

// AuthFields returns the credential field list, defaulting like Python's
// dataclass default (("user_id","User ID"),("api_key","API Key")).
// An explicit empty slice hides API fields for sites with only CF credentials.
func (s *Site) AuthFields() []CredField {
	if s.AuthFie != nil {
		return s.AuthFie
	}
	return []CredField{{"user_id", "User ID"}, {"api_key", "API Key"}}
}

// Base is "https://<domain>".
func (s *Site) Base() string { return "https://" + s.Domain }

// APIBase defaults to the public website, retaining existing site routes.
func (s *Site) APIBase() string {
	if s.APIDomain != "" {
		return "https://" + s.APIDomain
	}
	return s.Base()
}

// Nick is the short name for compact UI (domain without a leading www./TLD).
func (s *Site) Nick() string {
	parts := strings.Split(s.Domain, ".")
	for _, p := range parts {
		if p != "www" {
			return p
		}
	}
	return s.Domain
}

// PostURL builds the permalink for a post. When PostURLTm is set it is used
// verbatim ("{id}" placeholder); otherwise the engine-keyed default applies.
func (s *Site) PostURL(id PostId) string {
	idStr := id.String()
	if s.PostURLTm != "" {
		return strings.ReplaceAll(s.PostURLTm, "{id}", idStr)
	}
	switch s.Engine {
	case "moebooru":
		return fmt.Sprintf("%s/post/show/%s", s.Base(), idStr)
	case "gelbooru":
		return fmt.Sprintf("%s/index.php?page=post&s=view&id=%s", s.Base(), idStr)
	case "pixiv":
		return fmt.Sprintf("%s/artworks/%s", s.Base(), idStr)
	}
	return fmt.Sprintf("%s/posts/%s", s.Base(), idStr)
}

// FileURLFromMd5 rebuilds a data-file URL from the always-present md5.
func (s *Site) FileURLFromMd5(md5, ext string) string {
	return fmt.Sprintf("https://%s/data/%s/%s/%s.%s",
		s.StaticDomain, md5[:2], md5[2:4], md5, ext)
}

// PreviewURLFromMd5 rebuilds a preview URL from md5.
func (s *Site) PreviewURLFromMd5(md5 string) string {
	return fmt.Sprintf("https://%s/data/preview/%s/%s/%s.jpg",
		s.StaticDomain, md5[:2], md5[2:4], md5)
}

// Host is the URL netloc used as throttle/CF keys.
func (s *Site) Host() string { return s.Domain }

// TagSuggestion is one autocomplete hit.
type TagSuggestion struct {
	Name       string `json:"name"`
	PostCount  int64  `json:"post_count"`
	Category   int    `json:"category"`
	Antecedent string `json:"antecedent,omitempty"`
	// Value is an optional search token committed when it differs from the
	// display Name — Pixiv authors are users whose chip shows the nick but
	// searches by the numeric user id. Empty "" commits the name itself.
	Value string `json:"value,omitempty"`
}

// PostFile describes the downloadable media of a post.
type PostFile struct {
	URL    string `json:"url"`
	MD5    string `json:"md5"` // content hash (md5 or sha1 per engine)
	Ext    string `json:"ext"`
	Size   int64  `json:"size"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// PostInfo is one downloadable/viewable post.
type PostInfo struct {
	ID         PostId    `json:"id"`
	Site       string    `json:"site"`
	File       *PostFile `json:"file,omitempty"`
	PreviewURL string    `json:"preview_url"`
	SampleURL  string    `json:"sample_url"`
	Score      int64     `json:"score"`
	FavCount   int64     `json:"fav_count"`
	Rating     string    `json:"rating"`
	Artists    []string  `json:"artists,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	// AuthorID is an opaque per-engine author identifier ("" when not
	// applicable). Pixiv authors are users, not searchable tags, so the artist
	// name alone can't open their gallery — this carries the numeric pixiv
	// userId so the «работы автора» browser can query user_id:<n>.
	AuthorID string `json:"author_id,omitempty"`
	// CreatedAt is upload time as Unix epoch seconds (0 = unknown).
	CreatedAt int64 `json:"created_at"`
	// PageCount/PageIndex describe multi-image works (Pixiv albums):
	// PageCount==1 is an ordinary single-image post.
	PageCount int `json:"page_count"`
	PageIndex int `json:"page_index"`
	// Counts source attachments even when previews or access restrictions leave File empty.
	AttachmentCount int `json:"attachment_count,omitempty"`
	// Extra is an opaque per-engine scratch bag (JSON-safe values only — it
	// is persisted with favourites). Booru engines leave it empty;
	// Kemono/Coomer stash structured identity + file lists here. The core
	// never inspects it; only the owning engine reads its own keys.
	Extra map[string]any `json:"extra,omitempty"`
}

// Clone deep-copies the mutable parts (maps/slices).
func (p *PostInfo) Clone() *PostInfo {
	q := *p
	if p.File != nil {
		f := *p.File
		q.File = &f
	}
	if p.Artists != nil {
		q.Artists = append([]string(nil), p.Artists...)
	}
	if p.Tags != nil {
		q.Tags = append([]string(nil), p.Tags...)
	}
	if p.Extra != nil {
		q.Extra = cloneMap(p.Extra)
	}
	return &q
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case map[string]any:
			out[k] = cloneMap(t)
		case []any:
			out[k] = cloneSlice(t)
		default:
			out[k] = v
		}
	}
	return out
}

func cloneSlice(s []any) []any {
	out := make([]any, len(s))
	for i, v := range s {
		switch t := v.(type) {
		case map[string]any:
			out[i] = cloneMap(t)
		case []any:
			out[i] = cloneSlice(t)
		default:
			out[i] = v
		}
	}
	return out
}

// NormalizePostId keeps old persisted JSON compatible: decimal-only strings
// convert back to int; everything else stays an exact string (Misskey).
func NormalizePostId(v any) PostId {
	switch t := v.(type) {
	case nil:
		return S("")
	case int:
		return I(int64(t))
	case int32:
		return I(int64(t))
	case int64:
		return I(t)
	case float64:
		return I(int64(t))
	case jsonNumber:
		n, _ := t.Int64()
		return I(n)
	case string:
		return FromIDString(t)
	}
	return S(fmt.Sprint(v))
}

// jsonNumber matches encoding/json.Number without importing it here.
type jsonNumber interface{ Int64() (int64, error) }

// FromIDString parses a stored id string into the canonical form.
func FromIDString(text string) PostId {
	text = strings.TrimSpace(text)
	if text == "" {
		return S("")
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil && !strings.HasPrefix(strings.TrimSpace(text), "+") {
		return I(n)
	}
	return S(text)
}

// AsInt mirrors models.as_int: best-effort int conversion, 0 on failure.
func AsInt(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return int64(t)
	case jsonNumber:
		n, err := t.Int64()
		if err != nil {
			if f, e := strconv.ParseFloat(fmt.Sprint(v), 64); e == nil {
				return int64(f)
			}
		}
		return n
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	}
	return 0
}

var normRating = map[string]string{
	"general": "s", "safe": "s", "sensitive": "s",
	"questionable": "q", "explicit": "e",
}

// NormRating normalises a rating to a single letter (Gelbooru sends words).
func NormRating(v any) string {
	var s string
	switch t := v.(type) {
	case string:
		s = strings.ToLower(strings.TrimSpace(t))
	default:
		s = strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
	}
	if s == "" || s == "?" {
		return "?"
	}
	if r, ok := normRating[s]; ok {
		return r
	}
	return s[:1]
}

// ParseTime is a best-effort parse of an upload timestamp into Unix epoch
// seconds; 0 when unknown. Boorus disagree on the shape: Moebooru sends a
// numeric epoch, e621 an ISO 8601 string, Gelbooru a Ruby Time#to_s string
// ("Wed Jun 04 12:00:00 -0500 2025").
func ParseTime(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return int64(t)
	case jsonNumber:
		n, _ := t.Int64()
		return n
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		return parseTimeString(t)
	}
	return 0
}

func parseTimeString(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if strings.TrimLeft(s, "-") != "" && allDigits(strings.TrimLeft(s, "-")) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return n
		}
	}
	// ISO 8601 (e621), tolerate a Z zone.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04:05.999999999-07:00", "2006-01-02 15:04:05 -0700",
		"2006-01-02T15:04:05"} {
		if ts, err := time.Parse(layout, strings.Replace(s, "Z", "+00:00", 1)); err == nil {
			return ts.Unix()
		}
	}
	// Ruby Time#to_s (Gelbooru): "Wed Jun 04 12:00:00 -0500 2025".
	const ruby = "Mon Jan 02 15:04:05 -0700 2006"
	if ts, err := time.Parse(ruby, s); err == nil {
		return ts.Unix()
	}
	return 0
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FormatTime renders a compact local-time display string (” when 0).
func FormatTime(epoch int64) string {
	if epoch == 0 {
		return ""
	}
	return time.Unix(epoch, 0).Local().Format("2006-01-02 15:04")
}
