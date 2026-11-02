package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const defaultStatusProgressEvery = 3 * time.Second

var errStatusGone = errors.New("status message gone")

// statusReporter 始终编辑同一条状态消息。所有编辑在同一把锁内串行执行，
// 进度编辑按 interval 节流，阶段切换立即生效。
type statusReporter struct {
	api       *tgbotapi.BotAPI
	log       *slog.Logger
	tr        *trace
	chatID    int64
	replyTo   int
	messageID int
	interval  time.Duration

	mu        sync.Mutex
	stageText string
	lastText  string
	lastMode  string
	lastEdit  time.Time
	closed    bool
}

func (a *botApp) startStatus(ctx context.Context, chatID int64, replyTo int, text string) *statusReporter {
	r := &statusReporter{
		api:      a.api,
		log:      a.log,
		tr:       traceFrom(ctx),
		chatID:   chatID,
		replyTo:  replyTo,
		interval: a.statusProgressEvery,
	}
	if r.interval <= 0 {
		r.interval = defaultStatusProgressEvery
	}

	msg := tgbotapi.NewMessage(chatID, truncateText(text, 4096))
	msg.ReplyToMessageID = replyTo
	msg.AllowSendingWithoutReply = true
	start := time.Now()
	m, err := a.api.Send(msg)
	dur := time.Since(start)
	if err != nil {
		a.log.Warn("send status message failed", "chat_id", chatID, "err", redact(err, a.cfg.token))
		r.tr.setStatus(dur, 0, time.Time{})
		return r
	}
	r.messageID = m.MessageID
	r.tr.setStatus(dur, int64(m.Date), start.Add(dur/2))
	return r
}

// stage 记录当前阶段并立即编辑为「阶段…」。
func (r *statusReporter) stage(text string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.stageText = text
	if r.messageID == 0 {
		return
	}
	r.editLocked(text+"…", "")
}

// progress 用当前阶段生成进度文本；两个编辑之间至少间隔 interval。
func (r *statusReporter) progress(done, total int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.messageID == 0 || r.stageText == "" {
		return
	}
	if time.Since(r.lastEdit) < r.interval {
		return
	}
	r.editLocked(r.stageText+" · "+formatProgress(done, total), "")
}

func (r *statusReporter) finish(text string) {
	r.finishMode(text, "")
}

func (r *statusReporter) finishHTML(text string) {
	r.finishMode(text, tgbotapi.ModeHTML)
}

func (r *statusReporter) finishMode(text, mode string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.messageID == 0 {
		r.sendNewLocked(text, mode)
		return
	}
	if err := r.editLocked(text, mode); errors.Is(err, errStatusGone) {
		r.messageID = 0
		r.sendNewLocked(text, mode)
	}
}

func (r *statusReporter) delete() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.messageID == 0 {
		return
	}

	id := r.messageID
	start := time.Now()
	_, err := r.api.Request(tgbotapi.NewDeleteMessage(r.chatID, id))
	r.tr.addEdit(time.Since(start))
	if err == nil || isTelegramMessage(err, "message to delete not found") || errors.Is(err, errStatusGone) {
		return
	}
	r.log.Warn("delete status message failed", "chat_id", r.chatID, "err", redact(err, ""))
	// 兜底：避免留下停在进度中的状态消息。
	r.editLocked("✅ 已完成", "")
}

func (r *statusReporter) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

func (r *statusReporter) editLocked(text, mode string) error {
	if r.messageID == 0 {
		return nil
	}
	if mode == "" {
		text = truncateText(text, 4096)
	}
	if text == r.lastText && mode == r.lastMode {
		return nil
	}

	cfg := tgbotapi.NewEditMessageText(r.chatID, r.messageID, text)
	if mode != "" {
		cfg.ParseMode = mode
	}
	start := time.Now()
	_, err := r.api.Send(cfg)
	r.tr.addEdit(time.Since(start))
	r.lastEdit = time.Now()

	if err == nil {
		r.lastText, r.lastMode = text, mode
		return nil
	}
	if isTelegramMessage(err, "message is not modified") {
		r.lastText, r.lastMode = text, mode
		return nil
	}
	if isTelegramMessage(err, "message to edit not found") || isTelegramMessage(err, "message can't be edited") {
		r.messageID = 0
		return errStatusGone
	}
	r.log.Debug("edit status message failed", "chat_id", r.chatID, "err", redact(err, ""))
	return err
}

func (r *statusReporter) sendNewLocked(text, mode string) {
	if mode == "" {
		text = truncateText(text, 4096)
	}
	msg := tgbotapi.NewMessage(r.chatID, text)
	if mode != "" {
		msg.ParseMode = mode
	}
	msg.ReplyToMessageID = r.replyTo
	msg.AllowSendingWithoutReply = true
	start := time.Now()
	if _, err := r.api.Send(msg); err != nil {
		r.log.Warn("send final message failed", "chat_id", r.chatID, "err", redact(err, ""))
	}
	r.tr.addEdit(time.Since(start))
}

func isTelegramMessage(err error, substr string) bool {
	var apiErr *tgbotapi.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return strings.Contains(apiErr.Message, substr)
}

func formatProgress(done, total int64) string {
	if done < 0 {
		done = 0
	}
	if total <= 0 {
		return fmtBytes(done)
	}
	pct := done * 100 / total
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf("%d%% (%s/%s)", pct, fmtBytes(done), fmtBytes(total))
}

// progressReader 把读取字节数报告给回调；done 由调用方提供，便于媒体组共享总量。
type progressReader struct {
	r      io.Reader
	done   *int64
	total  int64
	report func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		done := atomic.AddInt64(p.done, int64(n))
		if p.report != nil {
			p.report(done, p.total)
		}
	}
	return n, err
}
