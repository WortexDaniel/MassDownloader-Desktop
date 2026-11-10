package library

import (
	"database/sql"
	"fmt"
	"massdownloader/internal/core"
	"massdownloader/internal/history"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// groupIndex resolves author groups by member and by folder.
type groupIndex struct {
	layout    string
	groups    []history.AuthorGroup
	byID      map[string]*history.AuthorGroup
	byMember  map[string]string
	byFolder  map[string]string
	byPrimary map[string]string
}

func loadIndex(q history.Queryer) (*groupIndex, error) {
	groups, err := history.LoadAuthorGroups(q)
	if err != nil {
		return nil, err
	}
	return newIndex(history.AuthorLayout(q), groups), nil
}

func newIndex(layout string, groups []history.AuthorGroup) *groupIndex {
	x := &groupIndex{layout: layout, groups: groups, byID: map[string]*history.AuthorGroup{}, byMember: map[string]string{}, byFolder: map[string]string{}, byPrimary: map[string]string{}}
	for i := range x.groups {
		g := &x.groups[i]
		x.byID[g.ID] = g
		for _, m := range g.Members {
			x.byMember[m.Key()] = g.ID
		}
		x.byFolder[strings.ToLower(g.Folder)] = g.ID
		x.byPrimary[strings.ToLower(g.PrimarySite+"\x00"+g.Folder)] = g.ID
	}
	return x
}

func (x *groupIndex) name(id string) string {
	if g := x.byID[id]; g != nil {
		return g.Name
	}
	return ""
}
func (x *groupIndex) memberGroup(site, author string) string {
	if site == "" || author == "" {
		return ""
	}
	return x.byMember[site+"\x00"+author]
}

// match finds the group whose folder contains the file. base is the folder
// the album structure is relative to; site is the by-site subfolder, if any.
func (x *groupIndex) match(f File) (g *history.AuthorGroup, base, site string) {
	rel, err := filepath.Rel(f.Root, f.Path)
	if err != nil {
		return nil, "", ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 {
		return nil, "", ""
	}
	if x.layout == history.LayoutPrimary {
		g = x.byID[x.byPrimary[strings.ToLower(parts[0]+"\x00"+parts[1])]]
		if g == nil {
			return nil, "", ""
		}
		return g, filepath.Join(f.Root, parts[0], parts[1]), ""
	}
	if !strings.EqualFold(parts[0], history.AuthorsDir) {
		return nil, "", ""
	}
	g = x.byID[x.byFolder[strings.ToLower(parts[1])]]
	if g == nil {
		return nil, "", ""
	}
	base = filepath.Join(f.Root, parts[0], parts[1])
	if x.layout == history.LayoutBySite && len(parts) >= 4 && core.GetSite(parts[2]) != nil {
		return g, filepath.Join(base, parts[2]), parts[2]
	}
	return g, base, ""
}

func (x *groupIndex) groupOf(f File) string {
	if g, _, _ := x.match(f); g != nil {
		return g.ID
	}
	return ""
}

func isMember(g *history.AuthorGroup, site, author string) bool {
	for _, m := range g.Members {
		if m.Site == site && m.Author == author {
			return true
		}
	}
	return false
}

// locate sets the source identity of a file. A file inside a group folder
// keeps a known member identity; otherwise the folder or history names it.
func (x *groupIndex) locate(f *File) {
	g, _, site := x.match(*f)
	if g == nil {
		plainLocation(f)
		return
	}
	if f.Site != "" && isMember(g, f.Site, f.Author) && (site == "" || site == f.Site) {
		return
	}
	if site == "" && len(f.Origins) > 0 {
		site = f.Origins[0].Site
	}
	if site == "" && len(g.Members) > 0 {
		single := true
		for _, m := range g.Members {
			single = single && m.Site == g.Members[0].Site
		}
		if single {
			site = g.Members[0].Site
		}
	}
	f.Site, f.Author = site, ""
	for _, m := range g.Members {
		if m.Site == site {
			f.Author = m.Author
			break
		}
	}
}

// currentBase is the folder that the album structure of a file is kept under.
func (x *groupIndex) currentBase(f File) string {
	if g, base, _ := x.match(f); g != nil {
		return base
	}
	if f.Site != "" && f.Author != "" {
		if base := filepath.Join(f.Root, f.Site, f.Author); within(base, f.Path) {
			return base
		}
	}
	if f.Site != "" {
		if base := filepath.Join(f.Root, f.Site); within(base, f.Path) {
			return base
		}
	}
	return filepath.Dir(f.Path)
}

// folderName turns a display name into a single safe folder name.
func folderName(name string) (string, error) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			r = '_'
		}
		b.WriteRune(r)
	}
	folder := strings.TrimRight(b.String(), ". ")
	for utf8.RuneCountInString(folder) > 100 {
		_, size := utf8.DecodeLastRuneInString(folder)
		folder = strings.TrimRight(folder[:len(folder)-size], ". ")
	}
	if !safeComponent(folder) || strings.EqualFold(folder, history.AuthorsDir) {
		return "", fmt.Errorf("недопустимое название группы")
	}
	return folder, nil
}

