package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"wx-mp-sync/internal/naicha"
)

// newNaichaArticle 在输出目录里造一篇可供同步的归档文章。
func newNaichaArticle(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "测试号", "2026-11", "测试文章")
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "post.md"), []byte("这是正文内容,足够长可以发布到论坛。\n\n![图](images/a.png)"), 0644); err != nil {
		t.Fatal(err)
	}
	// 1x1 PNG
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x04, 0x00, 0x00, 0x00, 0xb5, 0x1c, 0x0c, 0x02, 0x00, 0x00, 0x00,
		0x0b, 0x49, 0x44, 0x41, 0x54, 0x78, 0xda, 0x63, 0x64, 0xf8, 0x0f, 0x00,
		0x01, 0x05, 0x01, 0x01, 0x27, 0x18, 0xe3, 0x66, 0x00, 0x00, 0x00, 0x00,
		0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "a.png"), png, 0644); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"title":        "测试文章",
		"author":       "测试号",
		"creator":      "作者",
		"url":          "https://mp.weixin.qq.com/s?__biz=MzA1&mid=1&idx=1&sn=abc",
		"markdown":     "post.md",
		"publish_time": "2026年11月09日",
		"synced_at":    "2026-11-09T10:00:00+08:00",
	}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHandleNaichaSync(t *testing.T) {
	s, root := newTestServer(t)
	dir := newNaichaArticle(t, root)

	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/posts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		w.Header().Set("Location", "http://"+r.Host+"/t/7")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"post":{"id":7,"title":"测试文章","url":"http://` + r.Host + `/t/7"}}`))
	}))
	defer forum.Close()

	s.SetNaicha(naicha.New(naicha.Config{
		APIBase:    forum.URL,
		StatePath:  filepath.Join(root, ".naicha_sync_state.json"),
		TimeoutSec: 10,
	}, s.logger))

	rel, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader(`{"dir":"` + filepath.ToSlash(rel) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/naicha/sync", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleNaichaSync(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res struct {
		OK        bool   `json:"ok"`
		Duplicate bool   `json:"duplicate"`
		ID        int    `json:"id"`
		URL       string `json:"url"`
		Dir       string `json:"dir"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.ID != 7 || !strings.HasSuffix(res.URL, "/t/7") || res.Error != "" {
		t.Fatalf("unexpected response: %+v", res)
	}
	if filepath.ToSlash(rel) != res.Dir {
		t.Fatalf("dir = %q, want %q", res.Dir, filepath.ToSlash(rel))
	}

	// 列表接口应带上已发布状态。
	listReq := httptest.NewRequest(http.MethodGet, "/api/list", nil)
	listRec := httptest.NewRecorder()
	s.handleList(listRec, listReq)
	var list struct {
		OK   bool          `json:"ok"`
		List []ArticleInfo `json:"list"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if !list.OK || len(list.List) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if list.List[0].NaichaURL == "" {
		t.Fatalf("naicha_url missing: %+v", list.List[0])
	}

	// 重复同步应命中本地状态。
	req2 := httptest.NewRequest(http.MethodPost, "/api/naicha/sync", strings.NewReader(`{"dir":"`+filepath.ToSlash(rel)+`"}`))
	rec2 := httptest.NewRecorder()
	s.handleNaichaSync(rec2, req2)
	if !strings.Contains(rec2.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate not detected: %s", rec2.Body.String())
	}
}

func TestHandleNaichaSyncValidation(t *testing.T) {
	s, root := newTestServer(t)
	s.SetNaicha(naicha.New(naicha.Config{
		APIBase:    "http://127.0.0.1:1",
		StatePath:  filepath.Join(root, "state.json"),
		TimeoutSec: 1,
	}, s.logger))

	cases := []struct {
		name string
		body string
		code int
	}{
		{"empty", `{}`, http.StatusBadRequest},
		{"traversal", `{"dir":"../secret"}`, http.StatusBadRequest},
		{"absolute", `{"dir":"C:/Windows"}`, http.StatusBadRequest},
		{"missing", `{"dir":"不存在的目录"}`, http.StatusBadRequest},
		{"badurl", `{"url":"ftp://example.com/a"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/naicha/sync", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			s.handleNaichaSync(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tc.code, rec.Body.String())
			}
		})
	}
}

func TestHandleNaichaSyncDisabled(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/naicha/sync", strings.NewReader(`{"dir":"x"}`))
	rec := httptest.NewRecorder()
	s.handleNaichaSync(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "没有启用") {
		t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestFindArchivedByURL(t *testing.T) {
	s, root := newTestServer(t)
	dir := newNaichaArticle(t, root)
	want, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.ToSlash(want)
	got := s.findArchivedByURL("https://mp.weixin.qq.com/s?__biz=MzA1&mid=1&idx=1&sn=abc")
	if got != want {
		t.Fatalf("findArchivedByURL = %q, want %q", got, want)
	}
	if got := s.findArchivedByURL("https://mp.weixin.qq.com/s/other"); got != "" {
		t.Fatalf("unexpected archive match: %q", got)
	}
}

func TestHandleSaveForceNaicha(t *testing.T) {
	s, root := newTestServer(t)
	var hits int32
	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Location", "http://"+r.Host+"/t/9")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"post":{"id":9,"url":"http://` + r.Host + `/t/9"}}`))
	}))
	defer forum.Close()
	s.SetNaicha(naicha.New(naicha.Config{
		APIBase:    forum.URL,
		StatePath:  filepath.Join(root, ".naicha_sync_state.json"),
		TimeoutSec: 10,
	}, s.logger))

	payload := `{
	  "url": "https://mp.weixin.qq.com/s/test",
	  "title": "测试文章",
	  "nickname": "测试号",
	  "cgiDataNew": {
	    "title": "测试文章",
	    "nick_name": "测试号",
	    "create_time": "1760000000",
	    "content_noencode": "<p>这是正文内容,足够长可以发布到论坛。</p>"
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/save?naicha=1", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var res struct {
		OK     bool `json:"ok"`
		Naicha *struct {
			OK    bool   `json:"ok"`
			ID    int    `json:"id"`
			URL   string `json:"url"`
			Error string `json:"error"`
		} `json:"naicha"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Naicha == nil || !res.Naicha.OK || res.Naicha.ID != 9 {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("forum hits = %d", hits)
	}
}
