package main

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func visibleHTMLText(s string) string {
	s = htmlTagRe.ReplaceAllString(s, "")
	return strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
	).Replace(s)
}

func TestRenderPostHTML(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         "hello\nworld",
	}
	want := `<a href="https://x.com/u">User(@u)</a>:` + "\n\n" +
		"hello\nworld" + "\n\n" +
		`<a href="https://x.com/u/status/1">Source</a>`
	if got := renderPostHTML(post, captionLimit); got != want {
		t.Errorf("renderPostHTML =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderPostHTMLEscaping(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/a/status/1?a=1&b=2",
		AuthorName:   `A<&>"`,
		AuthorHandle: "a",
		Text:         `<b>&'"`,
	}
	got := renderPostHTML(post, captionLimit)

	if strings.Contains(got, "&#39;") {
		t.Errorf("output must not contain numeric apostrophe entity: %q", got)
	}
	if !strings.Contains(got, "A&lt;&amp;&gt;") {
		t.Errorf("author name not escaped: %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;&amp;'\"") {
		t.Errorf("body not escaped correctly: %q", got)
	}
	if !strings.Contains(got, "?a=1&amp;b=2") {
		t.Errorf("attribute not escaped: %q", got)
	}
}

func TestRenderPostHTMLWithoutText(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         "  \n ",
	}
	got := renderPostHTML(post, captionLimit)
	if !strings.Contains(got, "</a>:\n\n<a ") {
		t.Errorf("empty text should not leave a body block: %q", got)
	}
}

func TestRenderPostHTMLFallbacks(t *testing.T) {
	post := &Post{CanonicalURL: "https://x.com/u/status/1", AuthorHandle: "u"}
	got := renderPostHTML(post, captionLimit)
	if !strings.Contains(visibleHTMLText(got), "u(@u)") {
		t.Errorf("handle fallback missing: %q", got)
	}

	post = &Post{CanonicalURL: "https://x.com/i/web/status/1"}
	got = renderPostHTML(post, captionLimit)
	if !strings.Contains(visibleHTMLText(got), "原帖作者") {
		t.Errorf("author fallback missing: %q", got)
	}
	if strings.Contains(visibleHTMLText(got), "(@)") {
		t.Errorf("empty handle should not render parentheses: %q", got)
	}
}

func TestRenderPostCaptionTruncates(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         strings.Repeat("ab", 2000),
	}
	got := renderPostCaption(post)
	visible := visibleHTMLText(got)

	if n := utf16Len(visible); n > captionLimit {
		t.Errorf("visible length = %d, want <= %d", n, captionLimit)
	}
	if !strings.Contains(visible, "User(@u)") || !strings.Contains(visible, "Source") {
		t.Errorf("author or Source missing after truncation: %q", visible)
	}
	if !strings.HasSuffix(strings.TrimRight(visible, "\n"), "Source") {
		t.Errorf("Source should be the last block: %q", visible)
	}
	if !strings.Contains(visible, "…") {
		t.Errorf("truncated body should end with ellipsis: %q", visible)
	}
}

func TestRenderPostCaptionEmojiBoundary(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         strings.Repeat("😀", 1000),
	}
	visible := visibleHTMLText(renderPostCaption(post))
	if n := utf16Len(visible); n > captionLimit {
		t.Errorf("visible length = %d, want <= %d", n, captionLimit)
	}
	if !utf8.ValidString(visible) {
		t.Error("truncation produced invalid UTF-8")
	}
	if !strings.Contains(visible, "…") {
		t.Errorf("emoji body should be truncated: %q", visible)
	}
}

func TestRenderPostHTMLTranslation(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         "hello\nworld",
		Translation: &Translation{
			Text:         "你好\n世界",
			SourceLang:   "en",
			SourceLangEN: "English",
			Provider:     "grok",
		},
	}
	want := `<a href="https://x.com/u">User(@u)</a>:` + "\n\n" +
		`<tg-emoji emoji-id="5454065032298009119">🌐</tg-emoji> <i>翻译自英语</i>` + "\n\n" +
		"你好\n世界" + "\n\n" +
		"<blockquote expandable>hello\nworld</blockquote>" + "\n\n" +
		`<a href="https://x.com/u/status/1">Source</a>`
	if got := renderPostHTML(post, captionLimit); got != want {
		t.Errorf("renderPostHTML =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderPostHTMLTranslationFallbacks(t *testing.T) {
	base := Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         "hello",
	}
	want := renderPostHTML(&base, captionLimit)

	for _, tr := range []*Translation{
		{Text: "  "},
		{Text: "hello"},
	} {
		post := base
		post.Translation = tr
		if got := renderPostHTML(&post, captionLimit); got != want {
			t.Errorf("translation %+v should fall back, got %q", tr, got)
		}
	}
}

