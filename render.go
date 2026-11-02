package main

import "strings"

const (
	captionLimit     = 1024 // Telegram 媒体 caption 上限（按可见字符的 UTF-16 code unit 计）
	messageTextLimit = 4000 // 普通消息留出余量，低于 Telegram 的 4096
)

var (
	htmlTextEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	htmlAttrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
)

func renderPostCaption(post *Post) string {
	return renderPostHTML(post, captionLimit)
}

// renderPostHTML 生成帖子信息。visibleLimit 按可见字符（UTF-16 code unit）计算，
// 超过时只截断正文，作者行与 Source 始终保留。
func renderPostHTML(post *Post, visibleLimit int) string {
	if post == nil {
		return ""
	}

	handle := strings.TrimSpace(post.AuthorHandle)
	name := strings.TrimSpace(post.AuthorName)
	if name == "" {
		name = handle
	}
	if name == "" {
		name = "原帖作者"
	}

	authorURL := post.CanonicalURL
	authorVisible := name
	if handle != "" {
		authorURL = "https://x.com/" + handle
		authorVisible = name + "(@" + handle + ")"
	}
	authorLine := `<a href="` + escapeHTMLAttr(authorURL) + `">` + escapeHTMLText(authorVisible) + `</a>:`
	sourceLine := `<a href="` + escapeHTMLAttr(post.CanonicalURL) + `">Source</a>`

	body := strings.TrimSpace(post.Text)
	if body == "" {
		return authorLine + "\n\n" + sourceLine
	}

	// 作者行、冒号、两处空行与 Source 的可见长度。
	fixed := utf16Len(authorVisible) + 1 + 4 + utf16Len("Source")
	budget := visibleLimit - fixed
	if budget <= 1 {
		return authorLine + "\n\n" + sourceLine
	}
	if utf16Len(body) > budget {
		body = truncateUTF16(body, budget-1) + "…"
	}
	return authorLine + "\n\n" + escapeHTMLText(body) + "\n\n" + sourceLine
}

func escapeHTMLText(s string) string {
	return htmlTextEscaper.Replace(s)
}

func escapeHTMLAttr(s string) string {
	return htmlAttrEscaper.Replace(s)
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// truncateUTF16 在 rune 边界截断，使结果不超过 max 个 UTF-16 code unit。
func truncateUTF16(s string, max int) string {
	if max <= 0 {
		return ""
	}
	used := 0
	for i, r := range s {
		units := 1
		if r > 0xFFFF {
			units = 2
		}
		if used+units > max {
			return s[:i]
		}
		used += units
	}
	return s
}
