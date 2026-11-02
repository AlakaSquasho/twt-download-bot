package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	twitterSourceName = "twitter"
	userAgent         = "twt-download-bot/1.0"
	fetchAttempts     = 3
	fetchTimeout      = 20 * time.Second
)

var (
	tweetURLRe    = regexp.MustCompile(`(?i)(?:twitter|x)\.com/(?:[^\s/?#]+/)*status/(\d{1,25})(?:[^0-9]|$)`)
	bareTweetIDRe = regexp.MustCompile(`^\d{1,25}$`)
)

// fxTwitter 通过 FxTwitter 公开 API 抓取推文，无需登录。
type fxTwitter struct {
	base   string
	client *http.Client
	sleep  func(context.Context, time.Duration) error
}

func newFxTwitter(base string, client *http.Client) *fxTwitter {
	return &fxTwitter{
		base:   strings.TrimRight(base, "/"),
		client: client,
		sleep:  sleepCtx,
	}
}

func (s *fxTwitter) Name() string { return twitterSourceName }

func (s *fxTwitter) FindRefs(text string) []Match {
	var matches []Match
	for _, loc := range tweetURLRe.FindAllStringSubmatchIndex(text, -1) {
		id := text[loc[2]:loc[3]]
		matches = append(matches, Match{Pos: loc[0], Ref: s.ref(id)})
	}
	if len(matches) > 0 {
		return matches
	}
	if id := strings.TrimSpace(text); bareTweetIDRe.MatchString(id) {
		matches = append(matches, Match{Pos: 0, Ref: s.ref(id)})
	}
	return matches
}

func (s *fxTwitter) ref(id string) Ref {
	return Ref{
		Source: twitterSourceName,
		Key:    id,
		Stem:   id,
		URL:    "https://x.com/i/web/status/" + id,
		Arg:    id,
	}
}

func (s *fxTwitter) Fetch(ctx context.Context, ref Ref) (*Post, error) {
	sleep := s.sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	var lastErr error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		actx, at := beginAttempt(ctx, "fx", attempt)
		post, err := s.fetchOnce(actx, ref)
		at.finish(err)
		if err == nil {
			return post, nil
		}
		lastErr = err
		if errors.Is(err, ErrPostUnavailable) || ctx.Err() != nil {
			return nil, err
		}
		if attempt < fetchAttempts {
			sleepStart := time.Now()
			if err := sleep(ctx, time.Duration(attempt)*2*time.Second); err != nil {
				return nil, err
			}
			if tr := traceFrom(ctx); tr != nil {
				tr.addFetchSleep(time.Since(sleepStart))
			}
		}
	}
	return nil, lastErr
}

type fxResponse struct {
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Tweet   *fxTweet `json:"tweet"`
}

type fxTweet struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Text   string `json:"text"`
	Author struct {
		Name       string `json:"name"`
		ScreenName string `json:"screen_name"`
	} `json:"author"`
	RepostedBy *struct {
		ScreenName string `json:"screen_name"`
	} `json:"reposted_by"`
	Media struct {
		All []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"all"`
	} `json:"media"`
}

func (s *fxTwitter) fetchOnce(ctx context.Context, ref Ref) (*Post, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	at := attemptFrom(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/status/"+url.PathEscape(ref.Arg), nil)
	if err != nil {
		return nil, fmt.Errorf("build request for tweet %s: %w", ref.Key, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch tweet %s: %w", ref.Key, err)
	}
	defer resp.Body.Close()
	at.setStatus(resp.StatusCode)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read tweet %s response: %w", ref.Key, err)
	}
	at.setBytes(int64(len(body)))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, fmt.Errorf("fetch tweet %s: http %d", ref.Key, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: http 404", ErrPostUnavailable)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("%w: http %d", ErrPostUnavailable, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("fetch tweet %s: unexpected http %d", ref.Key, resp.StatusCode)
	}

	var data fxResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decode tweet %s: %w", ref.Key, err)
	}
	at.setAPICode(data.Code)
	if data.Tweet == nil {
		// FxTwitter 失败时也返回 HTTP 200，真正错误码在 JSON code 里。
		switch {
		case data.Code == http.StatusTooManyRequests:
			return nil, fmt.Errorf("fetch tweet %s: rate limited", ref.Key)
		case data.Code >= 400 && data.Code < 500:
			return nil, fmt.Errorf("%w: code %d: %s", ErrPostUnavailable, data.Code, data.Message)
		default:
			return nil, fmt.Errorf("fetch tweet %s: unexpected code %d: %s", ref.Key, data.Code, data.Message)
		}
	}
	return parseFxPost(data.Tweet, ref), nil
}

func parseFxPost(tweet *fxTweet, ref Ref) *Post {
	id := tweet.ID
	if id == "" {
		id = ref.Arg
	}
	screenName := tweet.Author.ScreenName
	if screenName == "" {
		screenName = handleFromTweetURL(tweet.URL)
	}
	if screenName == "" {
		screenName = "i/web"
	}
	canonical := tweet.URL
	if canonical == "" {
		canonical = fmt.Sprintf("https://x.com/%s/status/%s", screenName, id)
	}

	var media []Media
	for _, item := range tweet.Media.All {
		if item.URL == "" {
			continue
		}
		kind := MediaType(item.Type)
		switch kind {
		case MediaPhoto, MediaVideo, MediaGIF:
		default:
			continue
		}
		u := item.URL
		if kind == MediaPhoto {
			u = photoOriginalURL(u)
		}
		media = append(media, Media{URL: u, Type: kind})
	}

	for i := range media {
		ext := mediaExt(media[i].URL, media[i].Type)
		if len(media) == 1 {
			media[i].Filename = ref.Stem + ext
		} else {
			media[i].Filename = fmt.Sprintf("%s_%d%s", ref.Stem, i+1, ext)
		}
	}
	handle := tweet.Author.ScreenName
	if handle == "" {
		handle = handleFromTweetURL(tweet.URL)
	}
	return &Post{
		CanonicalURL: canonical,
		AuthorName:   tweet.Author.Name,
		AuthorHandle: handle,
		Text:         tweet.Text,
		Media:        media,
	}
}

func handleFromTweetURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[1] != "status" || parts[0] == "i" {
		return ""
	}
	return parts[0]
}

func photoOriginalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Hostname(), "pbs.twimg.com") {
		return raw
	}
	q := u.Query()
	if q.Has("name") {
		return raw
	}
	q.Set("name", "orig")
	u.RawQuery = q.Encode()
	return u.String()
}

func mediaExt(raw string, kind MediaType) string {
	if u, err := url.Parse(raw); err == nil {
		switch ext := strings.ToLower(path.Ext(u.Path)); ext {
		case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".mp4", ".webm", ".mov":
			return ext
		}
	}
	if kind == MediaPhoto {
		return ".jpg"
	}
	return ".mp4"
}
