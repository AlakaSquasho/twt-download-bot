package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	jobQueueSize       = 64
	maxRefsPerMessage  = 5
	mediaUploadTimeout = 10 * time.Minute
)

type botApp struct {
	cfg        config
	api        *tgbotapi.BotAPI
	mediaAPI   *tgbotapi.BotAPI
	log        *slog.Logger
	fetcher    *mediaFetcher
	sources    *sourceRegistry
	jobs       chan job
	uploadSlot *traceSlot

	statusProgressEvery time.Duration
}

type job struct {
	chatID        int64
	userMessageID int
	refs          []Ref
	trace         *trace
}

type contextTransport struct {
	ctx  context.Context
	base http.RoundTripper
	slot *traceSlot
}

func (t *contextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := t.ctx
	if at := t.slot.current(); at != nil {
		ctx = withAttemptTrace(ctx, at)
	}
	return t.base.RoundTrip(req.WithContext(ctx))
}

// newUploadTransport 强制 HTTP/1.1：Telegram 上传在丢包线路上会被 HTTP/2 流控进一步拖慢。
// Clone 会初始化 DefaultTransport 的 HTTP/2 并把 "h2" 写进 TLSClientConfig.NextProtos，
// 只清空 TLSNextProto 不够，必须同时把 ALPN 限制为 http/1.1。
func newUploadTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	} else {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	}
	tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return tr
}

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	apiClient := &http.Client{
		Timeout:   cfg.pollTimeout + 30*time.Second,
		Transport: &contextTransport{ctx: ctx, base: http.DefaultTransport},
	}
	api, err := newBotAPI(ctx, cfg.token, apiClient, log)
	if err != nil {
		return err
	}

	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()

	// 上传 50MB 文件远超轮询 client 的 90s 超时，单独用一个长超时副本。
	uploadSlot := &traceSlot{}
	mediaAPI := *api
	mediaAPI.Client = &http.Client{
		Timeout:   mediaUploadTimeout,
		Transport: &contextTransport{ctx: workerCtx, base: newUploadTransport(), slot: uploadSlot},
	}

	app := &botApp{
		cfg:        cfg,
		api:        api,
		mediaAPI:   &mediaAPI,
		log:        log,
		fetcher:    newMediaFetcher(newHTTPClient(checkMediaRedirect), cfg.tmpDir, cfg.maxUploadBytes),
		sources:    newRegistry(newFxTwitter(cfg.fxtwitterBase, newHTTPClient(nil))),
		jobs:       make(chan job, jobQueueSize),
		uploadSlot: uploadSlot,
	}

	if removed := cleanupStaleTemp(cfg.tmpDir, staleTempAge); removed > 0 {
		log.Info("removed stale temp files", "count", removed, "dir", cfg.tmpDir)
	}
	log.Info("bot started",
		"username", api.Self.UserName,
		"state_dir", cfg.stateDir,
		"tmp_dir", cfg.tmpDir,
		"allowed_users", len(cfg.allowedUsers),
		"timing", cfg.timing.String(),
	)

	go app.heartbeatLoop(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		app.worker(workerCtx)
	}()

	err = app.runPolling(ctx)
	stopWorker()
	wg.Wait()
	return err
}

