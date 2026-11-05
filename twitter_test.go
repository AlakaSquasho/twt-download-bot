package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func noSleep(context.Context, time.Duration) error { return nil }

func TestFindRefs(t *testing.T) {
	src := newFxTwitter("https://api.fxtwitter.com", nil, "")

	tests := []struct {
		name string
		text string
		ids  []string
	}{
		{"x.com", "https://x.com/user/status/1234567890", []string{"1234567890"}},
		{"twitter.com with query", "https://twitter.com/u/status/42?s=20", []string{"42"}},
		{"mobile", "https://mobile.twitter.com/a/status/7", []string{"7"}},
		{"bare id", "  12345  ", []string{"12345"}},
		{"multiple in order", "see https://x.com/a/status/1 and https://twitter.com/b/status/2.", []string{"1", "2"}},
		{"no link", "hello 12345 world", nil},
		{"other site", "https://example.com/status/123", nil},
		{"too long id", "https://x.com/a/status/12345678901234567890123456", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := src.FindRefs(tt.text)
			if len(got) != len(tt.ids) {
				t.Fatalf("got %d refs, want %d: %+v", len(got), len(tt.ids), got)
			}
			for i, want := range tt.ids {
				if got[i].Ref.Key != want {
					t.Errorf("ref %d key = %q, want %q", i, got[i].Ref.Key, want)
				}
			}
		})
	}
}

func TestRegistryExtractDedupes(t *testing.T) {
	r := newRegistry(newFxTwitter("https://api.fxtwitter.com", nil, ""))
	refs := r.Extract("https://x.com/a/status/1 https://twitter.com/b/status/1 https://x.com/c/status/2")
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(refs), refs)
	}
	if refs[0].Key != "1" || refs[1].Key != "2" {
		t.Fatalf("unexpected order: %+v", refs)
	}
}

func TestPhotoOriginalURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://pbs.twimg.com/media/abc.jpg", "https://pbs.twimg.com/media/abc.jpg?name=orig"},
		{"https://pbs.twimg.com/media/abc.jpg?format=jpg", "https://pbs.twimg.com/media/abc.jpg?format=jpg&name=orig"},
		{"https://pbs.twimg.com/media/abc.jpg?name=large", "https://pbs.twimg.com/media/abc.jpg?name=large"},
		{"https://video.twimg.com/abc.jpg", "https://video.twimg.com/abc.jpg"},
	}
	for _, tt := range tests {
		if got := photoOriginalURL(tt.in); got != tt.want {
			t.Errorf("photoOriginalURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseFxPost(t *testing.T) {
	tweet := &fxTweet{
		ID:   "123",
		URL:  "https://x.com/u/status/123",
		Text: "hello\nworld",
	}
	tweet.Author.Name = "User Name"
	tweet.Author.ScreenName = "u"
	tweet.Media.All = []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}{
		{Type: "photo", URL: "https://pbs.twimg.com/media/a.jpg?format=jpg"},
		{Type: "photo", URL: "https://pbs.twimg.com/media/b.jpg?format=jpg"},
		{Type: "video", URL: "https://video.twimg.com/v.mp4?tag=12"},
		{Type: "gif", URL: "https://video.twimg.com/g.mp4"},
	}

	post := parseFxPost(tweet, Ref{Key: "123", Stem: "123", Arg: "123"})
	if post.CanonicalURL != "https://x.com/u/status/123" {
		t.Errorf("canonical = %q", post.CanonicalURL)
	}
	if post.AuthorName != "User Name" || post.AuthorHandle != "u" {
		t.Errorf("author = %q (@%q)", post.AuthorName, post.AuthorHandle)
	}
	if post.Text != "hello\nworld" {
		t.Errorf("text = %q", post.Text)
	}
	if len(post.Media) != 4 {
		t.Fatalf("got %d media, want 4", len(post.Media))
	}
	wantNames := []string{"123_1.jpg", "123_2.jpg", "123_3.mp4", "123_4.mp4"}
	for i, want := range wantNames {
		if post.Media[i].Filename != want {
			t.Errorf("media %d filename = %q, want %q", i, post.Media[i].Filename, want)
		}
	}
	if !strings.Contains(post.Media[0].URL, "name=orig") {
		t.Errorf("photo URL missing name=orig: %s", post.Media[0].URL)
	}
}

func TestParseFxPostSingleAndEmpty(t *testing.T) {
	tweet := &fxTweet{ID: "9"}
	tweet.Author.ScreenName = "a"
	tweet.Media.All = []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}{{Type: "photo", URL: "https://pbs.twimg.com/media/x.png"}}

	post := parseFxPost(tweet, Ref{Key: "9", Stem: "9", Arg: "9"})
	if post.CanonicalURL != "https://x.com/a/status/9" {
		t.Errorf("canonical = %q", post.CanonicalURL)
	}
	if post.Media[0].Filename != "9.png" {
		t.Errorf("filename = %q", post.Media[0].Filename)
	}

	empty := &fxTweet{ID: "10"}
	if got := parseFxPost(empty, Ref{Stem: "10", Arg: "10"}); len(got.Media) != 0 {
		t.Errorf("expected no media, got %+v", got.Media)
	}
}

