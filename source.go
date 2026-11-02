package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

var (
	// ErrPostUnavailable 表示帖子不存在、已删除或不可见，重试没有意义。
	ErrPostUnavailable = errors.New("post unavailable")
	// ErrTooLarge 表示媒体超过允许上传的大小。
	ErrTooLarge = errors.New("media exceeds size limit")
)

type MediaType string

const (
	MediaPhoto MediaType = "photo"
	MediaVideo MediaType = "video"
	MediaGIF   MediaType = "gif"
)

// Ref 是从文本中识别出的一处帖子引用。
type Ref struct {
	Source string // 来源名，如 "twitter"
	Key    string // 同一来源内的唯一键，Twitter 为推文 ID
	Stem   string // 媒体文件名主干，Twitter 为推文 ID
	URL    string // 面向用户的链接
	Arg    string // 交给来源 Fetch 的原始标识
}

type Media struct {
	URL      string
	Type     MediaType
	Filename string // 建议文件名，含扩展名
}

type Post struct {
	CanonicalURL string
	AuthorName   string
	AuthorHandle string
	Text         string
	Media        []Media
}

// Match 带起始位置，便于多来源结果按原文顺序排列。
type Match struct {
	Pos int
	Ref Ref
}

// Source 是一个站点适配器。新增站点只需实现此接口并在 run() 中注册。
type Source interface {
	Name() string
	FindRefs(text string) []Match
	Fetch(ctx context.Context, ref Ref) (*Post, error)
}

type sourceRegistry struct {
	sources []Source
	byName  map[string]Source
}

func newRegistry(sources ...Source) *sourceRegistry {
	r := &sourceRegistry{
		sources: sources,
		byName:  make(map[string]Source, len(sources)),
	}
	for _, s := range sources {
		r.byName[s.Name()] = s
	}
	return r
}

func (r *sourceRegistry) Extract(text string) []Ref {
	var matches []Match
	for _, s := range r.sources {
		matches = append(matches, s.FindRefs(text)...)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Pos < matches[j].Pos
	})

	seen := make(map[string]struct{}, len(matches))
	refs := make([]Ref, 0, len(matches))
	for _, m := range matches {
		id := m.Ref.Source + "\x00" + m.Ref.Key
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		refs = append(refs, m.Ref)
	}
	return refs
}

func (r *sourceRegistry) Fetch(ctx context.Context, ref Ref) (*Post, error) {
	s, ok := r.byName[ref.Source]
	if !ok {
		return nil, fmt.Errorf("unknown source %q", ref.Source)
	}
	return s.Fetch(ctx, ref)
}
