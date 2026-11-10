package core

import "fmt"

// PageResult retains the raw cursor even when local filters discard every post.
type PageResult struct {
	Posts   []*PostInfo
	Next    any
	HasMore bool
}
type PageEngine interface {
	FetchPage(*ApiClient, *Site, string, int, any) (PageResult, error)
}

func FetchEnginePage(c *ApiClient, e Engine, s *Site, q string, limit int, cursor any) (PageResult, error) {
	q, hideText := SplitHideText(q)
	var result PageResult
	var err error
	if paged, ok := e.(PageEngine); ok {
		result, err = paged.FetchPage(c, s, q, limit, cursor)
	} else {
		result.Posts, err = e.FetchPosts(c, s, q, limit, cursor)
		if err == nil && len(result.Posts) > 0 {
			result.Next = e.NextCursor(q, result.Posts, cursor)
			result.HasMore = result.Next != nil && fmt.Sprint(result.Next) != "" && fmt.Sprint(result.Next) != fmt.Sprint(cursor)
		}
	}
	if err != nil {
		return PageResult{}, err
	}
	if hideText {
		result.Posts = DropTextPosts(result.Posts)
	}
	return result, nil
}
