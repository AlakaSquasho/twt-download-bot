package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

type timingMode int

const (
	timingOff timingMode = iota
	timingSummary
	timingVerbose
)

func (m timingMode) String() string {
	switch m {
	case timingOff:
		return "off"
	case timingVerbose:
		return "verbose"
	default:
		return "summary"
	}
}

func parseTimingMode(raw string) (timingMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off":
		return timingOff, nil
	case "summary":
		return timingSummary, nil
	case "verbose":
		return timingVerbose, nil
	default:
		return timingOff, fmt.Errorf("TIMING_TRACE must be off, summary or verbose, got %q", raw)
	}
}

// trace 记录一条 Telegram 消息从接收到回复完成的各阶段耗时。
// 所有方法都允许 nil receiver，便于测试和 TIMING_TRACE=off。
type trace struct {
	id      string
	mode    timingMode
	log     *slog.Logger
	start   time.Time
	msgDate time.Time

	mu       sync.Mutex
	last     time.Time
	enqueued time.Time

	queueWait time.Duration
	statusDur time.Duration
	skew      time.Duration
	skewSet   bool
	pollDelay time.Duration
	editDur   time.Duration
	refs      int

	fxDur   time.Duration
	fxTries int
	fxSleep time.Duration
	fxHTTP  int
	fxCode  int

	dlDur    time.Duration
	dlTries  int
	dlSleep  time.Duration
	dlBytes  int64
	dlBody   time.Duration
	dlSync   time.Duration
	dlRename time.Duration
	dlReused bool
	dlHTTP   int

	upDur        time.Duration
	upTries      int
	upKind       string
	upVia        string
	upSize       int64
	upWrote      time.Duration
	upTTFB       time.Duration
	upRetryAfter time.Duration
	upHTTP       int
	upErr        string
}

type traceCtxKey struct{}

func withTrace(ctx context.Context, t *trace) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, traceCtxKey{}, t)
}

func traceFrom(ctx context.Context) *trace {
	t, _ := ctx.Value(traceCtxKey{}).(*trace)
	return t
}

func newTrace(log *slog.Logger, id string, msgDateUnix int64, recv time.Time, mode timingMode) *trace {
	if mode == timingOff || log == nil {
		return nil
	}
	t := &trace{
		id:      id,
		mode:    mode,
		log:     log,
		start:   recv,
		last:    recv,
		msgDate: time.Unix(msgDateUnix, 0),
	}
	t.mark("receive")
	return t
}

func (t *trace) enqueueNow() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.enqueued = time.Now()
	t.mu.Unlock()
}

func (t *trace) enterWorker() {
	if t == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	if !t.enqueued.IsZero() {
		t.queueWait = now.Sub(t.enqueued)
	}
	t.last = now
	t.mu.Unlock()
	t.markDur("worker_start", t.queueWait)
}

func (t *trace) setRefs(n int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.refs = n
	t.mu.Unlock()
}

// setStatus 用状态消息的服务器时间估算时钟偏移，并换算用户发送到接收的延迟。
func (t *trace) setStatus(d time.Duration, serverUnix int64, midpoint time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.statusDur += d
	if !t.skewSet && serverUnix > 0 && t.msgDate.Unix() > 0 {
		skew := time.Unix(serverUnix, 0).Sub(midpoint)
		// 时间戳异常（如测试假数据或严重时钟错误）时不输出误导性的 poll。
		if skew > -24*time.Hour && skew < 24*time.Hour {
			t.skew = skew
			t.skewSet = true
			t.pollDelay = t.start.Sub(t.msgDate.Add(-skew))
		}
	}
	poll, skewSet, skew := t.pollDelay, t.skewSet, t.skew
	t.mu.Unlock()

	attrs := []any{}
	if skewSet {
		attrs = append(attrs, "tg_delay", fmtDur(poll), "skew", fmtDur(skew))
	}
	t.markDur("status_sent", d, attrs...)
}

func (t *trace) addEdit(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.editDur += d
	t.mu.Unlock()
}

func (t *trace) addInterRefSleep(d time.Duration) {
	if t == nil {
		return
	}
	t.markDur("inter_ref_sleep", d)
}

