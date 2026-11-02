package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	downloadAttempts       = 3
	downloadAttemptTimeout = 120 * time.Second
	maxRedirects           = 5
)

var (
	errMediaGone     = errors.New("media not found")
	errMediaHost     = errors.New("media URL is not on twimg.com")
	invalidNameChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

// newHTTPClient 复制默认 Transport，保留 HTTPS_PROXY 等环境变量配置。
func newHTTPClient(checkRedirect func(*http.Request, []*http.Request) error) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{Transport: transport, CheckRedirect: checkRedirect}
}

func isTwimgURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "twimg.com" || strings.HasSuffix(host, ".twimg.com")
}

func checkMediaRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	if !isTwimgURL(req.URL.String()) {
		return errMediaHost
	}
	return nil
}

type mediaFetcher struct {
	client   *http.Client
	destDir  string
	maxBytes int64
	allow    func(string) bool
	sleep    func(context.Context, time.Duration) error
}

func newMediaFetcher(client *http.Client, destDir string, maxBytes int64) *mediaFetcher {
	return &mediaFetcher{
		client:   client,
		destDir:  destDir,
		maxBytes: maxBytes,
		allow:    isTwimgURL,
		sleep:    sleepCtx,
	}
}

func (f *mediaFetcher) Download(ctx context.Context, media Media) (string, int64, error) {
	return f.DownloadWithProgress(ctx, media, nil)
}

func (f *mediaFetcher) DownloadWithProgress(ctx context.Context, media Media, progress func(done, total int64)) (string, int64, error) {
	if f.allow != nil && !f.allow(media.URL) {
		return "", 0, fmt.Errorf("%w: %s", errMediaHost, media.URL)
	}
	sleep := f.sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	var lastErr error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		actx, at := beginAttempt(ctx, "dl", attempt)
		path, size, err := f.downloadOnce(actx, media, progress)
		at.finish(err)
		if err == nil {
			return path, size, nil
		}
		lastErr = err
		if errors.Is(err, ErrTooLarge) || errors.Is(err, errMediaGone) || errors.Is(err, errMediaHost) || ctx.Err() != nil {
			break
		}
		if attempt < downloadAttempts {
			sleepStart := time.Now()
			if err := sleep(ctx, time.Duration(attempt)*2*time.Second); err != nil {
				break
			}
			if tr := traceFrom(ctx); tr != nil {
				tr.addDownloadSleep(time.Since(sleepStart))
			}
		}
	}
	return "", 0, lastErr
}

func (f *mediaFetcher) downloadOnce(ctx context.Context, media Media, progress func(done, total int64)) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadAttemptTimeout)
	defer cancel()
	at := attemptFrom(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, media.URL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download media: %w", err)
	}
	defer resp.Body.Close()
	at.setStatus(resp.StatusCode)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return "", 0, fmt.Errorf("download media: http %d", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone:
		return "", 0, fmt.Errorf("%w: http %d", errMediaGone, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return "", 0, fmt.Errorf("download media: unexpected http %d", resp.StatusCode)
	}
	if resp.ContentLength > f.maxBytes {
		return "", 0, ErrTooLarge
	}

	tmp, err := os.CreateTemp(f.destDir, "dl-*")
	if err != nil {
		return "", 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	finalized := false
	defer func() {
		if !finalized {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	copyStart := time.Now()
	var reader io.Reader = io.LimitReader(resp.Body, f.maxBytes+1)
	if progress != nil {
		var done int64
		reader = &progressReader{r: reader, done: &done, total: resp.ContentLength, report: progress}
	}
	written, err := io.Copy(tmp, reader)
	copyDur := time.Since(copyStart)
	if err != nil {
		return "", 0, fmt.Errorf("write media: %w", err)
	}
	at.setBody(copyDur, written)
	if written > f.maxBytes {
		return "", 0, ErrTooLarge
	}
	syncStart := time.Now()
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync media: %w", err)
	}
	at.setSync(time.Since(syncStart))
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("close media: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", 0, fmt.Errorf("chmod media: %w", err)
	}

	dest := uniquePath(f.destDir, sanitizeFilename(media.Filename))
	renameStart := time.Now()
	if err := os.Rename(tmpName, dest); err != nil {
		return "", 0, fmt.Errorf("move media into place: %w", err)
	}
	at.setRename(time.Since(renameStart))
	finalized = true
	return dest, written, nil
}

func sanitizeFilename(name string) string {
	base := filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	ext := strings.ToLower(filepath.Ext(base))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".mp4", ".webm", ".mov":
	default:
		ext = ".bin"
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	stem = invalidNameChars.ReplaceAllString(stem, "_")
	stem = strings.Trim(stem, "._-")
	if stem == "" {
		stem = "media"
	}
	if len(stem) > 120 {
		stem = stem[:120]
	}
	return stem + ext
}

func uniquePath(dir, name string) string {
	dest := filepath.Join(dir, name)
	if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
		return dest
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i <= 1000; i++ {
		dest = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
		if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
			return dest
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
}

func cleanupStaleTemp(dir string, maxAge time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err == nil {
			removed++
		}
	}
	return removed
}
