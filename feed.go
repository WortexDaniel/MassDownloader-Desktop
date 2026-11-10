package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"massdownloader/internal/core"
)

type FeedRule struct {
	Site          string         `json:"site"`
	Tags          []string       `json:"tags"`
	Exclude       []string       `json:"exclude"`
	Authors       []string       `json:"authors"`
	TagExpression string         `json:"tag_expression,omitempty"`
	Sort          string         `json:"sort,omitempty"`
	Rating        string         `json:"rating,omitempty"`
	Score         *FeedThreshold `json:"score,omitempty"`
	Faves         *FeedThreshold `json:"faves,omitempty"`
	HideText      bool           `json:"hide_text,omitempty"`
}

type FeedThreshold struct {
	Enabled bool   `json:"enabled"`
	Op      string `json:"op"`
	N       int64  `json:"n"`
}

func validateFeedThreshold(site, key string, value *FeedThreshold) error {
	if value == nil {
		return nil
	}
	spec := core.FiltersFor(core.GetSite(site)).Find(key)
	if spec == nil || spec.Kind != core.KindThreshold {
		return fmt.Errorf("%s: фильтр %s не поддерживается", site, key)
	}
	tokens := strings.Fields(spec.OpTokens)
	if len(tokens) == 0 {
		tokens = []string{">=", ">", "="}
	}
	if value.Op == "" {
		value.Op = tokens[0]
	}
	valid := false
	for _, token := range tokens {
		valid = valid || value.Op == token
	}
	maximum := spec.Maximum
	if maximum == 0 {
		maximum = 1_000_000
	}
	if !valid || value.N < spec.Minimum || value.N > maximum {
		return fmt.Errorf("%s: неверное условие фильтра %s", site, spec.Label)
	}
	return nil
}

type FeedFilter struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Rules    []FeedRule `json:"rules"`
	HideText bool       `json:"hide_text,omitempty"` // Built-in favourite-author filter.
}
type FeedSource struct {
	Site        string          `json:"site"`
	Query       string          `json:"query"`
	Name        string          `json:"name"`
	MatchTags   []string        `json:"match_tags,omitempty"`
	ExcludeTags []string        `json:"exclude_tags,omitempty"`
	TextMatch   bool            `json:"text_match,omitempty"`
	MatchAny    []FeedTagClause `json:"match_any,omitempty"`
	Sort        string          `json:"sort,omitempty"`
	HideText    bool            `json:"hide_text,omitempty"`
}