func newBotAPI(ctx context.Context, token string, client *http.Client, log *slog.Logger) (*tgbotapi.BotAPI, error) {
	backoff := 5 * time.Second
	for {
		api, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, client)
		if err == nil {
			return api, nil
		}
		if isFatalAuthError(err) {
			return nil, fmt.Errorf("telegram rejected the bot token: %w", err)
		}
		log.Warn("telegram API unreachable, retrying", "err", redact(err, token), "retry_in", backoff)
		if sleepCtx(ctx, backoff) != nil {
			return nil, ctx.Err()
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func isFatalAuthError(err error) bool {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusUnauthorized || apiErr.Code == http.StatusNotFound
	}
	return false
}

func (a *botApp) runPolling(ctx context.Context) error {
	offset := a.loadOffset()
	a.log.Info("polling started", "offset", offset)

	backoff := time.Second
	for ctx.Err() == nil {
		updatesCfg := tgbotapi.NewUpdate(offset)
		updatesCfg.Timeout = int(a.cfg.pollTimeout.Seconds())
		updatesCfg.Limit = 100
		updatesCfg.AllowedUpdates = []string{"message"}

		updates, err := a.api.GetUpdates(updatesCfg)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if isFatalAuthError(err) {
				return fmt.Errorf("telegram rejected the bot token: %w", err)
			}
			a.log.Warn("get updates failed, retrying", "err", redact(err, a.cfg.token), "retry_in", backoff)
			if sleepCtx(ctx, backoff) != nil {
				return nil
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		for _, update := range updates {
			if ctx.Err() != nil {
				return nil
			}
			offset = update.UpdateID + 1
			a.handleUpdate(ctx, update)
			saveStart := time.Now()
			if err := a.saveOffset(offset); err != nil {
				a.log.Warn("save offset failed", "err", err)
			} else {
				a.log.Debug("offset saved", "dur", time.Since(saveStart))
			}
		}
	}
	return nil
}

func (a *botApp) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	recv := time.Now()
	msg := update.Message
	if msg == nil || msg.From == nil {
		return
	}

	if _, ok := a.cfg.allowedUsers[msg.From.ID]; !ok {
		a.log.Warn("rejected message from unauthorized user",
			"user_id", msg.From.ID,
			"username", msg.From.UserName,
			"chat_id", msg.Chat.ID,
		)
		return
	}

	if msg.IsCommand() {
		a.handleCommand(msg)
		return
	}
	if msg.Text == "" {
		return
	}

	refs := a.sources.Extract(msg.Text)
	if len(refs) == 0 {
		lower := strings.ToLower(msg.Text)
		if strings.Contains(lower, "x.com/") || strings.Contains(lower, "twitter.com/") {
			a.replyTo(msg.Chat.ID, msg.MessageID, "未识别到推文链接。支持 x.com / twitter.com 的 /status/<id> 链接，或直接发送推文 ID。")
		}
		return
	}
	if len(refs) > maxRefsPerMessage {
		refs = refs[:maxRefsPerMessage]
		a.replyTo(msg.Chat.ID, msg.MessageID, fmt.Sprintf("一次最多处理 %d 条链接，已处理前 %d 条。", maxRefsPerMessage, maxRefsPerMessage))
	}

	traceID := fmt.Sprintf("%d:%d", msg.Chat.ID, msg.MessageID)
	tr := newTrace(a.log, traceID, int64(msg.Date), recv, a.cfg.timing)
	if tr != nil {
		tr.enqueueNow()
	}

	select {
	case a.jobs <- job{chatID: msg.Chat.ID, userMessageID: msg.MessageID, refs: refs, trace: tr}:
	case <-ctx.Done():
	default:
		a.replyTo(msg.Chat.ID, msg.MessageID, "当前任务较多，请稍后重试。")
	}
}

func (a *botApp) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-a.jobs:
			a.processJob(ctx, j)
		}
	}
}

func (a *botApp) processJob(ctx context.Context, j job) {
	tr := j.trace
	if tr != nil {
		ctx = withTrace(ctx, tr)
		defer tr.emit(a.log)
		tr.enterWorker()
		tr.setRefs(len(j.refs))
	}

	for i, ref := range j.refs {
		if i > 0 {
			sleepStart := time.Now()
			if sleepCtx(ctx, time.Second) != nil {
				return
			}
			if tr != nil {
				tr.addInterRefSleep(time.Since(sleepStart))
			}
		}
		if ctx.Err() != nil {
			return
		}
		a.processRef(ctx, j.chatID, j.userMessageID, ref, i, len(j.refs))
	}
}

func (a *botApp) processRef(ctx context.Context, chatID int64, replyTo int, ref Ref, index, total int) {
	tr := traceFrom(ctx)
	if tr != nil {
		tr.mark("status_start", "ref", ref.Key)
	}
	st := a.startStatus(ctx, chatID, replyTo, statusFetchText(index, total))
	defer st.close()

	if tr != nil {
		tr.mark("fetch_start", "ref", ref.Key)
	}
	fetchStart := time.Now()
	post, err := a.sources.Fetch(ctx, ref)
	fetchDur := time.Since(fetchStart)
	if tr != nil {
		tr.addFetch(fetchDur)
		tr.markDur("fetch_done", fetchDur)
	}
	if err != nil {
		if ctx.Err() != nil {
			st.finish("已取消")
			return
		}
		if errors.Is(err, ErrPostUnavailable) {
			st.finish("🚫 帖子不存在、已删除或不可见\n" + ref.URL)
			return
		}
		a.log.Warn("fetch post failed", "url", ref.URL, "err", err)
		st.finish("❌ 获取推文失败，请稍后重试\n" + ref.URL)
		return
	}
	if len(post.Media) == 0 {
		st.finishHTML(renderPostHTML(post, messageTextLimit))
		return
	}

	if tr != nil {
		tr.mark("send_start", "media", len(post.Media))
	}
	stats := a.sendPost(ctx, chatID, replyTo, post, renderPostCaption(post), st)
	if tr != nil {
		tr.mark("send_done", "sent", stats.sent, "failed", stats.failed)
	}

	if ctx.Err() != nil && stats.sent == 0 {
		st.finish("已取消")
		return
	}
	if stats.sent > 0 {
		st.delete()
		if stats.tooLarge > 0 || stats.failed > 0 {
			a.replyTo(chatID, replyTo, mediaFailureText(ref, stats, a.cfg.maxUploadBytes))
		}
		return
	}
	st.finish(mediaFailureText(ref, stats, a.cfg.maxUploadBytes))
}

