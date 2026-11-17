package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"wx-mp-sync/internal/naicha"
	"wx-mp-sync/pkg/scraper/xhs"
)

const testTinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func TestHandleXHSSyncCapturedNote(t *testing.T) {
	s, root := newTestServer(t)

	imageData, err := base64.StdEncoding.DecodeString(testTinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	imageServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imageData)
	}))
	defer imageServer.Close()

	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/pow":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":503,"message":"当前未配置 PoW 验证码密钥"}`)
		case "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "bbs_session", Value: "session-token", Path: "/"})
			_, _ = io.WriteString(w, `{"code":0,"message":"success"}`)
		case "/api/upload":
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				t.Errorf("parse multipart: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if _, _, err := r.FormFile("file"); err != nil {
				t.Errorf("form file: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"urlPath":"/uploads/posts/posts-1.png"}}`)
		case "/api/posts/create":
			var body struct {
				Title     string   `json:"title"`
				Content   string   `json:"content"`
				BoardSlug string   `json:"boardSlug"`
				Tags      []string `json:"manualTags"`
				CoverPath string   `json:"coverPath"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.BoardSlug != "discussion" {
				t.Errorf("boardSlug = %q", body.BoardSlug)
			}
			if !contains(body.Tags, "小红书") || !contains(body.Tags, "奶茶") {
				t.Errorf("tags = %v", body.Tags)
			}
			if !strings.Contains(body.Content, "![图1](/uploads/posts/posts-1.png)") {
				t.Errorf("content missing image markdown: %q", body.Content)
			}
			if body.CoverPath != "/uploads/posts/posts-1.png" {
				t.Errorf("coverPath = %q", body.CoverPath)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"id":"post-1","status":"NORMAL"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer forum.Close()

	s.SetNaicha(naicha.New(naicha.Config{
		APIBase:    forum.URL,
		Username:   "admin",
		Password:   "secret",
		Board:      "discussion",
		StatePath:  filepath.Join(root, ".naicha_sync_state.json"),
		TimeoutSec: 10,
	}, s.logger))
	scraper := xhs.NewClient()
	scraper.HTTP = imageServer.Client()
	s.SetXHS(scraper, "discussion", []string{"奶茶"})

	payload := `{
	  "url": "https://www.xiaohongshu.com/explore/66f1a2b3c4d5e6f708192a3b",
	  "note": {
	    "noteId": "66f1a2b3c4d5e6f708192a3b",
	    "title": "一杯好喝的奶茶",
	    "desc": "今天喝了桂花乌龙,三分糖刚刚好。",
	    "user": {"nickname": "奶茶小分队"},
	    "imageList": [
	      {"urlDefault": "` + imageServer.URL + `/a.png", "width": 1080, "height": 1440}
	    ],
	    "tagList": [{"name": "奶茶"}]
	  }
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/xhs/sync", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleXHSSync(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var res struct {
		OK     bool   `json:"ok"`
		ID     string `json:"id"`
		URL    string `json:"url"`
		Title  string `json:"title"`
		NoteID string `json:"note_id"`
		Images int    `json:"images"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.ID != "post-1" || !strings.HasSuffix(res.URL, "/post/post-1") {
		t.Fatalf("unexpected response: %+v", res)
	}
	if res.NoteID != "66f1a2b3c4d5e6f708192a3b" || res.Title == "" || res.Error != "" {
		t.Fatalf("unexpected response: %+v", res)
	}
	if res.Images != 1 {
		t.Fatalf("images = %d", res.Images)
	}

	// 再次同步应命中去重。
	rec2 := httptest.NewRecorder()
	s.handleXHSSync(rec2, httptest.NewRequest(http.MethodPost, "/api/xhs/sync", strings.NewReader(payload)))
	if !strings.Contains(rec2.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate not detected: %s", rec2.Body.String())
	}
}

func TestHandleXHSSyncValidation(t *testing.T) {
	s, root := newTestServer(t)
	s.SetNaicha(naicha.New(naicha.Config{
		APIBase:    "http://127.0.0.1:1",
		Username:   "admin",
		Password:   "secret",
		StatePath:  filepath.Join(root, "state.json"),
		TimeoutSec: 1,
	}, s.logger))
	s.SetXHS(xhs.NewClient(), "discussion", nil)

	cases := []struct {
		name string
		body string
		code int
	}{
		{"empty", `{}`, http.StatusBadRequest},
		{"badurl", `{"url":"https://example.com/explore/abc"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/xhs/sync", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			s.handleXHSSync(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tc.code, rec.Body.String())
			}
		})
	}
}

func TestHandleXHSSyncDisabled(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/xhs/sync", strings.NewReader(`{"url":"https://www.xiaohongshu.com/explore/abc"}`))
	rec := httptest.NewRecorder()
	s.handleXHSSync(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "没有启用") {
		t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