func TestParseFxPostDerivesHandleFromURL(t *testing.T) {
	tweet := &fxTweet{ID: "5", URL: "https://x.com/derived/status/5"}
	post := parseFxPost(tweet, Ref{Stem: "5", Arg: "5"})
	if post.AuthorHandle != "derived" {
		t.Errorf("handle = %q, want derived", post.AuthorHandle)
	}
	if post.CanonicalURL != "https://x.com/derived/status/5" {
		t.Errorf("canonical = %q", post.CanonicalURL)
	}
}

func TestFetchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status/123" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":200,"tweet":{"id":"123","author":{"screen_name":"u"},"media":{"all":[{"type":"photo","url":"https://pbs.twimg.com/media/a.jpg"}]}}}`))
	}))
	defer srv.Close()

	src := newFxTwitter(srv.URL, srv.Client(), "")
	src.sleep = noSleep
	post, err := src.Fetch(context.Background(), src.ref("123"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if post.CanonicalURL != "https://x.com/u/status/123" {
		t.Errorf("canonical = %q", post.CanonicalURL)
	}
	if len(post.Media) != 1 || post.Media[0].Filename != "123.jpg" {
		t.Errorf("media = %+v", post.Media)
	}
	if !strings.Contains(post.Media[0].URL, "name=orig") {
		t.Errorf("photo URL missing name=orig: %s", post.Media[0].URL)
	}
}

func TestFetchTranslation(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"code":200,"tweet":{"id":"123","url":"https://x.com/u/status/123","text":"hello","author":{"screen_name":"u"},"translation":{"text":"你好","source_lang":"en","source_lang_en":"English","provider":"grok"}}}`))
	}))
	defer srv.Close()

	src := newFxTwitter(srv.URL, srv.Client(), "zh-Hans")
	src.sleep = noSleep
	post, err := src.Fetch(context.Background(), src.ref("123"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/status/123/zh-Hans" {
		t.Errorf("paths = %v, want [/status/123/zh-Hans]", paths)
	}
	if post.Text != "hello" {
		t.Errorf("text = %q, want original", post.Text)
	}
	tr := post.Translation
	if tr == nil {
		t.Fatal("translation missing")
	}
	if tr.Text != "你好" || tr.SourceLang != "en" || tr.SourceLangEN != "English" || tr.Provider != "grok" {
		t.Errorf("translation = %+v", tr)
	}
}

func TestFetchTranslationUnsupportedFallsBack(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if strings.HasSuffix(r.URL.Path, "/zh-Hans") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":200,"tweet":{"id":"123","url":"https://x.com/u/status/123","text":"hello","author":{"screen_name":"u"}}}`))
	}))
	defer srv.Close()

	src := newFxTwitter(srv.URL, srv.Client(), "zh-Hans")
	src.sleep = noSleep
	post, err := src.Fetch(context.Background(), src.ref("123"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2 (language path + plain fallback)", got)
	}
	if post.Text != "hello" || post.Translation != nil {
		t.Errorf("post = %+v, want plain original", post)
	}
}

func TestParseFxPostTranslationFilters(t *testing.T) {
	tests := []struct {
		name string
		tr   *fxTranslation
	}{
		{"empty", &fxTranslation{Text: "  ", SourceLang: "en"}},
		{"same as original", &fxTranslation{Text: "hello", SourceLang: "en"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tweet := &fxTweet{ID: "1", Text: "hello", Translation: tt.tr}
			post := parseFxPost(tweet, Ref{Key: "1", Stem: "1", Arg: "1"})
			if post.Translation != nil {
				t.Errorf("translation = %+v, want nil", post.Translation)
			}
		})
	}
}

func TestFetchClassification(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		status      int
		wantUnavail bool
		wantCalls   int32
	}{
		{"not found", `{"code":404,"message":"not found"}`, 200, true, 1},
		{"rate limited", `{"code":429,"message":"rate limited"}`, 200, false, 3},
		{"bad json", `not json`, 200, false, 3},
		{"server error", `oops`, 500, false, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			src := newFxTwitter(srv.URL, srv.Client(), "")
			src.sleep = noSleep
			_, err := src.Fetch(context.Background(), src.ref("1"))
			if err == nil {
				t.Fatal("expected error")
			}
			if errors.Is(err, ErrPostUnavailable) != tt.wantUnavail {
				t.Errorf("ErrPostUnavailable = %v, want %v (err=%v)", errors.Is(err, ErrPostUnavailable), tt.wantUnavail, err)
			}
			if got := atomic.LoadInt32(&calls); got != tt.wantCalls {
				t.Errorf("calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}