type MemberInfo struct {
	history.AuthorMember
	Files int   `json:"files"`
	Size  int64 `json:"size"`
}
type GroupInfo struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Folder      string       `json:"folder"`
	PrimarySite string       `json:"primary_site"`
	Created     int64        `json:"created"`
	Members     []MemberInfo `json:"members"`
	Files       int          `json:"files"`
	Size        int64        `json:"size"`
	// Outside counts member files that are not stored in the group folder.
	Outside int    `json:"outside"`
	Path    string `json:"path"`
	Samples []File `json:"samples"`
}
type AuthorOverview struct {
	Layout string      `json:"layout"`
	Groups []GroupInfo `json:"groups"`
}

func (s *Service) Authors() (AuthorOverview, error) {
	c, err := s.snapshot()
	if err != nil {
		return AuthorOverview{}, err
	}
	out := AuthorOverview{Layout: c.idx.layout, Groups: []GroupInfo{}}
	pos := map[string]int{}
	for _, g := range c.idx.groups {
		info := GroupInfo{ID: g.ID, Name: g.Name, Folder: g.Folder, PrimarySite: g.PrimarySite, Created: g.Created, Members: []MemberInfo{}, Samples: []File{}}
		for _, m := range g.Members {
			info.Members = append(info.Members, MemberInfo{AuthorMember: m})
		}
		pos[g.ID] = len(out.Groups)
		out.Groups = append(out.Groups, info)
	}
	roots := map[string]bool{}
	for _, f := range c.files {
		if f.State != "active" {
			continue
		}
		id := c.idx.memberGroup(f.Site, f.Author)
		if id == "" {
			id = f.Group
		}
		i, ok := pos[id]
		if !ok {
			continue
		}
		info := &out.Groups[i]
		info.Files++
		info.Size += f.Size
		if f.Group != id {
			info.Outside++
		} else if !roots[id] {
			roots[id] = true
			info.Path = history.GroupDir(f.Root, c.idx.layout, *c.idx.byID[id], "")
		}
		for j := range info.Members {
			if info.Members[j].Site == f.Site && info.Members[j].Author == f.Author {
				info.Members[j].Files++
				info.Members[j].Size += f.Size
			}
		}
		if len(info.Samples) < 4 && f.Pixels != "" {
			info.Samples = append(info.Samples, f)
		}
	}
	return out, nil
}

type Identity struct {
	Site      string `json:"site"`
	Author    string `json:"author"`
	Files     int    `json:"files"`
	Group     string `json:"group,omitempty"`
	GroupName string `json:"group_name,omitempty"`
}

