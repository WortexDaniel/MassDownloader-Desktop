// Package reverse — the three providers: SauceNAO, Fluffle, IQDB.
// Ports of app/search/saucenao.py, fluffle.py, iqdb.py.
package reverse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"massdownloader/internal/core"
)

// buildMultipart wraps image bytes as a "file" multipart part.
func buildMultipart(field, filename string, image []byte) (*bytes.Buffer, string) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile(field, filename)
	part.Write(image)
	w.Close()
	return body, w.FormDataContentType()
}

// -- SauceNAO -----------------------------------------------------------------

type SauceNAO struct{}

func (SauceNAO) Key() string   { return "saucenao" }
func (SauceNAO) Label() string { return "SauceNAO" }
func (SauceNAO) Blurb() string {
	return "Pixiv · аниме-бору · e621 · FurAffinity · DeviantArt"
}

func (p SauceNAO) Available(getSetting func(string) string) (bool, string) {
	if strings.TrimSpace(getSetting("saucenao_api_key")) == "" {
		return false, "нужен бесплатный ключ SauceNAO API (вставьте его в поле выше)"
	}
	return true, ""
}

// Search posts output_type=2 JSON + the file part to saucenao.com/search.php.
func (p SauceNAO) Search(c *core.ApiClient, image []byte, filename string,
	getSetting func(string) string, shouldStop func() bool) ([]Match, error) {

	key := strings.TrimSpace(getSetting("saucenao_api_key"))
	q := url.Values{"output_type": {"2"}, "api_key": {key}, "db": {"999"},
		"numres": {"16"}}
	body, ctype := buildMultipart("file", orDefault(filename, "image.png"), image)
	data, err := c.PostJSON(nil,
		"https://saucenao.com/search.php", q, body, ctype, nil)
	if err != nil {
		return nil, err
	}
	m, ok := data.(map[string]any)
	if !ok {
		return nil, &core.ApiError{Msg: "SauceNAO: неожиданный ответ"}
	}
	return p.parse(m, minSim(getSetting))
}

// parse is pure (no network) — unit-testable with a canned payload.
func (p SauceNAO) parse(payload map[string]any, minSimilarity float64) ([]Match, error) {
	head, _ := payload["header"].(map[string]any)
	status := int64(0)
	if head != nil {
		status = core.AsInt(head["status"])
	}
	results, _ := payload["results"].([]any)
	if len(results) == 0 {
		// negative status -> account/rate problem; positive -> bad image
		if status < 0 && head != nil {
			msg := core.Str_(head["message"])
			if msg == "" {
				msg = "превышен лимит запросов или неверный ключ"
			}
			return nil, &core.ApiError{Msg: "SauceNAO: " + msg}
		}
		return nil, nil
	}
	out := make([]Match, 0, len(results))
	for _, item := range results {
		r, ok := item.(map[string]any)
		if !ok {
			continue
		}
		rh, _ := r["header"].(map[string]any)
		dataObj, _ := r["data"].(map[string]any)
		sim := float64(core.AsInt(rh["similarity"]))
		if f, err := strconv.ParseFloat(core.Str_(rh["similarity"]), 64); err == nil {
			sim = f
		}
		if sim < minSimilarity {
			continue
		}
		bestURL := bestURL(dataObj)
		if bestURL == "" {
			if exts, _ := dataObj["ext_urls"].([]any); len(exts) == 0 {
				continue
			}
		}
		thumb := ""
		if rh != nil {
			thumb = core.Str_(rh["thumbnail"])
		}
		indexName := ""
		if rh != nil {
			indexName = core.Str_(rh["index_name"])
		}
		m := Match{
			Provider:   "SauceNAO",
			Source:     sourceLabel(bestURL, indexName),
			URL:        bestURL,
			ThumbURL:   thumb,
			Similarity: sim,
			Author:     sauceAuthor(dataObj),
		}
		resolveInApp(&m)
		out = append(out, m)
	}
	return out, nil
}

// bestURL prefers a URL the app can browse in-app.
func bestURL(data map[string]any) string {
	if data == nil {
		return ""
	}
	exts, _ := data["ext_urls"].([]any)
	var urls []string
	for _, u := range exts {
		if s, ok := u.(string); ok && s != "" {
			urls = append(urls, s)
		}
	}
	for _, u := range urls { // prefer a URL the app can browse
		if key, _ := FindSitePost(u); key != "" {
			return u
		}
	}
	if len(urls) > 0 {
		return urls[0]
	}
	src := core.Str_(data["source"])
	if strings.HasPrefix(src, "http") {
		return src
	}
	return ""
}

func sauceAuthor(data map[string]any) string {
	if data == nil {
		return ""
	}
	for _, k := range []string{"member_name", "creator", "author_name",
		"author", "twitter_user_handle", "user_name", "pawoo_user_username"} {
		v := data[k]
		switch t := v.(type) {
		case []any:
			var parts []string
			for _, x := range t {
				if s, ok := x.(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, ", ")
			}
		case string:
			if t != "" {
				return t
			}
		}
	}
	return ""
}

