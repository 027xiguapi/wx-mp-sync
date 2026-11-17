package naicha

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func tinyPNGData(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fakeNaichaServer 模拟新论坛的登录 / 上传 / 发帖接口。
func fakeNaichaServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	const challenge = "abcd-1-9999999999-2-login-ip-1-sig"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/pow", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"captchaToken": challenge,
				"difficulty":   2,
				"expiresAt":    9999999999,
			},
		})
	})
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		nonce, _ := body["powNonce"].(float64)
		if body["captchaToken"] == nil || nonce == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":400,"message":"请先完成工作量证明验证"}`)
			return
		}
		sum := sha256.Sum256([]byte("abcd" + formatNonce(int64(nonce))))
		if !strings.HasPrefix(hex.EncodeToString(sum[:]), "00") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":400,"message":"工作量证明验证失败"}`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "session-token", Path: "/"})
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success"})
	})
	mux.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookieName); err != nil || c.Value != "session-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":401,"message":"请先登录"}`)
			return
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.FormValue("folder") != "posts" {
			t.Errorf("folder = %q", r.FormValue("folder"))
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		if !strings.HasSuffix(header.Filename, ".png") {
			t.Errorf("filename = %q", header.Filename)
		}
		atomic.AddInt32(hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"urlPath": "/uploads/posts/posts-1.png"},
		})
	})
	mux.HandleFunc("/api/posts/create", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookieName); err != nil || c.Value != "session-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":401,"message":"请先登录"}`)
			return
		}
		var body struct {
			Title     string   `json:"title"`
			Content   string   `json:"content"`
			BoardSlug string   `json:"boardSlug"`
			Tags      []string `json:"manualTags"`
			CoverPath string   `json:"coverPath"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.BoardSlug != "discussion" || body.Title == "" || body.CoverPath == "" {
			t.Errorf("unexpected create payload: %+v", body)
		}
		if !strings.Contains(body.Content, "![图1](/uploads/posts/posts-1.png)") {
			t.Errorf("content missing image markdown: %q", body.Content)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"id": "post-123", "status": "NORMAL"},
		})
	})
	return httptest.NewServer(mux)
}

func formatNonce(nonce int64) string {
	if nonce == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for nonce > 0 {
		i--
		buf[i] = byte('0' + nonce%10)
		nonce /= 10
	}
	return string(buf[i:])
}

func TestPublishWithAdminLoginAndUpload(t *testing.T) {
	var uploads int32
	server := fakeNaichaServer(t, &uploads)
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	client := New(Config{
		APIBase:    server.URL,
		Username:   "admin",
		Password:   "secret",
		Board:      "discussion",
		TimeoutSec: 10,
		StatePath:  statePath,
	}, testLogger())

	post := &PublishPost{
		Title:     "小红书笔记标题",
		Content:   "这是小红书笔记的正文内容,用来测试同步。",
		Tags:      []string{"小红书", "奶茶"},
		DedupeKey: "xhs:abc",
		Images: []PostImage{{
			Filename: "note.png",
			Data:     tinyPNGData(t),
		}},
	}

	res := client.Publish(context.Background(), post, false)
	if !res.OK {
		t.Fatalf("Publish failed: %s", res.Message)
	}
	if res.ID != "post-123" || !strings.HasSuffix(res.URL, "/post/post-123") {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Images != 1 || atomic.LoadInt32(&uploads) != 1 {
		t.Fatalf("images=%d uploads=%d", res.Images, uploads)
	}

	// 去重:再次发布不应再上传或发帖。
	res = client.Publish(context.Background(), post, false)
	if !res.OK || !res.Duplicate || res.ID != "post-123" {
		t.Fatalf("expected duplicate: %+v", res)
	}
	if atomic.LoadInt32(&uploads) != 1 {
		t.Fatalf("duplicate triggered upload: %d", uploads)
	}

	// force 应重新上传并发帖。
	res = client.Publish(context.Background(), post, true)
	if !res.OK || res.Duplicate {
		t.Fatalf("force publish failed: %+v", res)
	}
	if atomic.LoadInt32(&uploads) != 2 {
		t.Fatalf("force uploads = %d", uploads)
	}
}

func TestSyncDirUsesAdminLogin(t *testing.T) {
	dir := newArticleDir(t)
	var uploads int32
	server := fakeNaichaServer(t, &uploads)
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	client := New(Config{
		APIBase:    server.URL,
		Username:   "admin",
		Password:   "secret",
		Board:      "discussion",
		Tags:       []string{"公众号"},
		TimeoutSec: 10,
		StatePath:  statePath,
	}, testLogger())

	res := client.SyncDir(context.Background(), dir, false)
	if !res.OK || res.PostID != "post-123" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if atomic.LoadInt32(&uploads) != 1 {
		t.Fatalf("uploads = %d", uploads)
	}
	rec, ok := client.Lookup(dir)
	if !ok || rec.PostID != "post-123" || rec.URL == "" {
		t.Fatalf("state not persisted: %+v ok=%v", rec, ok)
	}
}

func TestPublishRetriesAfterSessionExpired(t *testing.T) {
	var uploads int32
	server := fakeNaichaServer(t, &uploads)
	defer server.Close()

	client := New(Config{
		APIBase:    server.URL,
		Username:   "admin",
		Password:   "secret",
		TimeoutSec: 10,
	}, testLogger())

	// 预置一个失效 Cookie;第一次上传返回 401 后应清空并重新登录。
	client.session = sessionCookieName + "=expired"
	res := client.Publish(context.Background(), &PublishPost{
		Title:   "短标题",
		Content: "正文内容足够长,用于测试会话失效后的重试。",
		Images: []PostImage{{
			Filename: "note.png",
			Data:     tinyPNGData(t),
		}},
	}, true)
	if !res.OK {
		t.Fatalf("Publish after expired session failed: %s", res.Message)
	}
	if atomic.LoadInt32(&uploads) != 1 {
		t.Fatalf("uploads = %d", uploads)
	}
}

func TestLoginCaptchaError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/auth/login" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":400,"message":"请先完成图形验证码验证"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := New(Config{APIBase: server.URL, Username: "admin", Password: "secret", TimeoutSec: 5}, testLogger())
	res := client.Publish(context.Background(), &PublishPost{
		Title:   "标题",
		Content: "正文内容足够长,用于测试登录验证码失败提示。",
	}, true)
	if res.OK {
		t.Fatal("expected failure")
	}
	if !strings.Contains(res.Message, "图形验证码") || !strings.Contains(res.Message, "bbs_session") {
		t.Fatalf("unexpected message: %s", res.Message)
	}
}

func TestPublishWithBotSecret(t *testing.T) {
	var calls int32
	const secret = "bot-secret-123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bot/posts" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&calls, 1)
		if got := r.Header.Get("x-bot-secret"); got != secret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":401,"message":"机器人 API 密钥无效"}`)
			return
		}
		var body struct {
			Bot       string   `json:"bot"`
			Title     string   `json:"title"`
			Content   string   `json:"content"`
			BoardSlug string   `json:"boardSlug"`
			Tags      []string `json:"manualTags"`
			CoverPath string   `json:"coverPath"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Bot != "bot-account" || body.BoardSlug != "discussion" || body.Title == "" {
			t.Errorf("unexpected bot payload: %+v", body)
		}
		if len(body.Tags) != 2 || body.CoverPath != "" {
			t.Errorf("unexpected tags/cover: %+v", body)
		}
		if !strings.Contains(body.Content, "![图1](https://sns-img.example.com/a.jpg)") {
			t.Errorf("content missing image link: %q", body.Content)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    0,
			"message": "发布成功",
			"data":    map[string]any{"id": "bot-post-1", "status": "NORMAL", "url": "/post/bot-post-1"},
		})
	}))
	defer server.Close()

	client := New(Config{
		APIBase:    server.URL,
		Bot:        "bot-account",
		BotSecret:  secret,
		Board:      "discussion",
		TimeoutSec: 10,
		StatePath:  filepath.Join(t.TempDir(), "state.json"),
	}, testLogger())
	if !client.BotReady() {
		t.Fatal("BotReady() = false")
	}

	post := &PublishPost{
		Title:     "小红书笔记标题",
		Content:   "这是小红书笔记的正文内容,用来测试机器人发帖。",
		Tags:      []string{"小红书", "奶茶"},
		DedupeKey: "xhs:bot-abc",
		Images:    []PostImage{{URL: "https://sns-img.example.com/a.jpg"}},
	}
	res := client.Publish(context.Background(), post, false)
	if !res.OK {
		t.Fatalf("Publish failed: %s", res.Message)
	}
	if res.ID != "bot-post-1" || res.URL != server.URL+"/post/bot-post-1" || res.Images != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// 去重:再次发布不应再请求论坛。
	res = client.Publish(context.Background(), post, false)
	if !res.OK || !res.Duplicate {
		t.Fatalf("expected duplicate: %+v", res)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("bot endpoint called %d times, want 1", calls)
	}
}

func TestPublishWithBotSecretRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":401,"message":"机器人 API 密钥无效"}`)
	}))
	defer server.Close()

	client := New(Config{
		APIBase:    server.URL,
		Bot:        "bot-account",
		BotSecret:  "wrong",
		TimeoutSec: 5,
	}, testLogger())
	res := client.Publish(context.Background(), &PublishPost{
		Title:   "测试标题",
		Content: "正文内容足够长,用于测试密钥错误提示。",
	}, true)
	if res.OK {
		t.Fatal("expected failure")
	}
	if !strings.Contains(res.Message, "BOT_API_SECRET") {
		t.Fatalf("unexpected message: %s", res.Message)
	}
}
