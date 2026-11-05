package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type tgRecorder struct {
	mu           sync.Mutex
	methods      []string
	files        []string
	captions     []string
	captionModes []string
	photoURLs    []string
	mediaJSON    string
	replyToIDs   []int
	texts        []string
	parseModes   []string
}

func (r *tgRecorder) record(method string, files []string, caption, mediaJSON, parseMode string, replyTo int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.methods = append(r.methods, method)
	r.files = append(r.files, files...)
	r.replyToIDs = append(r.replyToIDs, replyTo)
	r.parseModes = append(r.parseModes, parseMode)
	if caption != "" {
		r.captions = append(r.captions, caption)
		r.captionModes = append(r.captionModes, parseMode)
	}
	if mediaJSON != "" {
		r.mediaJSON = mediaJSON
	}
}

func (r *tgRecorder) recordText(method, text string) {
	if text == "" || (method != "sendMessage" && method != "editMessageText") {
		return
	}
	r.mu.Lock()
	r.texts = append(r.texts, text)
	r.mu.Unlock()
}

func (r *tgRecorder) lastText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.texts) == 0 {
		return ""
	}
	return r.texts[len(r.texts)-1]
}

func (r *tgRecorder) hasText(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.texts {
		if strings.Contains(t, substr) {
			return true
		}
	}
	return false
}

func (r *tgRecorder) count(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.methods {
		if m == method {
			n++
		}
	}
	return n
}

func (r *tgRecorder) lastParseMode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.parseModes) == 0 {
		return ""
	}
	return r.parseModes[len(r.parseModes)-1]
}

func (r *tgRecorder) recordPhotoURL(u string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.photoURLs = append(r.photoURLs, u)
}

func (r *tgRecorder) lastPhotoURL() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.photoURLs) == 0 {
		return ""
	}
	return r.photoURLs[len(r.photoURLs)-1]
}

func (r *tgRecorder) has(method string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.methods {
		if m == method {
			return true
		}
	}
	return false
}

func (r *tgRecorder) replyToByMethod() map[string][]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string][]int)
	for i, method := range r.methods {
		m[method] = append(m[method], r.replyToIDs[i])
	}
	return m
}

func fakeTelegram(t *testing.T) (*httptest.Server, *tgRecorder) {
	t.Helper()
	rec := &tgRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")

		var files []string
		var caption, mediaJSON, photoURL, parseMode string
		var replyTo int
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(64 << 20); err != nil {
				t.Errorf("ParseMultipartForm: %v", err)
			} else {
				for _, fhs := range r.MultipartForm.File {
					for _, fh := range fhs {
						files = append(files, fh.Filename)
					}
				}
				caption = r.FormValue("caption")
				mediaJSON = r.FormValue("media")
				parseMode = r.FormValue("parse_mode")
				replyTo, _ = strconv.Atoi(r.FormValue("reply_to_message_id"))
			}
		} else {
			_ = r.ParseForm()
			caption = r.FormValue("caption")
			mediaJSON = r.FormValue("media")
			photoURL = r.FormValue("photo")
			parseMode = r.FormValue("parse_mode")
			replyTo, _ = strconv.Atoi(r.FormValue("reply_to_message_id"))
		}
		rec.record(method, files, caption, mediaJSON, parseMode, replyTo)
		rec.recordText(method, r.FormValue("text"))
		if photoURL != "" {
			rec.recordPhotoURL(photoURL)
		}

		switch method {
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"testbot"}}`)
		case "sendMediaGroup":
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"message_id":1},{"message_id":2}]}`)
		case "sendMessage", "editMessageText", "sendPhoto", "sendVideo", "sendDocument":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":42,"type":"private"},"text":"ok"}}`)
		case "deleteMessage":
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected Telegram method %q", method)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"unexpected"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newTestApp(t *testing.T, tg *httptest.Server, fxURL string, mediaClient *http.Client) (*botApp, string) {
	t.Helper()
	api, err := tgbotapi.NewBotAPIWithClient("123:abc", tg.URL+"/bot%s/%s", tg.Client())
	if err != nil {
		t.Fatalf("NewBotAPIWithClient: %v", err)
	}
	tmpDir := t.TempDir()
	app := &botApp{
		cfg:      config{maxUploadBytes: 50 << 20, tmpDir: tmpDir},
		api:      api,
		mediaAPI: api,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		fetcher: &mediaFetcher{
			client:   mediaClient,
			destDir:  tmpDir,
			maxBytes: 50 << 20,
			allow:    func(string) bool { return true },
			sleep:    noSleep,
		},
		sources: newRegistry(newFxTwitter(fxURL, tg.Client(), "")),
	}
	return app, tmpDir
}