var indexPrefixRe = regexp.MustCompile(`^Index #\d+:\s*`)

func sourceLabel(rawURL, indexName string) string {
	host := ""
	if rawURL != "" {
		if u, err := url.Parse(rawURL); err == nil {
			host = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		}
	}
	if host != "" {
		return host
	}
	name := indexPrefixRe.ReplaceAllString(indexName, "")
	if i := strings.Index(name, " - "); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "источник"
	}
	return name
}

// -- Fluffle ------------------------------------------------------------------

type Fluffle struct{}

func (Fluffle) Key() string   { return "fluffle" }
func (Fluffle) Label() string { return "Fluffle" }
func (Fluffle) Blurb() string {
	return "Фурри: e621 · FurAffinity · Weasyl · Furbooru (без ключа)"
}

func (p Fluffle) Available(func(string) string) (bool, string) { return true, "" }

// Search posts the file part plus form fields to api.fluffle.xyz/v1/search.
func (p Fluffle) Search(c *core.ApiClient, image []byte, filename string,
	getSetting func(string) string, shouldStop func() bool) ([]Match, error) {

	body, ctype := buildMultipart("file", orDefault(filename, "image.png"), image)
	extra := &bytes.Buffer{}
	extra.WriteString("--")
	extra.WriteString(strings.Trim(ctype, "\""))
	// Simpler: send fields via a fresh multipart writer including both fields
	// and the file in one body.
	fresh := &bytes.Buffer{}
	w := multipart.NewWriter(fresh)
	_ = w.WriteField("includeNsfw", "true")
	_ = w.WriteField("limit", "32")
	part, _ := w.CreateFormFile("file", orDefault(filename, "image.png"))
	part.Write(image)
	w.Close()
	_ = body
	_ = extra
	data, err := c.PostJSON(nil, "https://api.fluffle.xyz/v1/search", nil,
		fresh, w.FormDataContentType(), nil)
	if err != nil {
		return nil, err
	}
	m, ok := data.(map[string]any)
	if !ok {
		return nil, &core.ApiError{Msg: "Fluffle: неожиданный ответ"}
	}
	return p.parse(m, minSim(getSetting))
}

var matchNote = map[string]string{
	"exact": "точное совпадение", "alternative": "похожее",
	"unlikely": "маловероятно",
}