// Identities lists source authors for the member picker.
func (s *Service) Identities(search string, limit int) ([]Identity, error) {
	c, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	search = strings.ToLower(strings.TrimSpace(search))
	counts := map[string]*Identity{}
	for _, f := range c.files {
		if f.State != "active" || f.Site == "" || f.Author == "" {
			continue
		}
		key := f.Site + "\x00" + f.Author
		id := counts[key]
		if id == nil {
			id = &Identity{Site: f.Site, Author: f.Author, Group: c.idx.memberGroup(f.Site, f.Author)}
			id.GroupName = c.idx.name(id.Group)
			counts[key] = id
		}
		id.Files++
	}
	out := []Identity{}
	for _, id := range counts {
		if search == "" || strings.Contains(strings.ToLower(id.Author), search) || strings.Contains(strings.ToLower(id.Site+"/"+id.Author), search) {
			out = append(out, *id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Site+"/"+out[i].Author < out[j].Site+"/"+out[j].Author
	})
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return out[:min(limit, len(out))], nil
}

// ---------------------------------------------------------------------------
// Candidates: authors that share identical or near-identical works.

type CandidateOptions struct {
	Mode      string  `json:"mode"`
	Threshold int     `json:"threshold"`
	MinShared int     `json:"min_shared"`
	MinRatio  float64 `json:"min_ratio"`
}
type CandidateMember struct {
	Site      string `json:"site,omitempty"`
	Author    string `json:"author,omitempty"`
	Group     string `json:"group,omitempty"`
	GroupName string `json:"group_name,omitempty"`
	Members   int    `json:"members,omitempty"`
	Files     int    `json:"files"`
	Works     int    `json:"works"`
	Key       string `json:"key"`
}
type CandidatePair struct {
	A      int     `json:"a"`
	B      int     `json:"b"`
	Shared int     `json:"shared"`
	Ratio  float64 `json:"ratio"`
}
type Candidate struct {
	ID      string            `json:"id"`
	Members []CandidateMember `json:"members"`
	Pairs   []CandidatePair   `json:"pairs"`
	Shared  int               `json:"shared"`
	Ratio   float64           `json:"ratio"`
	Samples [][]File          `json:"samples"`
	Name    string            `json:"name"`
}

type unionFind []int

func newUnion(n int) unionFind {
	u := make(unionFind, n)
	for i := range u {
		u[i] = i
	}
	return u
}
func (u unionFind) find(i int) int {
	for u[i] != i {
		u[i] = u[u[i]]
		i = u[i]
	}
	return i
}
func (u unionFind) join(a, b int) { u[u.find(a)] = u.find(b) }

func (s *Service) ignoredPairs(table string) (map[string]bool, error) {
	ignored := map[string]bool{}
	err := s.h.WithDB(func(db *sql.DB) error {
		rows, e := db.Query(`SELECT a,b FROM ` + table)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var a, b string
			if e = rows.Scan(&a, &b); e != nil {
				return e
			}
			ignored[pairKey(a, b)] = true
		}
		return rows.Err()
	})
	return ignored, err
}

// clusterWorks joins files that show the same work. Exact bytes always count;
// "pixels" adds decoded-pixel equality and "similar" adds perceptual hashes.
func clusterWorks(files []File, mode string, threshold int, ignored map[string]bool) unionFind {
	u := newUnion(len(files))
	join := func(a, b int) {
		if !ignored[pairKey(files[a].ID, files[b].ID)] {
			u.join(a, b)
		}
	}
	bySHA, byPixels := map[string]int{}, map[string]int{}
	for i, f := range files {
		if f.SHA != "" {
			key := fmt.Sprintf("%d:%s", f.Size, f.SHA)
			if j, ok := bySHA[key]; ok {
				join(i, j)
			} else {
				bySHA[key] = i
			}
		}
		if mode != "bytes" && f.Pixels != "" {
			if j, ok := byPixels[f.Pixels]; ok {
				join(i, j)
			} else {
				byPixels[f.Pixels] = i
			}
		}
	}
	if mode == "similar" {
		tree := newHashIndex(threshold)
		for i, f := range files {
			if f.Hash != "" && f.Frames <= 1 {
				tree.add(hashOf(f), i)
			}
		}
		for i, f := range files {
			if f.Hash == "" || f.Frames > 1 {
				continue
			}
			for _, j := range tree.find(hashOf(f), threshold) {
				if j > i && visuallyComparable(f, files[j], threshold) {
					join(i, j)
				}
			}
		}
	}
	return u
}

