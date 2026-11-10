package core

import "strings"

// HIDE_TEXT_TOKEN is consumed locally, before a query reaches a site engine.
const HIDE_TEXT_TOKEN = "hide:text"

func HideTextFilter() *Filter {
	f := NewBool("hide_text", "Скрыть текст", HIDE_TEXT_TOKEN, false)
	f.Tooltip = "Скрывать посты без вложений. Посты с изображениями, видео, аудио, архивами и другими файлами остаются видимыми."
	return f
}

func SplitHideText(query string) (string, bool) {
	parts, keep, enabled := strings.Fields(query), []string{}, false
	for _, part := range parts {
		if part == HIDE_TEXT_TOKEN {
			enabled = true
		} else {
			keep = append(keep, part)
		}
	}
	return strings.Join(keep, " "), enabled
}

func HasAttachments(p *PostInfo) bool {
	if p == nil {
		return false
	}
	if p.File != nil || p.AttachmentCount > 0 || AsInt(p.Extra["locked_count"]) > 0 {
		return true
	}
	// Older favourites predate AttachmentCount and retain the engine's file lists.
	for _, key := range []string{"files", "media", "attachments"} {
		if files, ok := p.Extra[key].([]any); ok && len(files) > 0 {
			return true
		}
	}
	return false
}

func DropTextPosts(posts []*PostInfo) []*PostInfo {
	kept := make([]*PostInfo, 0, len(posts))
	for _, p := range posts {
		if HasAttachments(p) {
			kept = append(kept, p)
		}
	}
	return preservePostCursor(posts, kept)
}
