package pixiv

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"massdownloader/internal/core"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type workEntry struct{ wid, uid int64 }
type userSnapshot struct {
	entries         []workEntry
	identity, query string
	used            time.Time
}

func (e *Engine) FetchPage(c *core.ApiClient, s *core.Site, tags string, limit int, cursor any) (core.PageResult, error) {
	q := parseQuery(tags)
	if len(q.userIDs) == 0 {
		return e.fetchSearchPage(c, s, tags, limit, cursor)
	}
	queryData, _ := json.Marshal(q.userIDs)
	query := string(queryData) + "|" + tags
	page := int64(1)
	token := ""
	if text, ok := cursor.(string); ok && strings.HasPrefix(text, "px:") {
		parts := strings.Split(text, ":")
		if len(parts) != 3 {
			return core.PageResult{}, fmt.Errorf("Pixiv: некорректный курсор")
		}
		token = parts[1]
		var err error
		page, err = strconv.ParseInt(parts[2], 10, 64)
		if err != nil || page < 1 {
			return core.PageResult{}, fmt.Errorf("Pixiv: некорректная страница")
		}
	} else if n := core.AsInt(cursor); n > 0 {
		page = n
	}
	var snapshot userSnapshot
	if token != "" {
		e.mu.Lock()
		var ok bool
		snapshot, ok = e.snapshots[token]
		if ok && time.Since(snapshot.used) < 30*time.Minute && snapshot.identity == c.AuthIdentity() && snapshot.query == query {
			snapshot.used = time.Now()
			e.snapshots[token] = snapshot
		} else {
			ok = false
		}
		e.mu.Unlock()
		if !ok {
			return core.PageResult{}, fmt.Errorf("Pixiv: снимок списка работ истёк или изменилась авторизация; обновите поиск")
		}
	} else {
		entries, err := e.userEntries(c, s, q.userIDs)
		if err != nil {
			return core.PageResult{}, err
		}
		snapshot = userSnapshot{entries: entries, identity: c.AuthIdentity(), query: query, used: time.Now()}
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return core.PageResult{}, err
		}
		token = hex.EncodeToString(b)
		e.mu.Lock()
		if e.snapshots == nil {
			e.snapshots = map[string]userSnapshot{}
		}
		for k, v := range e.snapshots {
			if time.Since(v.used) >= 30*time.Minute {
				delete(e.snapshots, k)
			}
		}
		entryCount := len(entries)
		for _, active := range e.snapshots {
			entryCount += len(active.entries)
		}
		if len(e.snapshots) >= 128 || entryCount > 1000000 {
			e.mu.Unlock()
			return core.PageResult{}, fmt.Errorf("Pixiv: слишком много активных списков работ; повторите позднее")
		}
		e.snapshots[token] = snapshot
		e.mu.Unlock()
	}
	if page > int64(len(snapshot.entries))/userWorkBatch+1 {
		return core.PageResult{}, nil
	}
	start := (page - 1) * userWorkBatch
	if start >= int64(len(snapshot.entries)) {
		return core.PageResult{}, nil
	}
	end := min(start+userWorkBatch, int64(len(snapshot.entries)))
	entries := snapshot.entries[start:end]
	byUser := map[int64][]int64{}
	for _, entry := range entries {
		byUser[entry.uid] = append(byUser[entry.uid], entry.wid)
	}
	works := map[string]map[string]any{}
	for uid, wids := range byUser {
		meta, err := e.userWorksMetaStrict(c, s, uid, wids)
		if err != nil {
			return core.PageResult{}, err
		}
		for k, v := range meta {
			works[k] = v
		}
	}
	out := []*core.PostInfo{}
	for _, entry := range entries {
		it, ok := works[strconv.FormatInt(entry.wid, 10)]
		if !ok {
			continue
		}
		if !modeOK(q.mode, it["xRestrict"]) {
			continue
		}
		p, err := e.build(c, s, it, q.op, q.threshold, q.hasThresh)
		if err != nil {
			return core.PageResult{}, err
		}
		if p != nil {
			out = append(out, p)
		}
	}
	result := core.PageResult{Posts: out, HasMore: end < int64(len(snapshot.entries))}
	if result.HasMore {
		result.Next = fmt.Sprintf("px:%s:%d", token, page+1)
	}
	for _, p := range out {
		if p.Extra == nil {
			p.Extra = map[string]any{}
		}
		p.Extra["_pixiv_next"] = result.Next
	}
	return result, nil
}
func (e *Engine) userEntries(c *core.ApiClient, s *core.Site, uids []int64) ([]workEntry, error) {
	entries := []workEntry{}
	seen := map[int64]bool{}
	for _, uid := range uids {
		if uid == 0 || seen[uid] {
			continue
		}
		seen[uid] = true
		data, err := c.GetJSON(nil, fmt.Sprintf("%s/ajax/user/%d/profile/all", s.Base(), uid), url.Values{"lang": {"en"}}, e.headers(c, s))
		if err != nil {
			return nil, err
		}
		m, ok := data.(map[string]any)
		if !ok || m["error"] == true {
			return nil, fmt.Errorf("Pixiv: не удалось получить список работ автора")
		}
		body, ok := m["body"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Pixiv: отсутствует список работ автора")
		}
		if body["illusts"] == nil && body["manga"] == nil {
			return nil, fmt.Errorf("Pixiv: некорректный список работ автора")
		}
		for _, wid := range userWorkIDs(body) {
			entries = append(entries, workEntry{wid, uid})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].wid > entries[j].wid })
	return entries, nil
}
func (e *Engine) userWorksMetaStrict(c *core.ApiClient, s *core.Site, uid int64, ids []int64) (map[string]map[string]any, error) {
	q := url.Values{"work_category": {"illustManga"}, "is_first_page": {"0"}, "lang": {"en"}}
	for _, id := range ids {
		q.Add("ids[]", strconv.FormatInt(id, 10))
	}
	data, err := c.GetJSON(nil, fmt.Sprintf("%s/ajax/user/%d/profile/illusts", s.Base(), uid), q, e.headers(c, s))
	if err != nil {
		return nil, err
	}
	m, ok := data.(map[string]any)
	if !ok || m["error"] == true {
		return nil, fmt.Errorf("Pixiv: ошибка получения работ автора")
	}
	body, ok := m["body"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Pixiv: отсутствуют данные работ автора")
	}
	raw, ok := body["works"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Pixiv: отсутствует список works")
	}
	out := map[string]map[string]any{}
	for k, v := range raw {
		if item, ok := v.(map[string]any); ok {
			out[k] = item
		}
	}
	return out, nil
}
func (e *Engine) NextCursor(_ string, batch []*core.PostInfo, cursor any) any {
	if len(batch) > 0 {
		if next, ok := batch[len(batch)-1].Extra["_pixiv_next"]; ok {
			return next
		}
	}
	return e.EngineBase.NextCursor("", batch, cursor)
}