func TestRenderPostHTMLTranslationEscaping(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         "<original &>",
		Translation: &Translation{
			Text:       "<translated &>",
			SourceLang: "en",
			Provider:   "grok",
		},
	}
	got := renderPostHTML(post, captionLimit)
	if !strings.Contains(got, "&lt;translated &amp;&gt;") {
		t.Errorf("translation not escaped: %q", got)
	}
	if !strings.Contains(got, "<blockquote expandable>&lt;original &amp;&gt;</blockquote>") {
		t.Errorf("original not escaped or not folded: %q", got)
	}
}

func TestRenderPostCaptionTranslationTruncatesOriginal(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         strings.Repeat("original ", 500),
		Translation: &Translation{
			Text:       "译文",
			SourceLang: "en",
			Provider:   "grok",
		},
	}
	visible := visibleHTMLText(renderPostCaption(post))
	if n := utf16Len(visible); n > captionLimit {
		t.Errorf("visible length = %d, want <= %d", n, captionLimit)
	}
	if !strings.Contains(visible, "译文") || !strings.Contains(visible, "翻译自英语") {
		t.Errorf("translation missing: %q", visible)
	}
	if !strings.Contains(visible, "original") {
		t.Errorf("original should still be present (truncated): %q", visible)
	}
	if !strings.HasSuffix(strings.TrimRight(visible, "\n"), "Source") {
		t.Errorf("Source should be the last block: %q", visible)
	}
}

func TestRenderPostCaptionTranslationTruncatesTranslation(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         "hello",
		Translation: &Translation{
			Text:       strings.Repeat("译", 5000),
			SourceLang: "en",
			Provider:   "grok",
		},
	}
	got := renderPostCaption(post)
	visible := visibleHTMLText(got)
	if n := utf16Len(visible); n > captionLimit {
		t.Errorf("visible length = %d, want <= %d", n, captionLimit)
	}
	if strings.Contains(got, "<blockquote") {
		t.Errorf("oversize translation must omit the original blockquote: %q", got)
	}
	if !strings.Contains(visible, "…") {
		t.Errorf("translation should be truncated: %q", visible)
	}
}

func TestRenderPostHTMLTranslationTinyOriginalBudget(t *testing.T) {
	post := &Post{
		CanonicalURL: "https://x.com/u/status/1",
		AuthorName:   "User",
		AuthorHandle: "u",
		Text:         "hello",
		Translation: &Translation{
			Text:       strings.Repeat("译", 30),
			SourceLang: "en",
			Provider:   "grok",
		},
	}
	got := renderPostHTML(post, 75)
	if strings.Contains(got, "<blockquote") {
		t.Errorf("too little room for the original; blockquote should be omitted: %q", got)
	}
	if n := utf16Len(visibleHTMLText(got)); n > 75 {
		t.Errorf("visible length = %d, want <= 75", n)
	}
}

func TestFormatTranslationLabel(t *testing.T) {
	tests := []struct {
		sourceLang   string
		sourceLangEN string
		provider     string
		want         string
	}{
		{"en", "English", "grok", "翻译自英语"},
		{"pt-BR", "Portuguese", "grok", "翻译自葡萄牙语"},
		{"ja", "Japanese", "", "翻译自日语"},
		{"xx", "Klingon", "grok", "翻译自Klingon"},
		{"", "", "", "翻译自原文"},
	}
	for _, tt := range tests {
		tr := &Translation{SourceLang: tt.sourceLang, SourceLangEN: tt.sourceLangEN, Provider: tt.provider}
		if got := formatTranslationLabel(tr); got != tt.want {
			t.Errorf("formatTranslationLabel(%q, %q, %q) = %q, want %q",
				tt.sourceLang, tt.sourceLangEN, tt.provider, got, tt.want)
		}
	}
}

func TestTranslationLabelHTML(t *testing.T) {
	tr := &Translation{SourceLang: "en", SourceLangEN: "English", Provider: "grok"}
	want := `<tg-emoji emoji-id="5454065032298009119">🌐</tg-emoji> <i>翻译自英语</i>`
	if got := translationLabelHTML(tr); got != want {
		t.Errorf("translationLabelHTML = %q, want %q", got, want)
	}
	if got, want := translationLabelVisibleLen(tr), 8; got != want {
		t.Errorf("translationLabelVisibleLen = %d, want %d", got, want)
	}
}

func TestTruncateUTF16(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"😀😀", 3, "😀"},
		{"😀😀", 2, "😀"},
		{"😀😀", 1, ""},
		{"你好", 1, "你"},
	}
	for _, tt := range tests {
		if got := truncateUTF16(tt.in, tt.max); got != tt.want {
			t.Errorf("truncateUTF16(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}
