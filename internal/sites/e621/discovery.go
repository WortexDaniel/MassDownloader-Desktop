package e621

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"massdownloader/internal/core"
)

type discoverySearch struct {
	query, order string
	popular      bool
}

// The last conflicting metatag wins, including hand-written queries.
// order:popular is an application token and never reaches /posts.json.
func discoveryQuery(query string) discoverySearch {
	tokens := []string{}
	for _, token := range strings.Fields(query) {
		low := strings.ToLower(token)
		order := strings.HasPrefix(low, "order:") || strings.HasPrefix(low, "-order:")
		date := strings.HasPrefix(strings.TrimLeft(low, "-~"), "date:")
		if order || date {
			kept := tokens[:0]
			for _, old := range tokens {
				oldLow := strings.ToLower(old)
				oldOrder := strings.HasPrefix(oldLow, "order:") || strings.HasPrefix(oldLow, "-order:")
				oldDate := strings.HasPrefix(strings.TrimLeft(oldLow, "-~"), "date:")
				if order && oldOrder || low == "order:popular" && oldDate || date && oldLow == "order:popular" {
					continue
				}
				kept = append(kept, old)
			}
			tokens = kept
		}
		tokens = append(tokens, token)
	}
	result := discoverySearch{}
	for _, token := range tokens {
		low := strings.ToLower(token)
		if strings.HasPrefix(low, "order:") {
			result.order = strings.TrimPrefix(low, "order:")
		} else if strings.HasPrefix(low, "-order:") {
			result.order = strings.TrimPrefix(low, "-order:")
			if result.order == "id" || result.order == "id_asc" {
				result.order = "id_desc"
			} else if result.order == "id_desc" {
				result.order = "id"
			}
		}
		if low == "order:popular" {
			result.popular = true
		}
	}
	result.query = strings.Join(tokens, " ")
	return result
}

type popularCursor struct {
	Site     string   `json:"site"`
	Query    string   `json:"query"`
	Identity string   `json:"identity"`
	IDs      []string `json:"ids"`
}

// Popular has a finite server-selected list. The cursor retains its remaining
// IDs so later chunks cannot fetch a different day's list or change its rank.
func (e *Engine) FetchPage(c *core.ApiClient, site *core.Site, tags string, limit int, cursor any) (core.PageResult, error) {
	q := discoveryQuery(tags)
	if !q.popular {
		posts, err := e.FetchPosts(c, site, q.query, limit, cursor)
		if err != nil {
			return core.PageResult{}, err
		}
		next := e.NextCursor(q.query, posts, cursor)
		return core.PageResult{Posts: posts, Next: next, HasMore: next != nil}, nil
	}
	if site.Key != "e621" && site.Key != "e6ai" {
		return core.PageResult{}, fmt.Errorf("Популярное недоступно для %s", site.Key)
	}
	if limit <= 0 {
		return core.PageResult{}, fmt.Errorf("некорректный размер страницы")
	}
	limit = min(limit, 320)
	searchTokens := []string{}
	for _, token := range strings.Fields(q.query) {
		if strings.ToLower(token) != "order:popular" {
			searchTokens = append(searchTokens, token)
		}
	}
	search := strings.Join(searchTokens, " ")
	var posts []*core.PostInfo
	var err error
	var remaining []string
	text := pageString(cursor)
	if strings.HasPrefix(text, "popular:") {
		var saved popularCursor
		data, decodeErr := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(text, "popular:"))
		if decodeErr != nil || len(data) > 16384 {
			return core.PageResult{}, fmt.Errorf("некорректный курсор популярного")
		}
		if err := json.Unmarshal(data, &saved); err != nil || saved.Site != site.Key || saved.Query != q.query || saved.Identity != c.AuthIdentity() || len(saved.IDs) == 0 || len(saved.IDs) > 320 {
			return core.PageResult{}, fmt.Errorf("курсор популярного не соответствует поиску")
		}
		for _, id := range saved.IDs {
			if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
				return core.PageResult{}, fmt.Errorf("некорректный ID в курсоре популярного")
			}
		}
		end := min(limit, len(saved.IDs))
		posts, err = e.popularMatches(c, site, search, saved.IDs[:end])
		remaining = saved.IDs[end:]
	} else {
		if cursor != nil && text != "1" && text != "" {
			return core.PageResult{}, fmt.Errorf("некорректная страница популярного")
		}
		data, fetchErr := c.GetJSON(nil, site.Base()+"/popular.json", url.Values{}, nil)
		if fetchErr != nil {
			return core.PageResult{}, fetchErr
		}
		posts, err = parsePosts(data, site, e.ParsePost)
		if err != nil {
			return core.PageResult{}, err
		}
		if len(posts) > 320 {
			return core.PageResult{}, fmt.Errorf("слишком большая подборка популярного")
		}
		if search != "" && len(posts) > 0 {
			ids := make([]string, len(posts))
			for i, p := range posts {
				ids[i] = p.ID.String()
			}
			posts, err = e.popularMatches(c, site, search, ids)
		}
		if len(posts) > limit {
			for _, post := range posts[limit:] {
				remaining = append(remaining, post.ID.String())
			}
			posts = posts[:limit]
		}
	}
	if err != nil {
		return core.PageResult{}, err
	}
	result := core.PageResult{Posts: posts}
	if len(remaining) > 0 {
		data, err := json.Marshal(popularCursor{Site: site.Key, Query: q.query, Identity: c.AuthIdentity(), IDs: remaining})
		if err != nil {
			return core.PageResult{}, err
		}
		result.Next = "popular:" + base64.RawURLEncoding.EncodeToString(data)
		result.HasMore = true
	}
	return result, nil
}

// Let the site's search evaluate tags, aliases, artists and metatags, then
// intersect with the original IDs and restore /popular's exact ordering.
func (e *Engine) popularMatches(c *core.ApiClient, site *core.Site, query string, ids []string) ([]*core.PostInfo, error) {
	posts, err := FetchPostsWithParser(c, site, strings.TrimSpace(query+" id:"+strings.Join(ids, ",")+" order:id_desc"), len(ids), 1, e.ParsePost)
	if err != nil {
		return nil, err
	}
	byID := map[string]*core.PostInfo{}
	for _, post := range posts {
		byID[post.ID.String()] = post
	}
	ordered := make([]*core.PostInfo, 0, len(posts))
	for _, id := range ids {
		if post := byID[id]; post != nil {
			ordered = append(ordered, post)
		}
	}
	return ordered, nil
}
