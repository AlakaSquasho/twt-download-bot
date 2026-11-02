package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func testFetcher(t *testing.T, handler http.HandlerFunc, maxBytes int64) (*mediaFetcher, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &mediaFetcher{
		client:   srv.Client(),
		destDir:  t.TempDir(),
		maxBytes: maxBytes,
		allow:    func(string) bool { return true },
		sleep:    noSleep,
	}, srv
}

func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	return entries
}

func TestDownloadContentLengthTooLarge(t *testing.T) {
	var calls int32
	fetcher, srv := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write(make([]byte, 100))
	}, 10)

	_, _, err := fetcher.Download(context.Background(), Media{URL: srv.URL + "/big.jpg", Filename: "big.jpg"})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry for too large)", got)
	}
	if entries := dirEntries(t, fetcher.destDir); len(entries) != 0 {
		t.Errorf("temp dir not empty: %v", entries)
	}
}

func TestDownloadChunkedTooLarge(t *testing.T) {
	fetcher, srv := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("0123456789"))
	}, 10)

	_, _, err := fetcher.Download(context.Background(), Media{URL: srv.URL + "/big.jpg", Filename: "big.jpg"})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if entries := dirEntries(t, fetcher.destDir); len(entries) != 0 {
		t.Errorf("temp dir not empty: %v", entries)
	}
}

func TestDownloadRetriesServerError(t *testing.T) {
	var calls int32
	fetcher, srv := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("hello"))
	}, 100)

	path, size, err := fetcher.Download(context.Background(), Media{URL: srv.URL + "/ok.mp4", Filename: "ok.mp4"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if size != 5 {
		t.Errorf("size = %d, want 5", size)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "hello" {
		t.Errorf("file body = %q, err = %v", body, err)
	}
	if filepath.Base(path) != "ok.mp4" {
		t.Errorf("filename = %q", filepath.Base(path))
	}
}

func TestDownloadRejectsNonTwimgHost(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	fetcher := &mediaFetcher{
		client:   srv.Client(),
		destDir:  t.TempDir(),
		maxBytes: 100,
		allow:    isTwimgURL,
		sleep:    noSleep,
	}
	_, _, err := fetcher.Download(context.Background(), Media{URL: srv.URL + "/x.jpg", Filename: "x.jpg"})
	if !errors.Is(err, errMediaHost) {
		t.Fatalf("err = %v, want errMediaHost", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("calls = %d, want 0", got)
	}
}

func TestIsTwimgURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://pbs.twimg.com/media/a.jpg", true},
		{"https://video.twimg.com/a.mp4", true},
		{"https://twimg.com/a", true},
		{"http://pbs.twimg.com/a.jpg", false},
		{"https://evil.com/?x=twimg.com", false},
		{"https://eviltwimg.com/a.jpg", false},
	}
	for _, tt := range tests {
		if got := isTwimgURL(tt.url); got != tt.want {
			t.Errorf("isTwimgURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"123.jpg", "123.jpg"},
		{"../evil/name.MP4", "name.mp4"},
		{"a b?.jpg", "a_b.jpg"},
		{"", "media.bin"},
		{"weird.exe", "weird.bin"},
	}
	for _, tt := range tests {
		if got := sanitizeFilename(tt.in); got != tt.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
