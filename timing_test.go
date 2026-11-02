package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestParseTimingMode(t *testing.T) {
	tests := []struct {
		in   string
		want timingMode
		ok   bool
	}{
		{"", timingOff, true},
		{"summary", timingSummary, true},
		{"OFF", timingOff, true},
		{" verbose ", timingVerbose, true},
		{"loud", timingOff, false},
	}
	for _, tt := range tests {
		got, err := parseTimingMode(tt.in)
		if (err == nil) != tt.ok {
			t.Errorf("parseTimingMode(%q) err = %v", tt.in, err)
			continue
		}
		if tt.ok && got != tt.want {
			t.Errorf("parseTimingMode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestTraceNilSafe(t *testing.T) {
	var tr *trace
	tr.mark("receive")
	tr.markDur("fetch_done", time.Second)
	tr.addFetch(time.Second)
	tr.addDownload(time.Second)
	tr.addUploadRetry(time.Second)
	tr.setStatus(time.Second, 0, time.Now())
	tr.emit(nil)

	if traceFrom(context.Background()) != nil {
		t.Fatal("traceFrom(background) should be nil")
	}
	ctx, at := beginAttempt(context.Background(), "fx", 1)
	if at != nil || ctx == nil {
		t.Fatalf("beginAttempt without trace = (%v, %v), want (ctx, nil)", ctx, at)
	}
	if newAttempt(context.Background(), "tg_photo", 1) != nil {
		t.Fatal("newAttempt without trace should be nil")
	}
}

func TestTraceSummary(t *testing.T) {
	var buf bytes.Buffer
	log := testLogger(&buf)
	now := time.Now()
	tr := newTrace(log, "42:7", now.Unix(), now, timingSummary)
	ctx := withTrace(context.Background(), tr)

	tr.enqueueNow()
	time.Sleep(2 * time.Millisecond)
	tr.enterWorker()
	tr.setRefs(1)
	tr.setStatus(120*time.Millisecond, now.Unix(), now.Add(-60*time.Millisecond))
	tr.addEdit(3 * time.Millisecond)

	tr.addFetch(150 * time.Millisecond)
	tr.addFetchSleep(2 * time.Second)
	fx := newAttempt(ctx, "fx", 1)
	fx.setStatus(http.StatusTooManyRequests)
	fx.setAPICode(429)
	fx.setBytes(512)
	fx.finish(nil)

	tr.addDownload(80 * time.Millisecond)
	dl := newAttempt(ctx, "dl", 1)
	dl.setStatus(http.StatusOK)
	dl.setBody(50*time.Millisecond, 4096)
	dl.setSync(7 * time.Millisecond)
	dl.setRename(time.Millisecond)
	dl.finish(nil)

	tg := newAttempt(ctx, "tg_photo", 1)
	tg.setSize(4096)
	tg.setStatus(http.StatusOK)
	tg.finish(nil)
	tr.addUploadRetry(60 * time.Second)

	tr.emit(log)
	out := buf.String()
	t.Log(out)
	for _, want := range []string{
		"TIMING summary",
		"trace=42:7",
		"fx_tries=1",
		"fx_sleeps=2.00s",
		"fx_http=429",
		"fx_code=429",
		"dl_tries=1",
		"dl_bytes=4.0KB",
		"copy=50ms",
		"fsync=7ms",
		"up_tries=1",
		"up_kind=photo",
		"retry_after=60.0s",
		"refs=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q in:\n%s", want, out)
		}
	}
}

func TestTraceVerboseSteps(t *testing.T) {
	var buf bytes.Buffer
	log := testLogger(&buf)
	now := time.Now()
	tr := newTrace(log, "1:2", now.Unix(), now, timingVerbose)
	ctx := withTrace(context.Background(), tr)

	tr.mark("fetch_start", "ref", "123")
	tr.markDur("fetch_done", 10*time.Millisecond, "tries", 2)
	at := newAttempt(ctx, "fx", 1)
	at.setStatus(200)
	at.setBytes(10)
	at.finish(nil)

	out := buf.String()
	for _, want := range []string{"TIMING step", "name=fetch_start", "name=fetch_done", "TIMING http", "label=fx"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose output missing %q in:\n%s", want, out)
		}
	}
}

func TestHTTPAttemptTraceReuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	tr := newTrace(testLogger(&bytes.Buffer{}), "1:1", time.Now().Unix(), time.Now(), timingVerbose)
	ctx := withTrace(context.Background(), tr)
	client := srv.Client()

	do := func() *attempt {
		actx, at := beginAttempt(ctx, "dl", 1)
		req, err := http.NewRequestWithContext(actx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		at.setStatus(resp.StatusCode)
		at.setBytes(int64(len(body)))
		at.finish(nil)
		return at
	}

	first := do()
	second := do()

	first.mu.Lock()
	firstReuse, firstStatus, firstBytes := first.reused, first.status, first.bytes
	first.mu.Unlock()
	second.mu.Lock()
	secondReuse, secondStatus, secondBytes := second.reused, second.status, second.bytes
	second.mu.Unlock()

	if firstReuse {
		t.Error("first request unexpectedly reused a connection")
	}
	if !secondReuse {
		t.Error("second request should reuse the keep-alive connection")
	}
	if firstStatus != http.StatusOK || secondStatus != http.StatusOK || firstBytes != 5 || secondBytes != 5 {
		t.Errorf("status/bytes = %d/%d %d/%d", firstStatus, firstBytes, secondStatus, secondBytes)
	}
}

func TestFetchRecordsEveryRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tr := newTrace(testLogger(&bytes.Buffer{}), "1:1", time.Now().Unix(), time.Now(), timingSummary)
	ctx := withTrace(context.Background(), tr)
	src := newFxTwitter(srv.URL, srv.Client())
	src.sleep = noSleep

	if _, err := src.Fetch(ctx, src.ref("9")); err == nil {
		t.Fatal("expected error")
	}

	tr.mu.Lock()
	tries, httpCode := tr.fxTries, tr.fxHTTP
	tr.mu.Unlock()
	if tries != fetchAttempts {
		t.Errorf("fxTries = %d, want %d", tries, fetchAttempts)
	}
	if httpCode != http.StatusInternalServerError {
		t.Errorf("fxHTTP = %d, want 500", httpCode)
	}
}

func TestFormatHelpers(t *testing.T) {
	if got := fmtDur(0); got != "0" {
		t.Errorf("fmtDur(0) = %q", got)
	}
	if got := fmtDur(1500 * time.Millisecond); got != "1.50s" {
		t.Errorf("fmtDur(1.5s) = %q", got)
	}
	if got := fmtDur(-time.Second); got != "-1.00s" {
		t.Errorf("fmtDur(-1s) = %q", got)
	}
	if got := fmtBytes(2048); got != "2.0KB" {
		t.Errorf("fmtBytes(2048) = %q", got)
	}
	if got := fmtRate(2048, time.Second); got != "2.0KB/s" {
		t.Errorf("fmtRate = %q", got)
	}
}

func TestProcessJobEmitsTimeline(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("jpeg-bytes"))
	}))
	defer mediaSrv.Close()
	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"tweet":{"id":"9","author":{"screen_name":"a"},"media":{"all":[{"type":"video","url":"` + mediaSrv.URL + `/v.mp4"}]}}}`))
	}))
	defer fxSrv.Close()

	app, _ := newTestApp(t, tgSrv, fxSrv.URL, mediaSrv.Client())
	var buf bytes.Buffer
	app.log = testLogger(&buf)
	app.cfg.timing = timingVerbose

	ref := app.sources.Extract("https://x.com/a/status/9")[0]
	tr := newTrace(app.log, "42:1", time.Now().Unix(), time.Now(), timingVerbose)
	app.processJob(context.Background(), job{chatID: 42, userMessageID: 1, refs: []Ref{ref}, trace: tr})

	out := buf.String()
	t.Log(out)
	for _, want := range []string{
		"TIMING step", "name=fetch_start", "name=fetch_done",
		"name=download_done", "name=send_done",
		"TIMING http", "label=fx", "label=dl",
		"TIMING summary", "trace=42:1", "up_kind=video",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pipeline timing missing %q in:\n%s", want, out)
		}
	}
	if !rec.has("sendVideo") {
		t.Errorf("sendVideo not called; methods = %v", rec.methods)
	}
}

func TestContextTransportAttachesAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tr := newTrace(testLogger(&bytes.Buffer{}), "1:1", time.Now().Unix(), time.Now(), timingSummary)
	ctx := withTrace(context.Background(), tr)
	slot := &traceSlot{}
	client := &http.Client{Transport: &contextTransport{ctx: context.Background(), base: srv.Client().Transport, slot: slot}}

	at := newAttempt(ctx, "tg_photo", 1)
	release := slot.set(at)
	resp, err := client.Post(srv.URL, "text/plain", strings.NewReader("hello"))
	release()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	recordUploadError(at, nil)
	at.finish(nil)

	at.mu.Lock()
	wrote, ttfb, status := at.wroteAt, at.ttfbAt, at.status
	at.mu.Unlock()
	if wrote.IsZero() || ttfb.IsZero() {
		t.Errorf("transport hooks not recorded: wrote=%v ttfb=%v", wrote, ttfb)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}

	tr.mu.Lock()
	tries := tr.upTries
	tr.mu.Unlock()
	if tries != 1 {
		t.Errorf("upTries = %d, want 1", tries)
	}
}

func TestProcessJobTimelineSendsPhotoByURL(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)

	var mediaCalls int32
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&mediaCalls, 1)
		_, _ = w.Write([]byte("jpeg-bytes"))
	}))
	defer mediaSrv.Close()
	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"tweet":{"id":"9","author":{"screen_name":"a"},"media":{"all":[{"type":"photo","url":"` + mediaSrv.URL + `/p.jpg"}]}}}`))
	}))
	defer fxSrv.Close()

	app, tmpDir := newTestApp(t, tgSrv, fxSrv.URL, mediaSrv.Client())
	var buf bytes.Buffer
	app.log = testLogger(&buf)
	app.cfg.timing = timingVerbose

	ref := app.sources.Extract("https://x.com/a/status/9")[0]
	tr := newTrace(app.log, "42:2", time.Now().Unix(), time.Now(), timingVerbose)
	app.processJob(context.Background(), job{chatID: 42, userMessageID: 1, refs: []Ref{ref}, trace: tr})

	out := buf.String()
	for _, want := range []string{
		"name=download_skip", "via=url", "up_via=url", "TIMING summary",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("URL photo timing missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "name=download_done") {
		t.Errorf("URL photo should not download:\n%s", out)
	}
	if got := atomic.LoadInt32(&mediaCalls); got != 0 {
		t.Errorf("media downloads = %d, want 0", got)
	}
	if got := rec.lastPhotoURL(); got != mediaSrv.URL+"/p.jpg" {
		t.Errorf("photo URL = %q, want %q", got, mediaSrv.URL+"/p.jpg")
	}
	if entries, _ := os.ReadDir(tmpDir); len(entries) != 0 {
		t.Errorf("temp dir not cleaned: %v", entries)
	}
}

