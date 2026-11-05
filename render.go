package main

import (
	"strings"
	"unicode"
)

const (
	captionLimit     = 1024 // Telegram 媒体 caption 上限（按可见字符的 UTF-16 code unit 计）
	messageTextLimit = 4000 // 普通消息留出余量，低于 Telegram 的 4096

	minOriginalTextLimit = 16 // 译文之后剩余空间太小时不再输出原文折叠块
)

var (
	htmlTextEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	htmlAttrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
)

func renderPostCaption(post *Post) string {
	return renderPostHTML(post, captionLimit)
}

// renderPostHTML 生成帖子信息。有译文时以译文为正文、原文放进可折叠引用。
// visibleLimit 按可见字符（UTF-16 code unit）计算，作者行与 Source 始终保留。
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
	if tr := post.Translation; tr != nil {
		if translated := strings.TrimSpace(tr.Text); translated != "" && translated != body {
			return renderTranslatedPost(post, authorLine, sourceLine, authorVisible, translated, body, visibleLimit)
		}
	}
	return renderPlainPost(authorLine, sourceLine, authorVisible, body, visibleLimit)
}

// renderPlainPost 是原始格式：作者行、正文、Source，超长时只截断正文。
func renderPlainPost(authorLine, sourceLine, authorVisible, body string, visibleLimit int) string {
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

// renderTranslatedPost 以译文为正文，原文放进可折叠引用；超长时优先截断原文。
func renderTranslatedPost(post *Post, authorLine, sourceLine, authorVisible, translated, original string, visibleLimit int) string {
	label := formatTranslationLabel(post.Translation)

	// 作者行、冒号、标注行与 Source 的可见长度；每处空行占 2 个字符。
	base := utf16Len(authorVisible) + 1 + utf16Len(label) + utf16Len("Source")
	transBudget := visibleLimit - base - 6 // 三处空行：作者行 / 标注 / 译文 / Source
	if transBudget <= 1 {
		return renderPlainPost(authorLine, sourceLine, authorVisible, original, visibleLimit)
	}

	translatedOnly := authorLine + "\n\n" + escapeHTMLText(label) + "\n\n" +
		escapeHTMLText(translated) + "\n\n" + sourceLine
	if utf16Len(translated) > transBudget {
		translated = truncateUTF16(translated, transBudget-1) + "…"
		return authorLine + "\n\n" + escapeHTMLText(label) + "\n\n" +
			escapeHTMLText(translated) + "\n\n" + sourceLine
	}

	fullBudget := visibleLimit - base - 8 // 四处空行：多出原文折叠块
	remaining := fullBudget - utf16Len(translated)
	if original == "" || remaining < minOriginalTextLimit {
		return translatedOnly
	}
	if utf16Len(original) > remaining {
		original = truncateUTF16(original, remaining-1) + "…"
	}
	return authorLine + "\n\n" + escapeHTMLText(label) + "\n\n" +
		escapeHTMLText(translated) + "\n\n" +
		"<blockquote expandable>" + escapeHTMLText(original) + "</blockquote>\n\n" + sourceLine
}

var languageNamesZH = map[string]string{
	"en": "英语",
	"zh": "中文",
	"ja": "日语",
	"ko": "韩语",
	"fr": "法语",
	"de": "德语",
	"es": "西班牙语",
	"ru": "俄语",
	"pt": "葡萄牙语",
	"it": "意大利语",
	"ar": "阿拉伯语",
	"hi": "印地语",
	"th": "泰语",
	"vi": "越南语",
	"id": "印尼语",
	"tr": "土耳其语",
	"nl": "荷兰语",
	"pl": "波兰语",
	"uk": "乌克兰语",
}

func formatTranslationLabel(tr *Translation) string {
	provider := strings.TrimSpace(tr.Provider)
	if provider == "" {
		provider = "Grok"
	} else {
		runes := []rune(provider)
		runes[0] = unicode.ToUpper(runes[0])
		provider = string(runes)
	}
	return "翻译自" + languageNameZH(tr.SourceLang, tr.SourceLangEN) + " by " + provider
}

func languageNameZH(sourceLang, sourceLangEN string) string {
	code := strings.ToLower(strings.TrimSpace(sourceLang))
	if name, ok := languageNamesZH[code]; ok {
		return name
	}
	if i := strings.IndexByte(code, '-'); i > 0 {
		if name, ok := languageNamesZH[code[:i]]; ok {
			return name
		}
	}
	if en := strings.TrimSpace(sourceLangEN); en != "" {
		return en
	}
	if code != "" {
		return code
	}
	return "原文"
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
