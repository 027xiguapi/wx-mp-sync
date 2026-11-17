package naicha

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func testLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func writePNG(t *testing.T, path string) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func newArticleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "images", "a.png"))
	md := "![配图](images/a.png)\n\n这是正文内容,用来测试同步文章到奶茶圈论坛。\n\n第二段。"
	if err := os.WriteFile(filepath.Join(dir, "post.md"), []byte(md), 0644); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"title":        "测试文章标题",
		"author":       "测试公众号",
		"creator":      "作者小茶",
		"url":          "https://mp.weixin.qq.com/s?__biz=MzA1&mid=100&idx=1&sn=abc&chksm=verylong&key=secret",
		"markdown":     "post.md",
		"publish_time": "2026年11月09日 10:00",
	}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSyncDirMultipartAndDedupe(t *testing.T) {
	dir := newArticleDir(t)
	var hits int32

	var got struct {
		title, content, category, tags, author string
		files                                  int
		urls                                   []string
		auth                                   string
	}
	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/api/posts" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		got.title = r.FormValue("title")
		got.content = r.FormValue("content")
		got.category = r.FormValue("category")
		got.tags = r.FormValue("tags")
		got.author = r.FormValue("author")
		got.auth = r.Header.Get("Authorization")
		got.urls = r.MultipartForm.Value["images"]
		got.files = len(r.MultipartForm.File["images"])

		w.Header().Set("Location", "/t/42")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"post":{"id":42,"title":"测试文章标题","url":"` + forumURL(r) + `/t/42"}}`))
	}))
	defer forum.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	client := New(Config{
		APIBase:    forum.URL,
		Token:      "secret-token",
		Category:   "group",
		Tags:       []string{"公众号", "奶茶"},
		TimeoutSec: 10,
		StatePath:  statePath,
	}, testLogger())

	res := client.SyncDir(context.Background(), dir, false)
	if !res.OK {
		t.Fatalf("SyncDir failed: %s", res.Message)
	}
	if res.ID != 42 || !strings.HasSuffix(res.URL, "/t/42") {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got.title != "测试文章标题" || got.author != "作者小茶" {
		t.Fatalf("fields wrong: %+v", got)
	}
	if got.category != "group" || got.tags != "公众号,奶茶" {
		t.Fatalf("category/tags wrong: %+v", got)
	}
	if got.auth != "Bearer secret-token" {
		t.Fatalf("auth header = %q", got.auth)
	}
	if got.files != 1 || len(got.urls) != 0 {
		t.Fatalf("images: files=%d urls=%v", got.files, got.urls)
	}
	if !strings.Contains(got.content, "原文：") || strings.Contains(got.content, "![") {
		t.Fatalf("content not cleaned: %q", got.content)
	}
	if textLen(got.content) > maxContentUnits {
		t.Fatalf("content too long: %d", textLen(got.content))
	}

	// 再次同步应走本地去重,不再请求论坛。
	res = client.SyncDir(context.Background(), dir, false)
	if !res.OK || !res.Duplicate {
		t.Fatalf("expected duplicate, got %+v", res)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("forum hit %d times, want 1", hits)
	}

	// 模拟进程重启:状态文件应能恢复去重。
	client2 := New(Config{APIBase: forum.URL, StatePath: statePath, TimeoutSec: 10}, testLogger())
	if rec, ok := client2.Lookup(dir); !ok || rec.ID != 42 {
		t.Fatalf("state not persisted: %+v ok=%v", rec, ok)
	}

	// force 应重新发布。
	res = client2.SyncDir(context.Background(), dir, true)
	if !res.OK || res.Duplicate {
		t.Fatalf("force sync failed: %+v", res)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("forum hit %d times, want 2", hits)
	}
}

// forumURL 从请求里拼出测试服务器地址(仅用于响应里的绝对 URL)。
func forumURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func TestSyncDirErrors(t *testing.T) {
	dir := newArticleDir(t)
	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"一个小时最多发布 10 个帖子"}`)
	}))
	defer forum.Close()

	client := New(Config{APIBase: forum.URL, TimeoutSec: 10}, testLogger())
	res := client.SyncDir(context.Background(), dir, false)
	if res.OK {
		t.Fatal("expected failure")
	}
	if !strings.Contains(res.Message, "429") || !strings.Contains(res.Message, "10") {
		t.Fatalf("unexpected error: %s", res.Message)
	}
}

func TestSyncDirValidationError(t *testing.T) {
	dir := newArticleDir(t)
	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"有几项还需要改一下。","errors":{"title":"标题需要在 4 到 80 个字之间。"}}`)
	}))
	defer forum.Close()

	client := New(Config{APIBase: forum.URL, TimeoutSec: 10}, testLogger())
	res := client.SyncDir(context.Background(), dir, false)
	if res.OK || !strings.Contains(res.Message, "title") || !strings.Contains(res.Message, "标题") {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSyncDirMissingArticle(t *testing.T) {
	client := New(Config{APIBase: "http://127.0.0.1:1", TimeoutSec: 1}, testLogger())
	res := client.SyncDir(context.Background(), filepath.Join(t.TempDir(), "missing"), false)
	if res.OK || !strings.Contains(res.Message, "读取文章目录失败") {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestTopicIDFromURL(t *testing.T) {
	cases := map[string]int{
		"http://localhost:3002/t/42":  42,
		"/t/7?x=1":                    7,
		"http://localhost:3002/p/abc": 0,
	}
	for raw, want := range cases {
		if got := topicIDFromURL(raw); got != want {
			t.Errorf("topicIDFromURL(%q)=%d want %d", raw, got, want)
		}
	}
}

func TestForumErrorFormat(t *testing.T) {
	err := forumError(http.StatusBadRequest, "400 Bad Request", []byte(`{"error":"有几项还需要改一下。","errors":{"content":"正文太短"}}`))
	if !strings.Contains(err.Error(), "content：正文太短") {
		t.Fatalf("unexpected error: %v", err)
	}
}