func TestProcessRefSendsMediaGroup(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)

	var mediaCalls int32
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&mediaCalls, 1)
		_, _ = w.Write([]byte("media-bytes"))
	}))
	defer mediaSrv.Close()

	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":200,"tweet":{"id":"123","url":"https://x.com/u/status/123","author":{"screen_name":"u"},"media":{"all":[{"type":"photo","url":"%s/a.jpg"},{"type":"video","url":"%s/v.mp4"}]}}}`,
			mediaSrv.URL, mediaSrv.URL)
	}))
	defer fxSrv.Close()

	app, tmpDir := newTestApp(t, tgSrv, fxSrv.URL, mediaSrv.Client())
	ref := app.sources.Extract("https://x.com/u/status/123")[0]
	app.processRef(context.Background(), 42, 1, ref, 0, 1)

	if !rec.has("sendMediaGroup") {
		t.Errorf("sendMediaGroup not called; methods = %v", rec.methods)
	}
	if !rec.has("deleteMessage") {
		t.Errorf("status message was not deleted; methods = %v", rec.methods)
	}
	if got := atomic.LoadInt32(&mediaCalls); got != 1 {
		t.Errorf("media downloads = %d, want 1 (only the video; photo is sent by URL)", got)
	}
	if len(rec.files) != 1 || !strings.HasSuffix(rec.files[0], ".mp4") {
		t.Errorf("uploaded files = %v, want one .mp4", rec.files)
	}
	if !strings.Contains(rec.mediaJSON, mediaSrv.URL+"/a.jpg") {
		t.Errorf("photo URL missing from media JSON: %q", rec.mediaJSON)
	}
	if !strings.Contains(rec.mediaJSON, "https://x.com/u/status/123") {
		t.Errorf("caption missing from media JSON: %q", rec.mediaJSON)
	}
	if !strings.Contains(rec.mediaJSON, "u(@u)") || !strings.Contains(rec.mediaJSON, "Source") {
		t.Errorf("caption is not rendered post info: %q", rec.mediaJSON)
	}
	if got := rec.replyToByMethod()["sendMessage"]; len(got) == 0 || got[0] != 1 {
		t.Errorf("status reply_to = %v, want 1", got)
	}
	if got := rec.replyToByMethod()["sendMediaGroup"]; len(got) == 0 || got[0] != 1 {
		t.Errorf("media reply_to = %v, want 1", got)
	}
	if entries, _ := os.ReadDir(tmpDir); len(entries) != 0 {
		t.Errorf("temp dir not cleaned: %v", entries)
	}
}

func TestProcessRefSendsSingleVideo(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)

	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("mp4-bytes"))
	}))
	defer mediaSrv.Close()

	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":200,"tweet":{"id":"7","author":{"screen_name":"v"},"media":{"all":[{"type":"video","url":"%s/v.mp4"}]}}}`, mediaSrv.URL)
	}))
	defer fxSrv.Close()

	app, tmpDir := newTestApp(t, tgSrv, fxSrv.URL, mediaSrv.Client())
	ref := app.sources.Extract("https://x.com/v/status/7")[0]
	app.processRef(context.Background(), 42, 1, ref, 0, 1)

	if !rec.has("sendVideo") {
		t.Errorf("sendVideo not called; methods = %v", rec.methods)
	}
	if !rec.has("deleteMessage") {
		t.Errorf("status message was not deleted; methods = %v", rec.methods)
	}
	if len(rec.files) != 1 || !strings.HasSuffix(rec.files[0], ".mp4") {
		t.Errorf("uploaded files = %v", rec.files)
	}
	if entries, _ := os.ReadDir(tmpDir); len(entries) != 0 {
		t.Errorf("temp dir not cleaned: %v", entries)
	}
}

