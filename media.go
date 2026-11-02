package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	photoUploadLimit = 10 << 20 // Telegram sendPhoto 的照片上限
	mediaGroupLimit  = 10       // sendMediaGroup 单次最多 10 项
	maxRetryAfter    = 60 * time.Second
)

type downloadedMedia struct {
	media Media
	path  string
	size  int64
	url   string // 非空表示未下载，直接交给 Telegram 按 URL 发送
}

type sendStats struct {
	sent         int
	tooLarge     int
	failed       int
	tooLargeURLs []string
}

type sendKind int

const (
	sendPhoto sendKind = iota
	sendVideo
	sendDocument
)

func chooseSendKind(item downloadedMedia) sendKind {
	if item.media.Type == MediaPhoto {
		if item.size <= photoUploadLimit {
			return sendPhoto
		}
		return sendDocument
	}
	return sendVideo
}

// canSendByURL 判断照片是否可以让 Telegram 自己从原地址拉取，省掉下载和上传。
func (a *botApp) canSendByURL(media Media) bool {
	if media.Type != MediaPhoto || media.URL == "" || a.fetcher == nil {
		return false
	}
	if a.fetcher.allow != nil {
		return a.fetcher.allow(media.URL)
	}
	return isTwimgURL(media.URL)
}

func chunkMedia(items []downloadedMedia, size int) [][]downloadedMedia {
	if size <= 0 {
		size = mediaGroupLimit
	}
	var chunks [][]downloadedMedia
	for len(items) > size {
		chunks = append(chunks, items[:size])
		items = items[size:]
	}
	if len(items) > 0 {
		chunks = append(chunks, items)
	}
	return chunks
}

// sendPost 下载并发送一条帖子的全部媒体。临时文件在返回前删除。
func (a *botApp) sendPost(ctx context.Context, chatID int64, replyTo int, post *Post, caption string, st *statusReporter) sendStats {
	var stats sendStats
	var items []downloadedMedia

	defer func() {
		for _, item := range items {
			if item.path != "" {
				_ = os.Remove(item.path)
			}
		}
	}()

	downloadTotal := 0
	for _, media := range post.Media {
		if !a.canSendByURL(media) {
			downloadTotal++
		}
	}
	downloadIndex := 0

	for _, media := range post.Media {
		if ctx.Err() != nil {
			stats.failed++
			continue
		}
		if a.canSendByURL(media) {
			if tr := traceFrom(ctx); tr != nil {
				tr.mark("download_skip", "url", media.URL)
			}
			items = append(items, downloadedMedia{media: media, url: media.URL})
			continue
		}
		downloadIndex++
		st.stage(progressStage("⬇️ 正在下载媒体", downloadIndex, downloadTotal))
		if tr := traceFrom(ctx); tr != nil {
			tr.mark("download_start", "url", media.URL)
		}
		dlStart := time.Now()
		path, size, err := a.fetcher.DownloadWithProgress(ctx, media, func(done, total int64) {
			st.progress(done, total)
		})
		dlDur := time.Since(dlStart)
		if tr := traceFrom(ctx); tr != nil {
			tr.addDownload(dlDur)
			tr.markDur("download_done", dlDur, "bytes", fmtBytes(size), "url", media.URL)
		}
		if err != nil {
			if errors.Is(err, ErrTooLarge) {
				stats.tooLarge++
				stats.tooLargeURLs = append(stats.tooLargeURLs, media.URL)
			} else {
				stats.failed++
				a.log.Warn("download media failed", "url", media.URL, "err", err)
			}
			continue
		}
		items = append(items, downloadedMedia{media: media, path: path, size: size})
	}
	if len(items) == 0 {
		return stats
	}

	captionSent := false
	chunks := chunkMedia(items, mediaGroupLimit)
	for ci, chunk := range chunks {
		if ctx.Err() != nil {
			stats.failed += len(chunk)
			continue
		}
		hasUpload := false
		for _, item := range chunk {
			if item.path != "" {
				hasUpload = true
				break
			}
		}
		if hasUpload {
			st.stage(progressStage("⬆️ 正在上传媒体", ci+1, len(chunks)))
		} else {
			st.stage(progressStage("⬆️ 正在发送媒体", ci+1, len(chunks)))
		}

		chunkCaption := ""
		if !captionSent {
			chunkCaption = caption
		}

		if err := a.sendChunk(ctx, chatID, replyTo, chunk, chunkCaption, st); err == nil {
			stats.sent += len(chunk)
			captionSent = true
			continue
		} else if len(chunk) == 1 {
			// 单条媒体已经尝试过 URL 和文件两种方式，不再重复发送。
			a.recordSendFailure(&stats, chunk[0], err)
			continue
		} else {
			a.log.Warn("send media group failed, falling back to individual sends", "err", redact(err, a.cfg.token))
		}

		for _, item := range chunk {
			if ctx.Err() != nil {
				stats.failed++
				continue
			}
			itemCaption := ""
			if !captionSent {
				itemCaption = caption
			}
			if err := a.sendSingle(ctx, chatID, replyTo, item, itemCaption, st); err != nil {
				a.recordSendFailure(&stats, item, err)
				continue
			}
			stats.sent++
			captionSent = true
		}
	}
	return stats
}