// parse is pure — unit-testable with a canned payload.
func (p Fluffle) parse(payload map[string]any, minSimilarity float64) ([]Match, error) {
	if payload == nil {
		return nil, &core.ApiError{Msg: "Fluffle: неожиданный ответ"}
	}
	hasErr := false
	if errs, ok := payload["errors"]; ok && errs != nil {
		hasErr = true
	}
	code := core.Str_(payload["code"])
	if hasErr || code != "" {
		msg := core.Str_(payload["message"])
		if msg == "" {
			msg = code
		}
		if msg == "" {
			msg = "ошибка запроса"
		}
		return nil, &core.ApiError{Msg: "Fluffle: " + msg}
	}
	results, _ := payload["results"].([]any)
	out := make([]Match, 0, len(results))
	for _, item := range results {
		r, ok := item.(map[string]any)
		if !ok {
			continue
		}
		score := float64(core.AsInt(r["score"]))
		if f, err := strconvParseFloat(fmt.Sprint(r["score"])); err == nil {
			score = f * 100.0
		} else if score > 1 {
			score = float64(core.AsInt(r["score"]))
		}
		if score < minSimilarity {
			continue
		}
		rawURL := core.Str_(r["location"])
		if rawURL == "" {
			continue
		}
		platform := core.Str_(r["platform"])
		source := platform
		if source == "" {
			if u, err := url.Parse(rawURL); err == nil {
				source = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
			}
		}
		credits, _ := r["credits"].([]any)
		var authors []string
		for _, cr := range credits {
			cm, ok := cr.(map[string]any)
			if !ok {
				continue
			}
			if name := core.Str_(cm["name"]); name != "" {
				authors = append(authors, name)
			}
		}
		thumb := ""
		if tm, ok := r["thumbnail"].(map[string]any); ok {
			thumb = core.Str_(tm["url"])
		}
		m := Match{
			Provider:   "Fluffle",
			Source:     source,
			URL:        rawURL,
			ThumbURL:   thumb,
			Similarity: round1(score),
			Author:     strings.Join(authors, ", "),
			Note:       matchNote[core.Str_(r["match"])],
		}
		resolveInApp(&m)
		out = append(out, m)
	}
	return out, nil
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// -- IQDB ---------------------------------------------------------------------

const iqdbSearchURL = "https://iqdb.org/"

var (
	iqdbBlockRe  = regexp.MustCompile(`(?is)<table\b.*?</table>`)
	iqdbHeaderRe = regexp.MustCompile(`(?is)<th[^>]*>(.*?)</th>`)
	iqdbHrefRe   = regexp.MustCompile(`(?i)<a[^>]+href=["']([^"']+)["']`)
	iqdbImgRe    = regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["']`)
	iqdbSimRe    = regexp.MustCompile(`(?i)(\d+)%\s*similarity`)
	iqdbResRe    = regexp.MustCompile(`(\d+×\d+)(?:\s*\[([^\]]+)\])?`)
	iqdbErrRe    = regexp.MustCompile(`(?is)<div[^>]*class=['"]err['"][^>]*>(.*?)</div>`)
	tagRe        = regexp.MustCompile(`<[^>]+>`)
)

// iqdbNames are friendlier source names for the hosts IQDB returns.
var iqdbNames = map[string]string{
	"danbooru.donmai.us":      "Danbooru",
	"gelbooru.com":            "Gelbooru",
	"yande.re":                "yande.re",
	"konachan.com":            "Konachan",
	"chan.sankakucomplex.com": "Sankaku",
	"sankakucomplex.com":      "Sankaku",
	"anime-pictures.net":      "Anime-Pictures",
	"zerochan.net":            "Zerochan",
	"e-shuushuu.net":          "E-Shuushuu",
}

type IQDB struct{}

func (IQDB) Key() string   { return "iqdb" }
func (IQDB) Label() string { return "IQDB" }
func (IQDB) Blurb() string {
	return "Аниме-бору: Danbooru · Gelbooru · yande.re · Konachan · " +
		"Sankaku · Zerochan (без ключа)"
}

func (p IQDB) Available(func(string) string) (bool, string) { return true, "" }

// Search POSTs the image as a file multipart part to iqdb.org and scrapes the
// result tables (IQDB has no JSON API).
func (p IQDB) Search(c *core.ApiClient, image []byte, filename string,
	getSetting func(string) string, shouldStop func() bool) ([]Match, error) {

	fresh := &bytes.Buffer{}
	w := multipart.NewWriter(fresh)
	_ = w.WriteField("MAX_FILE_SIZE", "8388608") // mirrors the form
	part, _ := w.CreateFormFile("file", orDefault(filename, "image.png"))
	part.Write(image)
	w.Close()
	text, err := c.PostText(nil, iqdbSearchURL, nil, fresh,
		w.FormDataContentType(), nil)
	if err != nil {
		return nil, err
	}
	return p.parse(text, minSim(getSetting))
}

// parse is pure — unit-testable with a canned HTML page.
func (p IQDB) parse(htmlText string, minSimilarity float64) ([]Match, error) {
	if err := iqdbErrRe.FindStringSubmatch(htmlText); err != nil {
	}
	if m := iqdbErrRe.FindStringSubmatch(htmlText); m != nil {
		msg := strings.TrimSpace(tagRe.ReplaceAllString(m[1], ""))
		// "no relevant matches" is a normal empty result, not an error
		if msg != "" && !strings.Contains(strings.ToLower(msg), "no relevant") {
			return nil, &core.ApiError{Msg: "IQDB: " + msg}
		}
	}
	out := []Match{}
	seen := map[string]bool{}
	for _, block := range iqdbBlockRe.FindAllString(htmlText, -1) {
		header := iqdbHeaderRe.FindStringSubmatch(block)
		if header != nil &&
			strings.Contains(strings.ToLower(stripTags(header[1])), "your image") {
			continue // the uploaded image preview block
		}
		simM := iqdbSimRe.FindStringSubmatch(block)
		hrefM := iqdbHrefRe.FindStringSubmatch(block)
		if simM == nil || hrefM == nil {
			continue // not a match block
		}
		sim, convErr := strconv.ParseFloat(simM[1], 64)
		if convErr != nil || sim < minSimilarity {
			continue
		}
		u := absURL(hrefM[1])
		host := hostOf(u)
		if u == "" || host == "" || host == "iqdb.org" {
			continue // self/garbage link
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		note := ""
		if resM := iqdbResRe.FindStringSubmatch(block); resM != nil {
			note = resM[1]
			if resM[2] != "" {
				note += " [" + resM[2] + "]"
			}
		}
		thumb := ""
		if imgM := iqdbImgRe.FindStringSubmatch(block); imgM != nil {
			thumb = absURL(imgM[1])
		}
		source := iqdbNames[host]
		if source == "" {
			source = host
		}
		m := Match{
			Provider:   "IQDB",
			Source:     source,
			URL:        u,
			ThumbURL:   thumb,
			Similarity: round1(sim),
			Note:       note,
		}
		resolveInApp(&m)
		out = append(out, m)
	}
	return out, nil
}

func stripTags(s string) string { return tagRe.ReplaceAllString(s, "") }

func absURL(u string) string {
	u = strings.TrimSpace(u)
	switch {
	case strings.HasPrefix(u, "//"):
		return "https:" + u
	case strings.HasPrefix(u, "/"):
		return "https://iqdb.org" + u
	}
	return u
}

func hostOf(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	if u, err := url.Parse(rawURL); err == nil {
		return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	}
	return ""
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

// RegisterProviders installs all three providers in UI order.
func RegisterProviders() {
	Register(ExactMD5{})
	Register(Fluffle{})
	Register(IQDB{})
	Register(SauceNAO{})
}

var _ = json.Marshal