func TestSendSingleFallsBackToDocument(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"testbot"}}`)
		case "sendVideo":
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"wrong file identifier"}`)
		case "sendDocument":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":42,"type":"private"}}}`)
		default:
			t.Errorf("unexpected method %q", method)
		}
	}))
	defer srv.Close()

	api, err := tgbotapi.NewBotAPIWithClient("123:abc", srv.URL+"/bot%s/%s", srv.Client())
	if err != nil {
		t.Fatalf("NewBotAPIWithClient: %v", err)
	}
	path := t.TempDir() + "/v.mp4"
	if err := os.WriteFile(path, []byte("mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &botApp{
		api:      api,
		mediaAPI: api,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	item := downloadedMedia{media: Media{Type: MediaVideo}, path: path, size: 3}
	if err := app.sendSingle(context.Background(), 42, 0, item, "cap", nil); err != nil {
		t.Fatalf("sendSingle: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 3 || methods[0] != "getMe" || methods[1] != "sendVideo" || methods[2] != "sendDocument" {
		t.Errorf("methods = %v, want getMe, sendVideo, sendDocument", methods)
	}
}

func TestProcessRefSendsSinglePhoto(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)

	var mediaCalls int32
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&mediaCalls, 1)
		_, _ = w.Write([]byte("jpeg-bytes"))
	}))
	defer mediaSrv.Close()

	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":200,"tweet":{"id":"9","author":{"screen_name":"a"},"media":{"all":[{"type":"photo","url":"%s/p.jpg"}]}}}`, mediaSrv.URL)
	}))
	defer fxSrv.Close()

	app, tmpDir := newTestApp(t, tgSrv, fxSrv.URL, mediaSrv.Client())
	ref := app.sources.Extract("9")[0]
	app.processRef(context.Background(), 42, 1, ref, 0, 1)

	if !rec.has("sendPhoto") {
		t.Errorf("sendPhoto not called; methods = %v", rec.methods)
	}
	if !rec.has("deleteMessage") {
		t.Errorf("status message was not deleted; methods = %v", rec.methods)
	}
	if got := atomic.LoadInt32(&mediaCalls); got != 0 {
		t.Errorf("media downloads = %d, want 0 (photo should be sent by URL)", got)
	}
	if len(rec.files) != 0 {
		t.Errorf("uploaded files = %v, want none", rec.files)
	}
	if got := rec.lastPhotoURL(); got != mediaSrv.URL+"/p.jpg" {
		t.Errorf("photo URL = %q, want %q", got, mediaSrv.URL+"/p.jpg")
	}
	if len(rec.captions) == 0 || !strings.Contains(rec.captions[0], "https://x.com/a/status/9") {
		t.Errorf("captions = %v", rec.captions)
	}
	if entries, _ := os.ReadDir(tmpDir); len(entries) != 0 {
		t.Errorf("temp dir not cleaned: %v", entries)
	}
}

func TestProcessRefSendsTranslatedText(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)

	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":200,"tweet":{"id":"11","url":"https://x.com/t/status/11","text":"hello","author":{"name":"T","screen_name":"t"},"translation":{"text":"你好","source_lang":"en","source_lang_en":"English","provider":"grok"}}}`)
	}))
	defer fxSrv.Close()

	app, _ := newTestApp(t, tgSrv, fxSrv.URL, nil)
	app.sources = newRegistry(newFxTwitter(fxSrv.URL, tgSrv.Client(), "zh-Hans"))
	ref := app.sources.Extract("11")[0]
	app.processRef(context.Background(), 42, 1, ref, 0, 1)

	text := rec.lastText()
	for _, want := range []string{"翻译自英语", "你好", "<blockquote expandable>hello</blockquote>", "Source"} {
		if !strings.Contains(text, want) {
			t.Errorf("edited text missing %q: %q", want, text)
		}
	}
	if mode := rec.lastParseMode(); mode != "HTML" {
		t.Errorf("parse mode = %q, want HTML", mode)
	}
}

func TestProcessRefSendsTranslatedCaption(t *testing.T) {
	tgSrv, rec := fakeTelegram(t)

	fxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":200,"tweet":{"id":"12","url":"https://x.com/t/status/12","text":"hello","author":{"name":"T","screen_name":"t"},"translation":{"text":"你好","source_lang":"en","source_lang_en":"English","provider":"grok"},"media":{"all":[{"type":"photo","url":"https://pbs.twimg.com/media/a.jpg"}]}}}`)
	}))
	defer fxSrv.Close()

	app, _ := newTestApp(t, tgSrv, fxSrv.URL, nil)
	app.sources = newRegistry(newFxTwitter(fxSrv.URL, tgSrv.Client(), "zh-Hans"))
	ref := app.sources.Extract("12")[0]
	app.processRef(context.Background(), 42, 1, ref, 0, 1)

	if !rec.has("sendPhoto") {
		t.Fatalf("sendPhoto not called; methods = %v", rec.methods)
	}
	if len(rec.captions) == 0 {
		t.Fatal("caption missing")
	}
	caption := rec.captions[0]
	for _, want := range []string{"翻译自英语", "你好", "<blockquote expandable>hello</blockquote>", "Source"} {
		if !strings.Contains(caption, want) {
			t.Errorf("caption missing %q: %q", want, caption)
		}
	}
}
