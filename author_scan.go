package main

import (
	"fmt"
	"massdownloader/internal/core"
	"massdownloader/internal/favorites"
	"strings"
)

type orderedAuthorEngine interface{ AuthorScanDescending(string) bool }
type authorScanResult struct {
	ids            []any
	complete, full bool
}

func scanAuthor(c *core.ApiClient, site *core.Site, query string, limit int, state favorites.AuthorState, identity string) (authorScanResult, error) {
	e := core.Registry.EngineFor(site)
	if e == nil {
		return authorScanResult{}, fmt.Errorf("unknown engine")
	}
	validationQuery, _ := core.SplitHideText(query)
	if err := e.ValidateQuery(validationQuery); err != nil {
		return authorScanResult{}, err
	}
	clean, imagesOnly := core.SplitImagesOnly(query)
	ordered, ok := e.(orderedAuthorEngine)
	// Ordered feeds need only reach a previously checked post. Validate the
	// entire page first so pinned/reordered posts cannot hide newer IDs. No periodic
	// 2000-post reconciliation is necessary for stable chronological queries.
	descending := ok && ordered.AuthorScanDescending(clean)
	incremental := descending && state.Initialized
	result := authorScanResult{full: !incremental}
	known := map[string]bool{}
	checked := state.CheckedIDs
	if len(checked) == 0 {
		// Older installations have no explicit checkpoint. Their saved baseline
		// and unread posts came from earlier scans; new exact views are excluded.
		viewed := map[string]bool{}
		for _, id := range state.ViewedIDs {
			viewed[core.NormalizePostId(id).Key()] = true
		}
		for _, id := range append(append([]any{}, state.KnownIDs...), state.UnreadIDs...) {
			if !viewed[core.NormalizePostId(id).Key()] {
				checked = append(checked, id)
			}
		}
	}
	for _, id := range checked {
		known[core.NormalizePostId(id).Key()] = true
	}
	cursor := any(1)
	cursors, seen := map[string]bool{}, map[string]bool{}
	var previous int64
	for {
		if c.Context().Err() != nil {
			return result, core.DownloadCancelled
		}
		key := fmt.Sprint(cursor)
		if cursors[key] {
			return result, fmt.Errorf("источник повторил курсор при проверке автора")
		}
		cursors[key] = true
		page, err := core.FetchEnginePage(c, e, site, clean, site.PageLimit, cursor)
		batch := page.Posts
		if err != nil {
			return result, err
		}
		if c.Context().Err() != nil {
			return result, core.DownloadCancelled
		}
		if len(batch) == 0 && !page.HasMore {
			result.complete = true
			return result, nil
		}
		next := page.Next
		hasKnown := false
		for _, p := range batch {
			if p == nil || p.ID.String() == "" {
				return result, fmt.Errorf("некорректный ID в ответе автора")
			}
			if descending && (!p.ID.IsNu || p.ID.Num <= 0 || previous > 0 && p.ID.Num >= previous) {
				descending = false
				incremental = false
				result.full = true
			}
			previous = p.ID.Num
		}
		for _, p := range batch {
			if known[p.ID.Key()] {
				hasKnown = true
			}
			if seen[p.ID.Key()] {
				continue
			}
			seen[p.ID.Key()] = true
			if imagesOnly && p.File != nil && core.ANIM_EXTS[p.File.Ext] {
				continue
			}
			// Historical unknown IDs below the overlap are outside the initial
			// baseline, not new publications. Preserve all newer IDs on the page.
			if incremental && hasKnown && !known[p.ID.Key()] {
				continue
			}
			if limit > 0 && len(result.ids) >= limit {
				return result, nil
			}
			result.ids = append(result.ids, packIDAny(p.ID))
		}
		// A single-page baseline is sufficient only for feeds whose stable
		// descending order lets future checks stop at that same overlap.
		// Other sources need the same bounded pass on their very first check.
		if descending && len(result.ids) > 0 && !state.Initialized || incremental && hasKnown {
			result.complete = true
			return result, nil
		}
		if next == nil || strings.TrimSpace(fmt.Sprint(next)) == "" {
			result.complete = true
			return result, nil
		}
		if fmt.Sprint(next) == key {
			return result, fmt.Errorf("источник не продвинул курсор автора")
		}
		if limit > 0 && len(result.ids) >= limit {
			return result, nil
		}
		cursor = next
	}
}