func progressStage(base string, index, total int) string {
	if total <= 1 {
		return base
	}
	return fmt.Sprintf("%s %d/%d", base, index, total)
}

func (a *botApp) recordSendFailure(stats *sendStats, item downloadedMedia, err error) {
	if errors.Is(err, ErrTooLarge) {
		stats.tooLarge++
		stats.tooLargeURLs = append(stats.tooLargeURLs, item.media.URL)
		return
	}
	stats.failed++
	a.log.Warn("send media failed", "url", item.media.URL, "err", redact(err, a.cfg.token))
}

func (a *botApp) sendChunk(ctx context.Context, chatID int64, replyTo int, items []downloadedMedia, caption string, st *statusReporter) error {
	if len(items) == 1 {
		return a.sendSingle(ctx, chatID, replyTo, items[0], caption, st)
	}
	return a.sendGroup(ctx, chatID, replyTo, items, caption, st)
}

func (a *botApp) sendSingle(ctx context.Context, chatID int64, replyTo int, item downloadedMedia, caption string, st *statusReporter) error {
	if item.url != "" {
		err := a.sendSingleAs(ctx, chatID, replyTo, item, caption, sendPhoto, st)
		if err == nil {
			return nil
		}
		a.log.Warn("send photo by URL failed, falling back to upload", "url", item.url, "err", redact(err, a.cfg.token))

		st.stage("⬇️ 正在下载媒体")
		dlStart := time.Now()
		path, size, dlErr := a.fetcher.DownloadWithProgress(ctx, item.media, func(done, total int64) {
			st.progress(done, total)
		})
		dlDur := time.Since(dlStart)
		if tr := traceFrom(ctx); tr != nil {
			tr.addDownload(dlDur)
			tr.markDur("download_fallback", dlDur, "bytes", fmtBytes(size), "url", item.url)
		}
		if dlErr != nil {
			return dlErr
		}
		defer os.Remove(path)
		item.path, item.size, item.url = path, size, ""
	}

	kind := chooseSendKind(item)
	err := a.sendSingleAs(ctx, chatID, replyTo, item, caption, kind, st)
	if err == nil || kind == sendDocument || ctx.Err() != nil {
		return err
	}
	a.log.Warn("primary send failed, retrying as document", "path", item.path, "err", redact(err, a.cfg.token))
	return a.sendSingleAs(ctx, chatID, replyTo, item, caption, sendDocument, st)
}

func (k sendKind) String() string {
	switch k {
	case sendPhoto:
		return "photo"
	case sendVideo:
		return "video"
	default:
		return "document"
	}
}

func recordUploadError(at *attempt, err error) {
	if at == nil {
		return
	}
	if err == nil {
		at.setStatus(http.StatusOK)
		return
	}
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) {
		at.setStatus(apiErr.Code)
	}
}

func (a *botApp) sendSingleAs(ctx context.Context, chatID int64, replyTo int, item downloadedMedia, caption string, kind sendKind, st *statusReporter) error {
	try := 0
	return a.withRetry(ctx, func() error {
		try++
		var file tgbotapi.RequestFileData
		via := "file"
		if item.url != "" {
			file = tgbotapi.FileURL(item.url)
			via = "url"
		} else {
			// FileReader 是一次性的，每次尝试都重新打开文件。
			f, err := os.Open(item.path)
			if err != nil {
				return err
			}
			defer f.Close()
			var reader io.Reader = f
			if st != nil {
				var done int64
				reader = &progressReader{r: f, done: &done, total: item.size, report: func(done, total int64) {
					st.progress(done, total)
				}}
			}
			file = tgbotapi.FileReader{Name: filepath.Base(item.path), Reader: reader}
		}

		var cfg tgbotapi.Chattable
		switch kind {
		case sendPhoto:
			c := tgbotapi.NewPhoto(chatID, file)
			c.Caption = caption
			if caption != "" {
				c.ParseMode = tgbotapi.ModeHTML
			}
			setReply(&c.BaseChat, replyTo)
			cfg = c
		case sendVideo:
			c := tgbotapi.NewVideo(chatID, file)
			c.Caption = caption
			if caption != "" {
				c.ParseMode = tgbotapi.ModeHTML
			}
			setReply(&c.BaseChat, replyTo)
			cfg = c
		default:
			c := tgbotapi.NewDocument(chatID, file)
			c.Caption = caption
			if caption != "" {
				c.ParseMode = tgbotapi.ModeHTML
			}
			setReply(&c.BaseChat, replyTo)
			cfg = c
		}

		label := "tg_" + kind.String()
		if via == "url" {
			label += "_url"
		}
		at := newAttempt(ctx, label, try)
		at.setSize(item.size)
		release := a.uploadSlot.set(at)
		defer release()

		if tr := traceFrom(ctx); tr != nil {
			tr.mark("upload_start", "kind", kind.String(), "via", via, "size", fmtBytes(item.size), "try", try)
		}
		uploadStart := time.Now()
		_, err := a.mediaAPI.Send(cfg)
		recordUploadError(at, err)
		at.finish(err)
		if tr := traceFrom(ctx); tr != nil {
			tr.markDur("upload_done", time.Since(uploadStart), "kind", kind.String(), "via", via)
		}
		return err
	})
}