func (t *trace) addFetch(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.fxDur += d
	t.mu.Unlock()
}

func (t *trace) addFetchSleep(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.fxSleep += d
	t.mu.Unlock()
	t.markDur("fx_retry_sleep", d)
}

func (t *trace) addDownload(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.dlDur += d
	t.mu.Unlock()
}

func (t *trace) addDownloadSleep(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.dlSleep += d
	t.mu.Unlock()
	t.markDur("dl_retry_sleep", d)
}

func (t *trace) addUploadRetry(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.upRetryAfter += d
	t.mu.Unlock()
	t.markDur("upload_retry_sleep", d)
}

func (t *trace) mark(name string, attrs ...any) {
	if t == nil || t.mode != timingVerbose {
		return
	}
	now := time.Now()
	t.mu.Lock()
	dur := now.Sub(t.last)
	t.last = now
	t.mu.Unlock()
	t.logStep(name, now.Sub(t.start), dur, attrs...)
}

func (t *trace) markDur(name string, dur time.Duration, attrs ...any) {
	if t == nil || t.mode != timingVerbose {
		return
	}
	t.logStep(name, time.Since(t.start), dur, attrs...)
}

func (t *trace) logStep(name string, at, dur time.Duration, attrs ...any) {
	args := make([]any, 0, 6+len(attrs))
	args = append(args, "trace", t.id, "name", name, "at", fmtDur(at), "dur", fmtDur(dur))
	args = append(args, attrs...)
	t.log.Info("TIMING step", args...)
}

func (t *trace) recordAttempt(a *attempt) {
	a.mu.Lock()
	label := a.label
	status, apiCode := a.status, a.apiCode
	bytes, size := a.bytes, a.size
	body, syncDur, renameDur := a.body, a.syncDur, a.renameDur
	dur, wroteDur, ttfbDur := a.dur, a.wroteDur, a.ttfbDur
	reused, err := a.reused, a.err
	a.mu.Unlock()

	t.mu.Lock()
	defer t.mu.Unlock()

	switch {
	case strings.HasPrefix(label, "fx"):
		t.fxTries++
		t.fxHTTP = status
		if apiCode != 0 {
			t.fxCode = apiCode
		}
	case strings.HasPrefix(label, "dl"):
		t.dlTries++
		t.dlHTTP = status
		t.dlBytes += bytes
		t.dlBody += body
		t.dlSync += syncDur
		t.dlRename += renameDur
		t.dlReused = reused
	case strings.HasPrefix(label, "tg"):
		t.upTries++
		t.upDur += dur
		kind := strings.TrimPrefix(label, "tg_")
		if strings.HasSuffix(kind, "_url") {
			t.upVia = "url"
			kind = strings.TrimSuffix(kind, "_url")
		} else {
			t.upVia = "file"
		}
		t.upKind = kind
		t.upHTTP = status
		if size > 0 {
			t.upSize = size
		}
		if wroteDur > 0 {
			t.upWrote = wroteDur
		}
		if ttfbDur > 0 {
			t.upTTFB = ttfbDur
		}
		if err != nil {
			t.upErr = err.Error()
		} else {
			t.upErr = ""
		}
	}
}