func randomFeedID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func (a *App) savedFeedFilters() []FeedFilter {
	filters := []FeedFilter{}
	b, _ := json.Marshal(a.settings.Get("feed_filters"))
	_ = json.Unmarshal(b, &filters)
	return filters
}
func (a *App) FeedFilters() []FeedFilter {
	return append([]FeedFilter{{ID: "favorites", Name: "Избранные авторы", Rules: []FeedRule{}, HideText: a.settings.Get("feed_hide_text") == true}}, a.savedFeedFilters()...)
}
func (a *App) SaveFeedFilter(filter FeedFilter) (FeedFilter, error) {
	if filter.ID == "favorites" {
		if len(filter.Rules) != 0 {
			return FeedFilter{}, fmt.Errorf("источники этого фильтра задаются избранными авторами")
		}
		a.dataMu.Lock()
		a.settings.Set("feed_hide_text", filter.HideText)
		err := a.settings.Save()
		a.dataMu.Unlock()
		if err != nil {
			return FeedFilter{}, err
		}
		a.emit("feed:filters-changed")
		return a.FeedFilters()[0], nil
	}
	filter.Name = strings.TrimSpace(filter.Name)
	if filter.ID == "favorites" || filter.Name == "" || len(filter.Name) > 200 || len(filter.Rules) == 0 || len(filter.Rules) > 64 {
		return FeedFilter{}, fmt.Errorf("задай название и хотя бы один сайт")
	}
	for i := range filter.Rules {
		rule := &filter.Rules[i]
		if core.GetSite(rule.Site) == nil {
			return FeedFilter{}, fmt.Errorf("неизвестный сайт: %s", rule.Site)
		}
		if rule.HideText && core.FiltersFor(core.GetSite(rule.Site)).Find("hide_text") == nil {
			return FeedFilter{}, fmt.Errorf("%s: фильтр Скрыть текст не поддерживается", rule.Site)
		}
		rule.Tags = cleanFeedTerms(rule.Tags)
		rule.Exclude = cleanFeedTerms(rule.Exclude)
		rule.Authors = cleanFeedTerms(rule.Authors)
		rule.TagExpression = strings.TrimSpace(rule.TagExpression)
		rule.Sort = strings.TrimSpace(rule.Sort)
		if rule.Sort != "" {
			valid := false
			if spec := core.FiltersFor(core.GetSite(rule.Site)).Find("sort"); spec != nil && spec.Kind == core.KindChoice {
				for _, option := range spec.Options {
					valid = valid || option.Value == rule.Sort
				}
			}
			if !valid {
				return FeedFilter{}, fmt.Errorf("%s: неизвестная сортировка: %s", rule.Site, rule.Sort)
			}
		}
		rule.Rating = strings.TrimSpace(rule.Rating)
		if rule.Rating != "" {
			valid := false
			if spec := core.FiltersFor(core.GetSite(rule.Site)).Find("rating"); spec != nil && spec.Kind == core.KindChoice {
				for _, option := range spec.Options {
					valid = valid || option.Value == rule.Rating
				}
			}
			if !valid {
				return FeedFilter{}, fmt.Errorf("%s: неизвестный рейтинг: %s", rule.Site, rule.Rating)
			}
		}
		if err := validateFeedThreshold(rule.Site, "score", rule.Score); err != nil {
			return FeedFilter{}, err
		}
		if err := validateFeedThreshold(rule.Site, "faves", rule.Faves); err != nil {
			return FeedFilter{}, err
		}
		if _, err := compileFeedExpression(rule.TagExpression); err != nil {
			return FeedFilter{}, fmt.Errorf("%s: %w", rule.Site, err)
		}
	}
	a.dataMu.Lock()
	if filter.ID == "" {
		filter.ID = randomFeedID()
	}
	filters := a.savedFeedFilters()
	found := false
	for i := range filters {
		if filters[i].ID == filter.ID {
			filters[i] = filter
			found = true
			break
		}
	}
	if !found {
		filters = append(filters, filter)
	}
	a.settings.Set("feed_filters", filters)
	err := a.settings.Save()
	a.dataMu.Unlock()
	if err == nil {
		a.emit("feed:filters-changed")
	}
	return filter, err
}
func cleanFeedTerms(terms []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term != "" && !seen[term] {
			out = append(out, term)
			seen[term] = true
		}
	}
	return out
}
func (a *App) DeleteFeedFilter(id string) error {
	if id == "favorites" {
		return fmt.Errorf("стандартный фильтр нельзя удалить")
	}
	a.dataMu.Lock()
	filters := []FeedFilter{}
	for _, f := range a.savedFeedFilters() {
		if f.ID != id {
			filters = append(filters, f)
		}
	}
	a.settings.Set("feed_filters", filters)
	err := a.settings.Save()
	a.dataMu.Unlock()
	if err == nil {
		a.emit("feed:filters-changed")
	}
	return err
}
func (a *App) FeedSources(id string) ([]FeedSource, error) {
	out := []FeedSource{}
	seen := map[string]bool{}
	add := func(site, query, name string) {
		siteInfo := core.GetSite(site)
		hideText := id == "favorites" && a.settings.Get("feed_hide_text") == true && siteInfo != nil && core.FiltersFor(siteInfo).Find("hide_text") != nil
		if hideText {
			query = strings.TrimSpace(query + " " + core.HIDE_TEXT_TOKEN)
		}
		key := site + "\x00" + query
		if core.GetSite(site) != nil && !seen[key] {
			out = append(out, FeedSource{Site: site, Query: query, Name: name, HideText: hideText})
			seen[key] = true
		}
	}
	if id == "favorites" {
		for _, author := range a.Authors() {
			add(core.Str_(author["site"]), core.Str_(author["query"]), core.Str_(author["name"]))
		}
		return out, nil
	}
	for _, f := range a.savedFeedFilters() {
		if f.ID != id {
			continue
		}
		for _, r := range f.Rules {
			site := core.GetSite(r.Site)
			engineImpl := core.Registry.EngineFor(site)
			if engineImpl == nil {
				return nil, fmt.Errorf("неизвестный сайт: %s", r.Site)
			}
			clauses, err := compileFeedExpression(r.TagExpression)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", r.Site, err)
			}
			for i := range clauses {
				clauses[i].Tags = append(append([]string{}, r.Tags...), clauses[i].Tags...)
				clauses[i].Exclude = append(append([]string{}, r.Exclude...), clauses[i].Exclude...)
			}
			clauses, err = uniqueFeedClauses(clauses)
			if err != nil {
				return nil, err
			}
			authors := r.Authors
			if len(authors) == 0 {
				authors = []string{""}
			}
			for _, author := range authors {
				for _, clause := range clauses {
					values := a.FilterDefaults(r.Site)
					values["hide_text"] = r.HideText
					if r.Rating != "" {
						values["rating"] = r.Rating
					}
					for key, threshold := range map[string]*FeedThreshold{"score": r.Score, "faves": r.Faves} {
						if threshold != nil {
							values[key] = map[string]any{"enabled": threshold.Enabled, "op": threshold.Op, "n": threshold.N}
						}
					}
					queryTags, queryExclude := clause.Tags, clause.Exclude
					if r.TagExpression != "" {
						queryTags, queryExclude = feedQueryTerms(queryTags), feedQueryTerms(queryExclude)
					}
					textMatch := engineImpl.TagPhrase()
					for _, spec := range core.FiltersFor(core.GetSite(r.Site)) {
						if spec.Key == "sort" && spec.Kind == core.KindChoice && r.Sort != "" {
							values[spec.Key] = r.Sort
						}
						switch spec.Kind {
						case core.KindTags:
							values[spec.Key] = queryTags
						case core.KindExclude:
							values[spec.Key] = queryExclude
						case core.KindAuthors:
							values[spec.Key] = []string{}
							if author != "" {
								term := author
								if core.GetSite(r.Site).Engine == "pixiv" {
									term = "user_id:" + strings.TrimPrefix(author, "user_id:")
								}
								values[spec.Key] = []string{term}
							}
						case core.KindText:
							if spec.Key == "q" {
								values[spec.Key] = strings.Join(queryTags, " ")
								textMatch = true
							}
						}
					}
					// Feed includes every media type, irrespective of preview defaults.
					values["images_only"] = false
					engine := site.Engine
					if r.TagExpression != "" && author != "" && (engine == "pixiv" || engine == "deviantart" || engine == "kemono") {
						// These author APIs paginate the gallery regardless of query tags.
						// Fetch it once and apply the OR clauses to each returned post.
						for _, spec := range core.FiltersFor(site) {
							if spec.Kind == core.KindTags || spec.Kind == core.KindExclude {
								values[spec.Key] = []string{}
							}
							if spec.Kind == core.KindText && spec.Key == "q" {
								values[spec.Key] = ""
							}
						}
					}
					source := FeedSource{Site: r.Site, Query: a.BuildQuery(r.Site, values), Name: author, Sort: r.Sort, HideText: r.HideText}
					if r.TagExpression != "" {
						source.MatchAny = []FeedTagClause{clause}
						source.TextMatch = textMatch || engine == "twitter"
					} else if author != "" && (engine == "pixiv" || engine == "deviantart" || engine == "kemono") {
						source.MatchTags = r.Tags
						source.ExcludeTags = r.Exclude
						source.TextMatch = engine != "pixiv"
					}
					// Author APIs can ignore tags. Merge their OR branches into one
					// request while retaining each branch's local conditions.
					merged := false
					for i := range out {
						if len(source.MatchAny) > 0 && len(out[i].MatchAny) > 0 && out[i].Site == source.Site && out[i].Query == source.Query && out[i].TextMatch == source.TextMatch {
							out[i].MatchAny = append(out[i].MatchAny, clause)
							merged = true
							break
						}
					}
					if !merged {
						raw, _ := json.Marshal(source)
						if !seen[string(raw)] {
							out = append(out, source)
							seen[string(raw)] = true
						}
					}
				}
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("фильтр не найден")
}

func (a *App) feedTextPath(post *core.PostInfo) string {
	root := a.settings.GetString("download_root")
	if post == nil || core.GetSite(post.Site) == nil || root == "" {
		return ""
	}
	id := sha256.Sum256([]byte(post.ID.String()))
	return filepath.Join(root, "text-posts", post.Site, fmt.Sprintf("post-%x.txt", id[:12]))
}

// Text-only works have no downloadable media. Export their readable caption
// without changing the existing downloader's file/album contract.
func (a *App) SaveFeedText(post *core.PostInfo, text string) (string, error) {
	if post == nil || core.GetSite(post.Site) == nil {
		return "", fmt.Errorf("неверный пост")
	}
	root := a.settings.GetString("download_root")
	if root == "" {
		return "", fmt.Errorf("не задана папка загрузки")
	}
	path := a.feedTextPath(post)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	content := fmt.Sprintf("%s\n%s\n\n%s\n", strings.Join(post.Artists, ", "), a.Permalink(post), text)
	f, err := os.CreateTemp(dir, ".feed-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.WriteString(content)
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	a.emit("history:changed")
	return path, nil
}

// Small pages keep a multi-source feed usable without downloading media eagerly.
// Cursor is advanced on the actual engine page, retaining social-site offsets.
func (a *App) FeedPage(siteKey, query string, cursor any) (PreviewPage, error) {
	return a.feedPage(context.Background(), siteKey, query, cursor)
}
func (a *App) runFeedPage(ctx context.Context, siteKey, query string, cursor any) (PreviewPage, error) {
	ctx = core.WithRequestPriority(ctx, core.PriorityNormal)
	site := core.GetSite(siteKey)
	e := core.Registry.EngineFor(site)
	if e == nil {
		return PreviewPage{}, fmt.Errorf("неизвестный сайт: %s", siteKey)
	}
	validationQuery, _ := core.SplitHideText(query)
	if err := e.ValidateQuery(validationQuery); err != nil {
		return PreviewPage{}, err
	}
	if cursor == nil || core.Str_(cursor) == "" {
		cursor = 1
	}
	if n, ok := cursor.(float64); ok {
		cursor = int(n)
	}
	limit := feedFetchLimit(site)
	client := a.operationClient()
	client.SetContext(ctx)
	if text, ok := cursor.(string); ok && strings.HasPrefix(text, "fb:") {
		return a.feedBuffer.read(text, siteKey, query, client.AuthIdentity())
	}
	a.feedBuffer.acknowledge(siteKey, query, client.AuthIdentity(), cursor)
	client.MaxRetries = 1 // A rate-limited feed pauses the host instead of retrying every author.
	result, err := core.FetchEnginePage(client, e, site, query, limit, cursor)
	if err != nil {
		return PreviewPage{}, err
	}
	posts := result.Posts
	page := PreviewPage{Posts: posts, Next: result.Next, HasMore: result.HasMore}
	if page.Posts == nil {
		page.Posts = []*core.PostInfo{}
	}
	return a.feedBuffer.store(page, siteKey, query, client.AuthIdentity())
}