func TestSendPhotoURLFallsBackToUpload(t *testing.T) {
	var urlCalls, uploadCalls, mediaCalls int32
	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"testbot"}}`)
		case "sendMessage", "editMessageText":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":42,"type":"private"},"text":"ok"}}`)
		case "deleteMessage":
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case "sendPhoto":
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				atomic.AddInt32(&uploadCalls, 1)
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":42,"type":"private"}}}`)
				return
			}
			atomic.AddInt32(&urlCalls, 1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"failed to get HTTP URL content"}`)
		default:
			t.Errorf("unexpected Telegram method %q", method)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"unexpected"}`)
		}
	}))
	defer tgSrv.Close()

	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&mediaCalls, 1)
		_, _ = w.Write([]byte("jpeg-bytes"))
	}))
	defer mediaSrv.Close()
	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"tweet":{"id":"9","author":{"screen_name":"a"},"media":{"all":[{"type":"photo","url":"` + mediaSrv.URL + `/p.jpg"}]}}}`))
	}))
	defer fxSrv.Close()

	app, tmpDir := newTestApp(t, tgSrv, fxSrv.URL, mediaSrv.Client())
	ref := app.sources.Extract("https://x.com/a/status/9")[0]
	app.processRef(context.Background(), 42, 1, ref, 0, 1)

	if got := atomic.LoadInt32(&urlCalls); got != 1 {
		t.Errorf("URL attempts = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&uploadCalls); got != 1 {
		t.Errorf("upload attempts = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&mediaCalls); got != 1 {
		t.Errorf("media downloads = %d, want 1", got)
	}
	if entries, _ := os.ReadDir(tmpDir); len(entries) != 0 {
		t.Errorf("temp dir not cleaned: %v", entries)
	}
}

func TestUploadTransportForcesHTTP1(t *testing.T) {
	tr := newUploadTransport()
	if tr.ForceAttemptHTTP2 {
		t.Error("upload transport should not attempt HTTP/2")
	}
	if tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 {
		t.Errorf("TLSNextProto = %v, want empty non-nil map", tr.TLSNextProto)
	}
}

func TestUploadTransportNegotiatesHTTP1(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := newUploadTransport()
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("NextProtos = %v, want [http/1.1]", got)
	}

	serverTLS := srv.Client().Transport.(*http.Transport).TLSClientConfig
	tr.TLSClientConfig.RootCAs = serverTLS.RootCAs
	tr.TLSClientConfig.InsecureSkipVerify = serverTLS.InsecureSkipVerify

	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Errorf("protocol = %s, want HTTP/1.1", resp.Proto)
	}
}
