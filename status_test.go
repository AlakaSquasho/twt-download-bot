package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatusReporterEditsSameMessage(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)
	app, _ := newTestApp(t, tgSrv, tgSrv.URL, tgSrv.Client())
	app.statusProgressEvery = time.Nanosecond

	r := app.startStatus(context.Background(), 42, 99, "⏳ 正在获取推文…")
	if r.messageID == 0 {
		t.Fatal("status message was not created")
	}
	r.stage("⬇️ 正在下载媒体")
	time.Sleep(time.Millisecond)
	r.progress(50, 100)
	r.finish("✅ 完成")

	if got := rec.replyToByMethod()["sendMessage"]; len(got) == 0 || got[0] != 99 {
		t.Errorf("status reply_to = %v, want 99", got)
	}
	if !rec.hasText("⬇️ 正在下载媒体…") {
		t.Errorf("stage text missing; texts = %v", rec.texts)
	}
	if !rec.hasText("50%") {
		t.Errorf("progress text missing; texts = %v", rec.texts)
	}
	if got := rec.lastText(); got != "✅ 完成" {
		t.Errorf("final text = %q, want ✅ 完成", got)
	}
}

func TestStatusReporterThrottlesProgress(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)
	app, _ := newTestApp(t, tgSrv, tgSrv.URL, tgSrv.Client())
	app.statusProgressEvery = time.Hour

	r := app.startStatus(context.Background(), 42, 99, "⏳ 正在获取推文…")
	r.stage("⬇️ 正在下载媒体")
	before := rec.count("editMessageText")
	r.progress(10, 100)
	r.progress(90, 100)

	if got := rec.count("editMessageText"); got != before {
		t.Errorf("progress edits = %d, want %d within throttle window", got, before)
	}
}

func TestStatusReporterDeleteStopsEdits(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)
	app, _ := newTestApp(t, tgSrv, tgSrv.URL, tgSrv.Client())

	r := app.startStatus(context.Background(), 42, 99, "⏳ 正在获取推文…")
	r.delete()
	if !rec.has("deleteMessage") {
		t.Errorf("deleteMessage not called; methods = %v", rec.methods)
	}

	before := rec.count("editMessageText")
	r.stage("⬇️ 正在下载媒体")
	r.finish("✅ 完成")
	if got := rec.count("editMessageText"); got != before {
		t.Errorf("edits after delete = %d, want %d", got, before)
	}
}

func TestStatusReporterFallsBackToNewReply(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)
	app, _ := newTestApp(t, tgSrv, tgSrv.URL, tgSrv.Client())

	r := &statusReporter{
		api:     app.api,
		log:     app.log,
		chatID:  42,
		replyTo: 99,
	}
	r.finish("done")

	if !rec.hasText("done") {
		t.Errorf("final text missing; texts = %v", rec.texts)
	}
	if got := rec.replyToByMethod()["sendMessage"]; len(got) == 0 || got[0] != 99 {
		t.Errorf("fallback reply_to = %v, want 99", got)
	}
}

func TestProcessRefNoMediaEditsStatusIntoPostInfo(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)
	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":200,"tweet":{"id":"9","url":"https://x.com/a/status/9","text":"hello there","author":{"name":"Alice","screen_name":"a"}}}`)
	}))
	defer fxSrv.Close()

	app, _ := newTestApp(t, tgSrv, fxSrv.URL, tgSrv.Client())
	ref := app.sources.Extract("https://x.com/a/status/9")[0]
	app.processRef(context.Background(), 42, 99, ref, 0, 1)

	if rec.has("deleteMessage") {
		t.Errorf("status message must be kept for text-only posts; methods = %v", rec.methods)
	}
	last := rec.lastText()
	if !strings.Contains(last, `href="https://x.com/a"`) || !strings.Contains(last, "Alice(@a)") {
		t.Errorf("post info missing from final edit: %q", last)
	}
	if !strings.Contains(last, "hello there") || !strings.Contains(last, "Source") {
		t.Errorf("body or source missing from final edit: %q", last)
	}
	if got := rec.lastParseMode(); got != "HTML" {
		t.Errorf("final edit parse mode = %q, want HTML", got)
	}
}

func TestProcessRefUnavailableKeepsErrorStatus(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)
	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":404,"message":"not found"}`)
	}))
	defer fxSrv.Close()

	app, _ := newTestApp(t, tgSrv, fxSrv.URL, tgSrv.Client())
	ref := app.sources.Extract("https://x.com/a/status/9")[0]
	app.processRef(context.Background(), 42, 99, ref, 0, 1)

	if rec.has("deleteMessage") {
		t.Errorf("error status must not be deleted; methods = %v", rec.methods)
	}
	if got := rec.lastText(); !strings.Contains(got, "帖子不存在") {
		t.Errorf("error text = %q", got)
	}
}

func TestProgressReaderAccumulatesAcrossReaders(t *testing.T) {
	var done, last int64
	wrap := func(s string) *progressReader {
		return &progressReader{
			r:     strings.NewReader(s),
			done:  &done,
			total: 10,
			report: func(d, total int64) {
				last = d
			},
		}
	}
	if _, err := io.Copy(io.Discard, wrap("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, wrap("world")); err != nil {
		t.Fatal(err)
	}
	if done != 10 || last != 10 {
		t.Errorf("done = %d, last = %d, want 10", done, last)
	}
}