func unitKey(site, author, group string) string {
	if group != "" {
		return "g:" + group
	}
	return "a:" + site + "\x00" + author
}

func (s *Service) Candidates(o CandidateOptions) ([]Candidate, error) {
	c, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if o.Mode != "bytes" && o.Mode != "similar" {
		o.Mode = "pixels"
	}
	o.Threshold = min(max(o.Threshold, 0), 20)
	if o.MinShared < 1 {
		o.MinShared = 2
	}
	if o.MinRatio < 0 || o.MinRatio > 1 {
		o.MinRatio = 0.1
	}
	ignoredFiles, err := s.ignoredPairs("library_ignored")
	if err != nil {
		return nil, err
	}
	ignoredUnits, err := s.ignoredPairs("library_author_ignored")
	if err != nil {
		return nil, err
	}
	files := []File{}
	unitOf := []int{}
	unitIndex := map[string]int{}
	units := []CandidateMember{}
	for _, f := range c.files {
		if f.State != "active" || f.Site == "" || f.Author == "" {
			continue
		}
		group := c.idx.memberGroup(f.Site, f.Author)
		key := unitKey(f.Site, f.Author, group)
		i, ok := unitIndex[key]
		if !ok {
			i = len(units)
			unitIndex[key] = i
			m := CandidateMember{Key: key}
			if group != "" {
				m.Group, m.GroupName, m.Members = group, c.idx.name(group), len(c.idx.byID[group].Members)
			} else {
				m.Site, m.Author = f.Site, f.Author
			}
			units = append(units, m)
		}
		units[i].Files++
		files = append(files, f)
		unitOf = append(unitOf, i)
	}
	u := clusterWorks(files, o.Mode, o.Threshold, ignoredFiles)
	clusters := map[int][]int{}
	for i := range files {
		root := u.find(i)
		clusters[root] = append(clusters[root], i)
	}
	type pairStat struct {
		shared  int
		samples []int
	}
	pairs := map[[2]int]*pairStat{}
	roots := make([]int, 0, len(clusters))
	for root := range clusters {
		roots = append(roots, root)
	}
	sort.Ints(roots)
	for _, root := range roots {
		present := map[int]bool{}
		for _, i := range clusters[root] {
			present[unitOf[i]] = true
		}
		list := make([]int, 0, len(present))
		for unit := range present {
			units[unit].Works++
			list = append(list, unit)
		}
		sort.Ints(list)
		for a := 0; a < len(list); a++ {
			for b := a + 1; b < len(list); b++ {
				key := [2]int{list[a], list[b]}
				p := pairs[key]
				if p == nil {
					p = &pairStat{}
					pairs[key] = p
				}
				p.shared++
				if len(p.samples) < 6 {
					p.samples = append(p.samples, root)
				}
			}
		}
	}
	accepted := newUnion(len(units))
	ratio := func(key [2]int, shared int) float64 {
		return float64(shared) / float64(max(1, min(units[key[0]].Works, units[key[1]].Works)))
	}
	for key, p := range pairs {
		if p.shared >= o.MinShared && ratio(key, p.shared) >= o.MinRatio && !ignoredUnits[pairKey(units[key[0]].Key, units[key[1]].Key)] {
			accepted.join(key[0], key[1])
		}
	}
	components := map[int][]int{}
	for i := range units {
		components[accepted.find(i)] = append(components[accepted.find(i)], i)
	}
	out := []Candidate{}
	for _, list := range components {
		if len(list) < 2 {
			continue
		}
		sort.Slice(list, func(a, b int) bool { return units[list[a]].Files > units[list[b]].Files })
		local := map[int]int{}
		cand := Candidate{Members: []CandidateMember{}, Pairs: []CandidatePair{}, Samples: [][]File{}}
		keys := []string{}
		for i, unit := range list {
			local[unit] = i
			cand.Members = append(cand.Members, units[unit])
			keys = append(keys, units[unit].Key)
		}
		sort.Strings(keys)
		cand.ID = strings.Join(keys, "\x01")
		sampleRoots := []int{}
		seen := map[int]bool{}
		for key, p := range pairs {
			a, okA := local[key[0]]
			b, okB := local[key[1]]
			if !okA || !okB || p.shared < 1 {
				continue
			}
			r := ratio(key, p.shared)
			cand.Pairs = append(cand.Pairs, CandidatePair{A: a, B: b, Shared: p.shared, Ratio: r})
			cand.Ratio = max(cand.Ratio, r)
			for _, root := range p.samples {
				if !seen[root] {
					seen[root] = true
					sampleRoots = append(sampleRoots, root)
				}
			}
		}
		sort.Slice(cand.Pairs, func(i, j int) bool { return cand.Pairs[i].Shared > cand.Pairs[j].Shared })
		// Count works shared by at least two members of the component once.
		for _, root := range roots {
			present := map[int]bool{}
			for _, i := range clusters[root] {
				if _, ok := local[unitOf[i]]; ok {
					present[unitOf[i]] = true
				}
			}
			if len(present) > 1 {
				cand.Shared++
			}
		}
		sort.Ints(sampleRoots)
		for _, root := range sampleRoots[:min(6, len(sampleRoots))] {
			sample, used := []File{}, map[int]bool{}
			for _, i := range clusters[root] {
				if !used[unitOf[i]] && len(sample) < 4 {
					used[unitOf[i]] = true
					sample = append(sample, files[i])
				}
			}
			cand.Samples = append(cand.Samples, sample)
		}
		cand.Name = suggestName(cand.Members)
		out = append(out, cand)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Shared != out[j].Shared {
			return out[i].Shared > out[j].Shared
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// suggestName prefers an existing group, then a readable non-numeric author.
func suggestName(members []CandidateMember) string {
	for _, m := range members {
		if m.GroupName != "" {
			return m.GroupName
		}
	}
	best := ""
	for _, m := range members {
		numeric := strings.Trim(m.Author, "0123456789") == ""
		if best == "" || !numeric && strings.Trim(best, "0123456789") == "" {
			best = m.Author
		}
		if !numeric {
			break
		}
	}
	return strings.TrimSuffix(strings.TrimSuffix(best, "_(artist)"), " (artist)")
}

func (s *Service) IgnoreAuthors(keys []string) error {
	defer s.invalidate()
	if len(keys) < 2 || len(keys) > 50 {
		return fmt.Errorf("выберите от 2 до 50 авторов")
	}
	return s.h.WithDB(func(db *sql.DB) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for i := range keys {
			for j := i + 1; j < len(keys); j++ {
				a, b := keys[i], keys[j]
				if a > b {
					a, b = b, a
				}
				if _, err = tx.Exec(`INSERT OR IGNORE INTO library_author_ignored(a,b) VALUES(?,?)`, a, b); err != nil {
					return err
				}
			}
		}
		return tx.Commit()
	})
}

// ---------------------------------------------------------------------------
// Planning: every author change is a reconciliation of the desired groups and
// layout with the current folders.

type AuthorsRequest struct {
	GroupID     string                 `json:"group_id"`
	Name        string                 `json:"name"`
	Members     []history.AuthorMember `json:"members"`
	Absorb      []string               `json:"absorb,omitempty"`
	Dissolve    bool                   `json:"dissolve,omitempty"`
	Dedupe      bool                   `json:"dedupe"`
	Policy      string                 `json:"policy,omitempty"`
	Preferred   string                 `json:"preferred,omitempty"`
	PrimarySite string                 `json:"primary_site,omitempty"`
	Layout      string                 `json:"layout,omitempty"`
}

// chooseKeeper mirrors the interface: resolution first or source first, then
// the larger file and finally the path for a stable choice.
func chooseKeeper(files []File, preferred, policy string) File {
	best := files[0]
	source := func(f File) int {
		if preferred == "" {
			return 0
		}
		if f.Site == preferred {
			return 1
		}
		for _, o := range f.Origins {
			if o.Site == preferred {
				return 1
			}
		}
		return 0
	}
	better := func(a, b File) bool {
		qa, qb := int64(a.Width)*int64(a.Height), int64(b.Width)*int64(b.Height)
		sa, sb := source(a), source(b)
		if policy == "source" && sa != sb {
			return sa > sb
		}
		if qa != qb {
			return qa > qb
		}
		if sa != sb {
			return sa > sb
		}
		if a.Size != b.Size {
			return a.Size > b.Size
		}
		return a.Path < b.Path
	}
	for _, f := range files[1:] {
		if better(f, best) {
			best = f
		}
	}
	return best
}

func sameGroup(a, b history.AuthorGroup) bool {
	if a.ID != b.ID || a.Name != b.Name || a.Folder != b.Folder || a.PrimarySite != b.PrimarySite || len(a.Members) != len(b.Members) {
		return false
	}
	keys := map[string]bool{}
	for _, m := range a.Members {
		keys[m.Key()] = true
	}
	for _, m := range b.Members {
		if !keys[m.Key()] {
			return false
		}
	}
	return true
}

// checkGroups verifies that the stored groups and layout still match.
func checkGroups(q history.Queryer, expected []history.AuthorGroup, layout string) error {
	groups, err := history.LoadAuthorGroups(q)
	if err != nil {
		return err
	}
	byID := map[string]history.AuthorGroup{}
	for _, g := range groups {
		byID[g.ID] = g
	}
	for _, g := range expected {
		if current, ok := byID[g.ID]; !ok || !sameGroup(current, g) {
			return fmt.Errorf("группы авторов изменились; подготовьте новый план")
		}
	}
	if layout != "" && history.AuthorLayout(q) != layout {
		return fmt.Errorf("структура папок изменилась; подготовьте новый план")
	}
	return nil
}

func writeGroups(tx *sql.Tx, remove, insert []history.AuthorGroup, layout string) error {
	for _, g := range remove {
		if _, err := tx.Exec(`DELETE FROM library_author_members WHERE group_id=?`, g.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM library_author_groups WHERE id=?`, g.ID); err != nil {
			return err
		}
	}
	for _, g := range insert {
		if _, err := tx.Exec(`DELETE FROM library_author_members WHERE group_id=?`, g.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO library_author_groups(id,name,folder,primary_site,created) VALUES(?,?,?,?,?)`, g.ID, g.Name, g.Folder, g.PrimarySite, g.Created); err != nil {
			return err
		}
		for _, m := range g.Members {
			if _, err := tx.Exec(`INSERT INTO library_author_members(group_id,site,author) VALUES(?,?,?)`, g.ID, m.Site, m.Author); err != nil {
				return fmt.Errorf("автор %s/%s уже состоит в другой группе", m.Site, m.Author)
			}
		}
	}
	if layout != "" {
		if _, err := tx.Exec(`INSERT INTO library_meta(key,value) VALUES('author_layout',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, layout); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) planAuthors(r Request, p *Plan, c *catalog) error {
	idx := c.idx
	a := AuthorsRequest{}
	if r.Authors != nil {
		a = *r.Authors
	}
	layoutAfter := idx.layout
	desired := map[string]*history.AuthorGroup{}
	for i := range idx.groups {
		g := idx.groups[i]
		desired[g.ID] = &g
	}
	affectedIDs := map[string]bool{}
	affectedKeys := map[string]bool{}
	before := []history.AuthorGroup{}
	touch := func(g history.AuthorGroup) {
		if affectedIDs[g.ID] {
			return
		}
		affectedIDs[g.ID] = true
		before = append(before, g)
		for _, m := range g.Members {
			affectedKeys[m.Key()] = true
		}
		delete(desired, g.ID)
	}
	var result *history.AuthorGroup
	identityFiles := map[string]int{}
	for _, f := range c.files {
		if f.State == "active" && f.Site != "" && f.Author != "" {
			identityFiles[f.Site+"\x00"+f.Author]++
		}
	}
	switch r.Kind {
	case "layout":
		if !history.ValidLayout(a.Layout) {
			return fmt.Errorf("неизвестная структура папок")
		}
		if a.Layout == idx.layout {
			return fmt.Errorf("эта структура уже используется")
		}
		layoutAfter = a.Layout
		p.Snapshot.Groups = append([]history.AuthorGroup{}, idx.groups...)
		p.Groups = append([]history.AuthorGroup{}, idx.groups...)
		for _, g := range idx.groups {
			affectedIDs[g.ID] = true
			for _, m := range g.Members {
				affectedKeys[m.Key()] = true
			}
		}
	case "authors":
		var old *history.AuthorGroup
		if a.GroupID != "" {
			if old = idx.byID[a.GroupID]; old == nil {
				return fmt.Errorf("группа не найдена; обновите список")
			}
		}
		if a.Dissolve {
			if old == nil {
				return fmt.Errorf("выберите группу")
			}
			touch(*old)
			break
		}
		members, seen := []history.AuthorMember{}, map[string]bool{}
		for _, m := range a.Members {
			m.Site, m.Author = strings.TrimSpace(m.Site), strings.TrimSpace(m.Author)
			if m.Site == "" || m.Author == "" || seen[m.Key()] {
				continue
			}
			seen[m.Key()] = true
			members = append(members, m)
		}
		if len(members) < 2 {
			return fmt.Errorf("в группе должно быть минимум два автора; чтобы убрать группу, распустите её")
		}
		absorb := map[string]bool{}
		for _, id := range a.Absorb {
			if id != a.GroupID && idx.byID[id] != nil {
				absorb[id] = true
			}
		}
		for _, m := range members {
			if gid := idx.byMember[m.Key()]; gid != "" && gid != a.GroupID && !absorb[gid] {
				return fmt.Errorf("автор %s/%s уже в группе «%s»", m.Site, m.Author, idx.name(gid))
			}
		}
		if old != nil {
			touch(*old)
		}
		for id := range absorb {
			touch(*idx.byID[id])
		}
		name := strings.TrimSpace(a.Name)
		folder, err := folderName(name)
		if err != nil {
			return err
		}
		for _, g := range desired {
			if strings.EqualFold(g.Folder, folder) {
				return fmt.Errorf("папка «%s» уже занята группой «%s»", folder, g.Name)
			}
		}
		g := history.AuthorGroup{ID: a.GroupID, Name: name, Folder: folder, Members: members, Created: time.Now().Unix()}
		if old != nil {
			g.Created = old.Created
		} else {
			g.ID = newID()
		}
		sort.Slice(g.Members, func(i, j int) bool { return g.Members[i].Key() < g.Members[j].Key() })
		primary := strings.TrimSpace(a.PrimarySite)
		if primary == "" && old != nil {
			primary = old.PrimarySite
		}
		valid := false
		for _, m := range members {
			valid = valid || m.Site == primary
		}
		if !valid {
			bySite := map[string]int{}
			for _, m := range members {
				bySite[m.Site] += identityFiles[m.Key()]
			}
			primary = ""
			for _, m := range members {
				if primary == "" || bySite[m.Site] > bySite[primary] || bySite[m.Site] == bySite[primary] && m.Site < primary {
					primary = m.Site
				}
			}
		}
		g.PrimarySite = primary
		result = &g
		desired[g.ID] = result
		for _, m := range members {
			affectedKeys[m.Key()] = true
		}
		affectedIDs[g.ID] = true
		p.Snapshot.Groups = before
		p.Groups = []history.AuthorGroup{g}
	default:
		return fmt.Errorf("неизвестная операция")
	}
	if r.Kind == "authors" && result == nil {
		p.Snapshot.Groups = before
	}
	p.Snapshot.Layout = idx.layout
	p.Layout = layoutAfter
	after := newIndex(layoutAfter, func() []history.AuthorGroup {
		list := []history.AuthorGroup{}
		for _, g := range desired {
			list = append(list, *g)
		}
		return list
	}())
	// In the primary layout the group folder sits next to ordinary authors of
	// that site and must not take over one of them.
	if layoutAfter == history.LayoutPrimary {
		for _, g := range after.groups {
			if !affectedIDs[g.ID] && r.Kind != "layout" {
				continue
			}
			for key := range identityFiles {
				site, author, _ := strings.Cut(key, "\x00")
				if site == g.PrimarySite && strings.EqualFold(author, g.Folder) && !isMember(&g, site, author) {
					return fmt.Errorf("папка %s/%s уже принадлежит другому автору; выберите другое название", site, author)
				}
			}
		}
	}
	reserved := map[string]bool{}
	for _, f := range c.files {
		reserved[key(f.Path)] = true
	}
	// Exact duplicates inside the merged group are removed in the same plan.
	removed := map[string]File{}
	if result != nil && a.Dedupe {
		ignored, err := s.ignoredPairs("library_ignored")
		if err != nil {
			return err
		}
		pool := []File{}
		for _, f := range c.files {
			if f.State == "active" && after.byMember[f.Site+"\x00"+f.Author] == result.ID {
				pool = append(pool, f)
			}
		}
		u := clusterWorks(pool, "pixels", 0, ignored)
		clusters := map[int][]File{}
		for i := range pool {
			clusters[u.find(i)] = append(clusters[u.find(i)], pool[i])
		}
		for _, list := range clusters {
			if len(list) < 2 {
				continue
			}
			keeper := chooseKeeper(list, a.Preferred, a.Policy)
			for _, f := range list {
				if f.ID != keeper.ID && !ignored[pairKey(f.ID, keeper.ID)] {
					removed[f.ID] = keeper
				}
			}
		}
	}
	targets := map[string]string{}
	for _, f := range c.files {
		if f.State != "active" || removed[f.ID].ID != "" {
			continue
		}
		identity := f.Site + "\x00" + f.Author
		involved := affectedKeys[identity] || f.Group != "" && affectedIDs[f.Group]
		if !involved {
			continue
		}
		base := idx.currentBase(f)
		rel, err := filepath.Rel(base, f.Path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			rel = filepath.Base(f.Path)
		}
		var dir string
		if g := after.byID[after.byMember[identity]]; g != nil && f.Author != "" {
			dir = history.GroupDir(f.Root, layoutAfter, *g, f.Site)
		} else if f.Site != "" && f.Author != "" {
			dir = filepath.Join(f.Root, f.Site, f.Author)
		} else {
			// The source of this file is unknown; it stays where it is.
			continue
		}
		target := filepath.Join(dir, rel)
		if same(target, f.Path) {
			continue
		}
		path, err := uniquePath(target, f.ID, reserved)
		if err != nil {
			return err
		}
		targets[f.ID] = path
		change := Change{Before: f, After: f}
		change.After.Version++
		change.After.Path = path
		change.After.Group = ""
		p.Changes = append(p.Changes, change)
	}
	for _, f := range c.files {
		keeper := removed[f.ID]
		if keeper.ID == "" {
			continue
		}
		change := Change{Before: f, After: f, Backup: quarantine(f, p.ID)}
		change.After.Version++
		change.After.State = "quarantined"
		change.After.Group = ""
		k := keeper
		change.Keep = &k
		change.KeepPath = keeper.Path
		if path := targets[keeper.ID]; path != "" {
			change.KeepPath = path
		}
		p.Changes = append(p.Changes, change)
	}
	sort.SliceStable(p.Changes, func(i, j int) bool {
		return (p.Changes[i].Keep == nil) && (p.Changes[j].Keep != nil)
	})
	return nil
}