func (a *botApp) sendGroup(ctx context.Context, chatID int64, replyTo int, items []downloadedMedia, caption string, st *statusReporter) error {
	try := 0
	var totalSize int64
	for _, item := range items {
		totalSize += item.size
	}
	return a.withRetry(ctx, func() error {
		try++
		media := make([]interface{}, 0, len(items))
		var files []*os.File
		defer func() {
			for _, f := range files {
				_ = f.Close()
			}
		}()

		var uploaded int64
		for i, item := range items {
			var file tgbotapi.RequestFileData
			if item.url != "" {
				file = tgbotapi.FileURL(item.url)
			} else {
				f, err := os.Open(item.path)
				if err != nil {
					return err
				}
				files = append(files, f)
				var reader io.Reader = f
				if st != nil {
					reader = &progressReader{r: f, done: &uploaded, total: totalSize, report: func(done, total int64) {
						st.progress(done, total)
					}}
				}
				file = tgbotapi.FileReader{Name: filepath.Base(item.path), Reader: reader}
			}

			itemCaption := ""
			if i == 0 {
				itemCaption = caption
			}
			switch chooseSendKind(item) {
			case sendPhoto:
				m := tgbotapi.NewInputMediaPhoto(file)
				m.Caption = itemCaption
				if itemCaption != "" {
					m.ParseMode = tgbotapi.ModeHTML
				}
				media = append(media, m)
			case sendVideo:
				m := tgbotapi.NewInputMediaVideo(file)
				m.Caption = itemCaption
				if itemCaption != "" {
					m.ParseMode = tgbotapi.ModeHTML
				}
				media = append(media, m)
			default:
				m := tgbotapi.NewInputMediaDocument(file)
				m.Caption = itemCaption
				if itemCaption != "" {
					m.ParseMode = tgbotapi.ModeHTML
				}
				media = append(media, m)
			}
		}
		at := newAttempt(ctx, "tg_group", try)
		at.setSize(totalSize)
		release := a.uploadSlot.set(at)
		defer release()

		if tr := traceFrom(ctx); tr != nil {
			tr.mark("upload_start", "kind", "group", "items", len(items), "size", fmtBytes(totalSize), "try", try)
		}
		mg := tgbotapi.NewMediaGroup(chatID, media)
		mg.ReplyToMessageID = replyTo
		uploadStart := time.Now()
		_, err := a.mediaAPI.SendMediaGroup(mg)
		recordUploadError(at, err)
		at.finish(err)
		if tr := traceFrom(ctx); tr != nil {
			tr.markDur("upload_done", time.Since(uploadStart), "kind", "group", "items", len(items))
		}
		return err
	})
}

func setReply(c *tgbotapi.BaseChat, replyTo int) {
	c.ReplyToMessageID = replyTo
	c.AllowSendingWithoutReply = true
}

func (a *botApp) withRetry(ctx context.Context, fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}
	var apiErr *tgbotapi.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.RetryAfter <= 0 && apiErr.Code < 500 {
		return err
	}

	delay := 2 * time.Second
	if apiErr.RetryAfter > 0 {
		delay = time.Duration(apiErr.RetryAfter) * time.Second
	}
	if delay > maxRetryAfter {
		delay = maxRetryAfter
	}
	tr := traceFrom(ctx)
	if tr != nil {
		tr.mark("upload_retry", "code", apiErr.Code, "delay", fmtDur(delay), "retry_after", apiErr.RetryAfter)
	}
	sleepStart := time.Now()
	if sleepCtx(ctx, delay) != nil {
		return ctx.Err()
	}
	if tr != nil {
		tr.addUploadRetry(time.Since(sleepStart))
	}
	return fn()
}
