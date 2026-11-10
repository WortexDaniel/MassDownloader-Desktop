package favorites

import "encoding/json"

// ViewAuthorPosts acknowledges only loaded, visible posts. Opening an author
// or prefetching a page must not acknowledge any other unread IDs.
func (f *Favorites) ViewAuthorPosts(site, query string, postIDs []any) (bool, error) {
	row := f.authors[akey(site, query)]
	if row == nil {
		return false, nil
	}
	state := newAuthorState(stateToAny(row.State))
	before, _ := json.Marshal(state)
	viewed, known := map[string]bool{}, map[string]bool{}
	for _, item := range state.KnownIDs {
		known[normalizeID(item).Key()] = true
	}
	alreadyViewed := map[string]bool{}
	for _, item := range state.ViewedIDs {
		alreadyViewed[normalizeID(item).Key()] = true
	}
	for _, item := range postIDs {
		id := normalizeID(item)
		if id.String() == "" || viewed[id.Key()] {
			continue
		}
		viewed[id.Key()] = true
		if !alreadyViewed[id.Key()] {
			state.ViewedIDs = append(state.ViewedIDs, packID(id))
			alreadyViewed[id.Key()] = true
		}
		if !known[id.Key()] {
			state.KnownIDs = append(state.KnownIDs, packID(id))
			known[id.Key()] = true
		}
	}
	if len(viewed) == 0 {
		return false, nil
	}
	remaining := []any{}
	for _, item := range state.UnreadIDs {
		if !viewed[normalizeID(item).Key()] {
			remaining = append(remaining, item)
		}
	}
	state.UnreadIDs = remaining
	// Leave a new subscription uninitialized until its first baseline check.
	if len(state.KnownIDs) > authorKnownLimit {
		state.KnownIDs = state.KnownIDs[:authorKnownLimit]
	}
	after, _ := json.Marshal(state)
	if string(before) == string(after) {
		return false, nil
	}
	if err := f.writeAuthorState(site, query, state); err != nil {
		return false, err
	}
	row.State = state
	return true, nil
}