func (t *trace) emit(log *slog.Logger) {
	if t == nil || t.mode == timingOff {
		return
	}

	t.mu.Lock()
	recv := time.Since(t.start)
	poll := t.pollDelay
	queue := t.queueWait
	status := t.statusDur
	refs := t.refs
	fxDur, fxTries, fxSleep, fxHTTP, fxCode := t.fxDur, t.fxTries, t.fxSleep, t.fxHTTP, t.fxCode
	dlDur, dlTries, dlSleep, dlBytes := t.dlDur, t.dlTries, t.dlSleep, t.dlBytes
	dlBody, dlSync, dlRename, dlReused, dlHTTP := t.dlBody, t.dlSync, t.dlRename, t.dlReused, t.dlHTTP
	upDur, upTries, upKind, upVia, upSize := t.upDur, t.upTries, t.upKind, t.upVia, t.upSize
	upWrote, upTTFB, upRetry, upHTTP, upErr := t.upWrote, t.upTTFB, t.upRetryAfter, t.upHTTP, t.upErr
	edit := t.editDur
	skew, skewSet := t.skew, t.skewSet
	t.mu.Unlock()

	args := []any{
		"trace", t.id,
		"poll", fmtDur(poll),
		"queue", fmtDur(queue),
		"status", fmtDur(status),
		"refs", refs,
		"fx", fmtDur(fxDur),
		"fx_tries", fxTries,
		"fx_sleeps", fmtDur(fxSleep),
		"fx_http", fxHTTP,
		"fx_code", fxCode,
		"dl", fmtDur(dlDur),
		"dl_bytes", fmtBytes(dlBytes),
		"dl_rate", fmtRate(dlBytes, dlBody),
		"dl_tries", dlTries,
		"dl_sleeps", fmtDur(dlSleep),
		"dl_http", dlHTTP,
		"copy", fmtDur(dlBody),
		"fsync", fmtDur(dlSync),
		"rename", fmtDur(dlRename),
		"reuse", dlReused,
		"up", fmtDur(upDur),
		"up_kind", upKind,
		"up_via", upVia,
		"up_size", fmtBytes(upSize),
		"up_rate", fmtRate(upSize, upWrote),
		"up_tries", upTries,
		"up_http", upHTTP,
		"wrote", fmtDur(upWrote),
		"ttfb", fmtDur(upTTFB),
		"retry_after", fmtDur(upRetry),
		"edit", fmtDur(edit),
		"total", fmtDur(recv + poll),
		"recv", fmtDur(recv),
	}
	if skewSet {
		args = append(args, "skew", fmtDur(skew))
	} else {
		args = append(args, "skew", "unknown")
	}
	if upErr != "" {
		args = append(args, "up_err", upErr)
	}
	log.Info("TIMING summary", args...)
}

type attemptCtxKey struct{}

type attempt struct {
	trace *trace
	label string
	n     int
	start time.Time

	mu        sync.Mutex
	dnsStart  time.Time
	connStart time.Time
	tlsStart  time.Time
	dns       time.Duration
	tcp       time.Duration
	tls       time.Duration
	reused    bool
	wroteAt   time.Time
	ttfbAt    time.Time

	status    int
	apiCode   int
	bytes     int64
	size      int64
	body      time.Duration
	syncDur   time.Duration
	renameDur time.Duration
	dur       time.Duration
	wroteDur  time.Duration
	ttfbDur   time.Duration
	err       error
}

func newAttempt(ctx context.Context, label string, n int) *attempt {
	t := traceFrom(ctx)
	if t == nil {
		return nil
	}
	return &attempt{trace: t, label: label, n: n, start: time.Now()}
}

// beginAttempt 返回带 httptrace 的 context，供 fetch/download 这类我们自己的请求使用。
func beginAttempt(ctx context.Context, label string, n int) (context.Context, *attempt) {
	a := newAttempt(ctx, label, n)
	if a == nil {
		return ctx, nil
	}
	ctx = context.WithValue(ctx, attemptCtxKey{}, a)
	ctx = httptrace.WithClientTrace(ctx, a.hooks())
	return ctx, a
}

func attemptFrom(ctx context.Context) *attempt {
	a, _ := ctx.Value(attemptCtxKey{}).(*attempt)
	return a
}

// withAttemptTrace 供 contextTransport 使用：把上传 attempt 的 hooks 挂到基础 context 上。
func withAttemptTrace(ctx context.Context, a *attempt) context.Context {
	if a == nil {
		return ctx
	}
	return httptrace.WithClientTrace(ctx, a.hooks())
}

func (a *attempt) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			a.mu.Lock()
			a.dnsStart = time.Now()
			a.mu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			a.mu.Lock()
			if !a.dnsStart.IsZero() {
				a.dns += time.Since(a.dnsStart)
			}
			a.mu.Unlock()
		},
		ConnectStart: func(_, _ string) {
			a.mu.Lock()
			a.connStart = time.Now()
			a.mu.Unlock()
		},
		ConnectDone: func(_, _ string, err error) {
			a.mu.Lock()
			if err == nil && !a.connStart.IsZero() {
				a.tcp = time.Since(a.connStart)
			}
			a.mu.Unlock()
		},
		TLSHandshakeStart: func() {
			a.mu.Lock()
			a.tlsStart = time.Now()
			a.mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			a.mu.Lock()
			if err == nil && !a.tlsStart.IsZero() {
				a.tls = time.Since(a.tlsStart)
			}
			a.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			a.mu.Lock()
			a.reused = info.Reused
			a.mu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			a.mu.Lock()
			a.wroteAt = time.Now()
			a.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			a.mu.Lock()
			a.ttfbAt = time.Now()
			a.mu.Unlock()
		},
	}
}

