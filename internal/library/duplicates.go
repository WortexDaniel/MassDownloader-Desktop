package library

import (
	"fmt"
	"math"
	"math/bits"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Group struct {
	ID        string         `json:"id"`
	Mode      string         `json:"mode"`
	Files     []File         `json:"files"`
	Distances map[string]int `json:"distances"`
	Reason    string         `json:"reason"`
}
type GroupPage struct {
	Groups []Group `json:"groups"`
	Total  int     `json:"total"`
}

// hashIndex finds perceptual hashes within a Hamming distance using the
// pigeonhole principle: with threshold+1 chunks at least one chunk is equal.
type hashIndex struct {
	bounds  [][2]uint
	buckets []map[uint64][]int32
	hashes  []uint64
	present []bool
	stamp   []uint32
	round   uint32
}

func newHashIndex(threshold int) *hashIndex {
	n := min(max(threshold, 0), 20) + 1
	x := &hashIndex{}
	start := uint(0)
	for i := 0; i < n; i++ {
		width := uint(64 / n)
		if i < 64%n {
			width++
		}
		x.bounds = append(x.bounds, [2]uint{start, width})
		x.buckets = append(x.buckets, map[uint64][]int32{})
		start += width
	}
	return x
}
func (x *hashIndex) chunk(hash uint64, i int) uint64 {
	b := x.bounds[i]
	return (hash >> b[0]) & (1<<b[1] - 1)
}
func (x *hashIndex) add(hash uint64, index int) {
	for len(x.hashes) <= index {
		x.hashes = append(x.hashes, 0)
		x.present = append(x.present, false)
		x.stamp = append(x.stamp, 0)
	}
	x.hashes[index], x.present[index] = hash, true
	for i := range x.bounds {
		c := x.chunk(hash, i)
		x.buckets[i][c] = append(x.buckets[i][c], int32(index))
	}
}

// find returns every indexed entry within the threshold, in ascending order.
func (x *hashIndex) find(hash uint64, threshold int) []int {
	x.round++
	out := []int{}
	for i := range x.bounds {
		for _, j := range x.buckets[i][x.chunk(hash, i)] {
			if x.stamp[j] != x.round {
				x.stamp[j] = x.round
				if bits.OnesCount64(x.hashes[j]^hash) <= threshold {
					out = append(out, int(j))
				}
			}
		}
	}
	sort.Ints(out)
	return out
}

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}
func hashOf(f File) uint64 { v, _ := strconv.ParseUint(f.Hash, 16, 64); return v }
func visuallyComparable(a, b File, threshold int) bool {
	if a.Hash == "" || b.Hash == "" || a.Frames > 1 || b.Frames > 1 || a.Width <= 0 || b.Width <= 0 {
		return false
	}
	if bits.OnesCount64(hashOf(a)^hashOf(b)) > threshold {
		return false
	}
	ra, rb := float64(a.Width)/float64(a.Height), float64(b.Width)/float64(b.Height)
	if math.Abs(math.Log(ra/rb)) > .07 {
		return false
	}
	// A grayscale hash alone would merge differently colored variants. Such
	// candidates can still be reviewed by raising the distance in the UI.
	if len(a.Color) == 4 && len(b.Color) == 4 {
		sum := 0
		for i := 0; i < 4; i++ {
			sum += int(math.Abs(float64(a.Color[i] - b.Color[i])))
		}
		if sum > 100 {
			return false
		}
	}
	return true
}
func (s *Service) Duplicates(q Query) (GroupPage, error) {
	groups, err := s.duplicateGroups(q)
	if err != nil {
		return GroupPage{}, err
	}
	total := len(groups)
	start := min(max(q.Offset, 0), total)
	limit := min(max(q.Limit, 1), 100)
	return GroupPage{Groups: groups[start:min(total, start+limit)], Total: total}, nil
}

// duplicateGroups returns every group for the query. Results are cached until
// the catalog changes; paging does not repeat a perceptual search.
func (s *Service) duplicateGroups(q Query) ([]Group, error) {
	c, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	q.Threshold = min(max(q.Threshold, 0), 20)
	cacheKey := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s", q.Mode, q.Threshold, q.Site, q.Author, q.Group, q.Search, q.Format)
	s.cacheMu.Lock()
	if hit, ok := s.dupes[cacheKey]; ok && hit.gen == c.gen && hit.rev == c.rev {
		s.cacheMu.Unlock()
		return hit.groups, nil
	}
	s.cacheMu.Unlock()
	q.Sort = ""
	files := filtered(c.files, q, c.idx)
	ignored, err := s.ignoredPairs("library_ignored")
	if err != nil {
		return nil, err
	}
	groups := []Group{}
	threshold := min(max(q.Threshold, 0), 20)
	if q.Mode == "similar" {
		tree := newHashIndex(threshold)
		for i, f := range files {
			if f.Hash != "" && f.Frames <= 1 {
				tree.add(hashOf(f), i)
			}
		}
		used := map[int]bool{}
		for i, anchor := range files {
			if used[i] || anchor.Hash == "" {
				continue
			}
			candidates := tree.find(hashOf(anchor), threshold)
			g := Group{ID: anchor.ID, Mode: q.Mode, Files: []File{anchor}, Distances: map[string]int{anchor.ID: 0}, Reason: "перцептивное сходство; сравните версии перед заменой"}
			matched := []int{i}
			for _, j := range candidates {
				if j <= i || used[j] || ignored[pairKey(anchor.ID, files[j].ID)] || !visuallyComparable(anchor, files[j], threshold) {
					continue
				}
				g.Files = append(g.Files, files[j])
				matched = append(matched, j)
				g.Distances[files[j].ID] = bits.OnesCount64(hashOf(anchor) ^ hashOf(files[j]))
			}
			if len(g.Files) > 1 {
				groups = append(groups, g)
				for _, j := range matched {
					used[j] = true
				}
			}
		}
	} else {
		buckets := map[string][]File{}
		keys := []string{}
		for _, f := range files {
			value := ""
			switch q.Mode {
			case "name":
				value = strings.ToLower(strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path)))
			case "bytes":
				if f.SHA != "" {
					value = fmt.Sprintf("%d:%s", f.Size, f.SHA)
				}
			case "pixels":
				value = f.Pixels
			default:
				return nil, fmt.Errorf("неизвестный режим поиска дублей")
			}
			if value == "" {
				continue
			}
			if _, ok := buckets[value]; !ok {
				keys = append(keys, value)
			}
			buckets[value] = append(buckets[value], f)
		}
		sort.Strings(keys)
		for _, k := range keys {
			bucket := buckets[k]
			if len(bucket) < 2 {
				continue
			}
			group := []File{bucket[0]}
			for _, f := range bucket[1:] {
				if !ignored[pairKey(bucket[0].ID, f.ID)] {
					group = append(group, f)
				}
			}
			if len(group) < 2 {
				continue
			}
			reason := map[string]string{"name": "одинаковое имя без расширения", "bytes": "совпадают размер и SHA-256; при удалении проверяются байты", "pixels": "совпадают пиксели, ориентация и ICC-профиль; метаданные могут отличаться"}[q.Mode]
			groups = append(groups, Group{ID: group[0].ID, Mode: q.Mode, Files: group, Distances: map[string]int{}, Reason: reason})
		}
	}
	s.cacheMu.Lock()
	if s.gen == c.gen {
		if s.dupes == nil {
			s.dupes = map[string]dupeCache{}
		}
		s.dupes[cacheKey] = dupeCache{gen: c.gen, rev: c.rev, groups: groups}
	}
	s.cacheMu.Unlock()
	return groups, nil
}