func statusFetchText(index, total int) string {
	if total <= 1 {
		return "⏳ 正在获取推文…"
	}
	return fmt.Sprintf("⏳ 正在获取推文… (%d/%d)", index+1, total)
}

func mediaFailureText(ref Ref, stats sendStats, maxUploadBytes int64) string {
	var b strings.Builder
	if stats.sent > 0 {
		fmt.Fprintf(&b, "⚠️ 已发送 %d 个媒体", stats.sent)
	} else {
		b.WriteString("⚠️ 没有可发送的媒体")
	}
	if stats.tooLarge > 0 {
		fmt.Fprintf(&b, "；%d 个超过 %d MB 未发送", stats.tooLarge, maxUploadBytes>>20)
	}
	if stats.failed > 0 {
		fmt.Fprintf(&b, "；%d 个失败", stats.failed)
	}
	b.WriteString("\n")
	b.WriteString(ref.URL)
	for _, mediaURL := range stats.tooLargeURLs {
		b.WriteString("\n")
		b.WriteString(mediaURL)
	}
	return b.String()
}

func (a *botApp) handleCommand(msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start", "help":
		a.reply(msg.Chat.ID, strings.Join([]string{
			"发送推文链接即可下载媒体，并以 Reply 方式返回帖子内容与媒体。",
			"支持 x.com / twitter.com 的 /status/<id> 链接，也支持直接发送推文 ID。",
			"一条消息可包含多个链接（最多 5 条）。",
			fmt.Sprintf("单个媒体上限 %d MB。", a.cfg.maxUploadBytes>>20),
		}, "\n"))
	}
}

func (a *botApp) reply(chatID int64, text string) {
	if _, err := a.api.Send(tgbotapi.NewMessage(chatID, truncateText(text, 4096))); err != nil {
		a.log.Warn("send message failed", "chat_id", chatID, "err", redact(err, a.cfg.token))
	}
}

func (a *botApp) replyTo(chatID int64, replyTo int, text string) {
	msg := tgbotapi.NewMessage(chatID, truncateText(text, 4096))
	setReply(&msg.BaseChat, replyTo)
	if _, err := a.api.Send(msg); err != nil {
		a.log.Warn("send reply failed", "chat_id", chatID, "err", redact(err, a.cfg.token))
	}
}

func truncateText(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

func (a *botApp) heartbeatLoop(ctx context.Context) {
	a.writeHeartbeat()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.writeHeartbeat()
		}
	}
}

func (a *botApp) writeHeartbeat() {
	path := filepath.Join(a.cfg.stateDir, heartbeatFile)
	data := []byte(strconv.FormatInt(time.Now().Unix(), 10))
	if err := writeFileAtomic(path, data); err != nil {
		a.log.Warn("write heartbeat failed", "err", err)
	}
}

func (a *botApp) loadOffset() int {
	path := filepath.Join(a.cfg.stateDir, offsetFile)
	if data, err := os.ReadFile(path); err == nil {
		if id, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && id >= 0 {
			return id
		}
		a.log.Warn("ignoring invalid offset file", "path", path)
	}

	a.log.Info("no saved offset, skipping pending updates")
	pending := tgbotapi.NewUpdate(-1)
	pending.Limit = 1
	pending.Timeout = 0
	updates, err := a.api.GetUpdates(pending)
	if err == nil {
		if len(updates) > 0 {
			offset := updates[len(updates)-1].UpdateID + 1
			if err := a.saveOffset(offset); err != nil {
				a.log.Warn("save offset failed", "err", err)
			}
			return offset
		}
		return 0
	}
	a.log.Warn("could not read pending updates", "err", redact(err, a.cfg.token))
	return 0
}

func (a *botApp) saveOffset(offset int) error {
	return writeFileAtomic(filepath.Join(a.cfg.stateDir, offsetFile), []byte(strconv.Itoa(offset)))
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
