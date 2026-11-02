package main

import (
	"strings"
	"testing"
)

func TestChooseSendKind(t *testing.T) {
	tests := []struct {
		name string
		item downloadedMedia
		want sendKind
	}{
		{"small photo", downloadedMedia{media: Media{Type: MediaPhoto}, size: 5 << 20}, sendPhoto},
		{"10mb photo", downloadedMedia{media: Media{Type: MediaPhoto}, size: photoUploadLimit}, sendPhoto},
		{"large photo", downloadedMedia{media: Media{Type: MediaPhoto}, size: photoUploadLimit + 1}, sendDocument},
		{"video", downloadedMedia{media: Media{Type: MediaVideo}, size: 1}, sendVideo},
		{"gif", downloadedMedia{media: Media{Type: MediaGIF}, size: 1}, sendVideo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chooseSendKind(tt.item); got != tt.want {
				t.Errorf("chooseSendKind = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChunkMedia(t *testing.T) {
	makeItems := func(n int) []downloadedMedia {
		items := make([]downloadedMedia, n)
		for i := range items {
			items[i] = downloadedMedia{media: Media{Type: MediaPhoto}}
		}
		return items
	}

	tests := []struct {
		n        int
		wantLens []int
	}{
		{0, nil},
		{1, []int{1}},
		{10, []int{10}},
		{11, []int{10, 1}},
		{25, []int{10, 10, 5}},
	}
	for _, tt := range tests {
		chunks := chunkMedia(makeItems(tt.n), mediaGroupLimit)
		if len(chunks) != len(tt.wantLens) {
			t.Fatalf("n=%d: got %d chunks, want %d", tt.n, len(chunks), len(tt.wantLens))
		}
		for i, want := range tt.wantLens {
			if len(chunks[i]) != want {
				t.Errorf("n=%d chunk %d len = %d, want %d", tt.n, i, len(chunks[i]), want)
			}
		}
	}
}

func TestTruncateText(t *testing.T) {
	if got := truncateText("hello", 10); got != "hello" {
		t.Errorf("got %q", got)
	}
	if got := truncateText("你好世界", 3); got != "你好…" {
		t.Errorf("got %q", got)
	}
	if strings.Contains(truncateText(strings.Repeat("a", 5000), 4000), strings.Repeat("a", 4001)) {
		t.Error("not truncated")
	}
}