func (a *attempt) setStatus(code int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.status = code
	a.mu.Unlock()
}

func (a *attempt) setSize(n int64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.size = n
	a.mu.Unlock()
}

func (a *attempt) setBytes(n int64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.bytes = n
	a.mu.Unlock()
}

func (a *attempt) setAPICode(code int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.apiCode = code
	a.mu.Unlock()
}

func (a *attempt) setBody(d time.Duration, n int64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.body = d
	a.bytes = n
	a.mu.Unlock()
}

func (a *attempt) setSync(d time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.syncDur = d
	a.mu.Unlock()
}

func (a *attempt) setRename(d time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.renameDur = d
	a.mu.Unlock()
}

func (a *attempt) finish(err error) {
	if a == nil {
		return
	}

	dur := time.Since(a.start)
	a.mu.Lock()
	a.dur = dur
	a.err = err
	if !a.wroteAt.IsZero() {
		a.wroteDur = a.wroteAt.Sub(a.start)
	}
	if !a.ttfbAt.IsZero() {
		a.ttfbDur = a.ttfbAt.Sub(a.start)
	}
	status, apiCode, bytes, size := a.status, a.apiCode, a.bytes, a.size
	body, syncDur, renameDur := a.body, a.syncDur, a.renameDur
	dns, tcp, tls, reused := a.dns, a.tcp, a.tls, a.reused
	wroteDur, ttfbDur := a.wroteDur, a.ttfbDur
	a.mu.Unlock()

	a.trace.recordAttempt(a)

	if a.trace.mode != timingVerbose {
		return
	}
	attrs := []any{"try", a.n, "status", status, "bytes", bytes}
	if apiCode != 0 {
		attrs = append(attrs, "api_code", apiCode)
	}
	if size > 0 {
		attrs = append(attrs, "size", fmtBytes(size))
	}
	if wroteDur > 0 {
		attrs = append(attrs, "wrote", fmtDur(wroteDur))
	}
	if ttfbDur > 0 {
		attrs = append(attrs, "ttfb", fmtDur(ttfbDur))
	}
	attrs = append(attrs, "reuse", reused, "dns", fmtDur(dns), "tcp", fmtDur(tcp), "tls", fmtDur(tls))
	if body > 0 {
		attrs = append(attrs, "body", fmtDur(body), "rate", fmtRate(bytes, body))
	}
	if syncDur > 0 {
		attrs = append(attrs, "fsync", fmtDur(syncDur))
	}
	if renameDur > 0 {
		attrs = append(attrs, "rename", fmtDur(renameDur))
	}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}
	a.trace.log.Info("TIMING http", append([]any{"trace", a.trace.id, "label", a.label}, attrs...)...)
}

// traceSlot 让不支持 context 的 tgbotapi 请求也能关联到当前上传 attempt。
// 只有单 worker 会写它；轮询使用另一个不带 slot 的 client。
type traceSlot struct {
	mu  sync.Mutex
	cur *attempt
}

func (s *traceSlot) set(a *attempt) func() {
	if s == nil {
		return func() {}
	}
	s.mu.Lock()
	prev := s.cur
	s.cur = a
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.cur = prev
		s.mu.Unlock()
	}
}

func (s *traceSlot) current() *attempt {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		return "-" + fmtDur(-d)
	}
	if d == 0 {
		return "0"
	}
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%dms", int64(d/time.Millisecond))
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}

func fmtBytes(n int64) string {
	if n <= 0 {
		return "0B"
	}
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func fmtRate(n int64, d time.Duration) string {
	if n <= 0 || d <= 0 {
		return "0B/s"
	}
	return fmtBytes(int64(float64(n)/d.Seconds())) + "/s"
}