// batchAuthorEngine searches several authors with one OR query ("~a ~b
// id:>N"). Only engines with stable descending IDs and real OR support opt in.
type batchAuthorEngine interface {
	AuthorBatchSize() int
}

type authorProbeItem struct {
	query, tag string
	maxID      int64
	checked    []any
}

// authorProbeTag returns the single plain tag an author query consists of,
// or "" when the query needs its own scan (metatags, several tags, OR/NOT).
func authorProbeTag(query string) string {
	clean, _ := core.SplitHideText(query)
	clean, _ = core.SplitImagesOnly(clean)
	fields := strings.Fields(clean)
	if len(fields) != 1 {
		return ""
	}
	tag := strings.ToLower(fields[0])
	if strings.ContainsAny(tag, ":*{}~") || strings.HasPrefix(tag, "-") {
		return ""
	}
	return tag
}

// checkpointIDs mirrors scanAuthor's stopping set for a stored state.
func checkpointIDs(state favorites.AuthorState) []any {
	if len(state.CheckedIDs) > 0 {
		return state.CheckedIDs
	}
	viewed := map[string]bool{}
	for _, id := range state.ViewedIDs {
		viewed[core.NormalizePostId(id).Key()] = true
	}
	var out []any
	for _, id := range append(append([]any{}, state.KnownIDs...), state.UnreadIDs...) {
		if !viewed[core.NormalizePostId(id).Key()] {
			out = append(out, id)
		}
	}
	return out
}

func maxNumericID(ids []any) int64 {
	var best int64
	for _, id := range ids {
		if p := core.NormalizePostId(id); p.IsNu && p.Num > best {
			best = p.Num
		}
	}
	return best
}

// probeAuthorBatch asks once whether any of items has a post newer than its
// checkpoint. It returns the authors proven to have nothing new; everyone
// else needs the normal per-author scan. Unattributed posts (tag aliases)
// and a truncated page only ever move authors to the per-author scan.
func probeAuthorBatch(c *core.ApiClient, e core.Engine, site *core.Site, items []authorProbeItem) (map[string]bool, error) {
	if len(items) == 0 {
		return map[string]bool{}, nil
	}
	minID := items[0].maxID
	tags := make([]string, 0, len(items)+1)
	for _, it := range items {
		minID = min(minID, it.maxID)
		tags = append(tags, "~"+it.tag)
	}
	if len(items) == 1 {
		tags[0] = items[0].tag
	}
	tags = append(tags, fmt.Sprintf("id:>%d", minID))
	query := strings.Join(tags, " ")
	limit := site.PageLimit
	page, err := core.FetchEnginePage(c, e, site, query, limit, 1)
	if err != nil {
		return nil, err
	}
	dirty := map[string]bool{}
	var lowest, unattributed int64
	for _, p := range page.Posts {
		if p == nil || !p.ID.IsNu {
			return nil, fmt.Errorf("пакетная проверка: нечисловой ID")
		}
		if lowest == 0 || p.ID.Num < lowest {
			lowest = p.ID.Num
		}
		tagSet := map[string]bool{}
		for _, t := range p.Tags {
			tagSet[strings.ToLower(t)] = true
		}
		for _, t := range p.Artists {
			tagSet[strings.ToLower(t)] = true
		}
		matched := false
		for _, it := range items {
			if tagSet[it.tag] {
				matched = true
				if p.ID.Num > it.maxID {
					dirty[it.query] = true
				}
			}
		}
		if !matched {
			unattributed = max(unattributed, p.ID.Num)
		}
	}
	truncated := len(page.Posts) >= limit || page.HasMore
	clean := map[string]bool{}
	for _, it := range items {
		if dirty[it.query] || it.maxID < unattributed || (truncated && it.maxID < lowest) {
			continue
		}
		clean[it.query] = true
	}
	return clean, nil
}
