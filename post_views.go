package main

import "massdownloader/internal/core"

// MarkPostViewed is shared by the main window and the feed's parent-process
// bridge. Source queries preserve authorship when an API omits artist metadata.
func (a *App) MarkPostViewed(post *core.PostInfo, sourceQueries []string) (bool, error) {
	if post == nil || post.ID.String() == "" {
		return false, nil
	}
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	queries := map[string]bool{}
	hints := []string{}
	identities := map[string][]string{}
	for _, author := range a.favorites.Authors() {
		if author["site"] == post.Site {
			hints = append(hints, author["name"].(string))
			query := author["query"].(string)
			identity := core.AuthorIdentity(post.Site, query)
			identities[identity] = append(identities[identity], query)
		}
	}
	match := func(query string) {
		for _, subscription := range identities[core.AuthorIdentity(post.Site, query)] {
			queries[subscription] = true
		}
	}
	for _, query := range sourceQueries {
		match(query)
	}
	if engine := core.Registry.EngineFor(core.GetSite(post.Site)); engine != nil {
		for _, artist := range append(core.PostArtists(post, hints), post.Artists...) {
			match(engine.ArtistQuery(artist, post))
		}
	}
	changed := false
	for query := range queries {
		on, err := a.favorites.ViewAuthorPosts(post.Site, query, []any{packIDAny(post.ID)})
		changed = changed || on
		if err != nil {
			if changed {
				a.emit("authors:changed")
			}
			return changed, err
		}
	}
	if changed {
		a.emit("authors:changed")
	}
	return changed, nil
}

func (a *App) MarkAllAuthorViewed(site, query string) (bool, error) {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	changed, err := a.favorites.ClearAuthorUnread(site, query)
	if changed {
		a.emit("authors:changed")
	}
	return changed, err
}
