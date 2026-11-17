package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	chatgptarchive "wx-mp-sync/internal/chatgpt"
	scraper "wx-mp-sync/pkg/scraper/chatgpt"
)

// fakeChatGPTArchiver 生成假的 ChatGPT 归档目录。
type fakeChatGPTArchiver struct {
	root string
	mu   sync.Mutex
	seen map[string]bool
}

func (f *fakeChatGPTArchiver) Save(_ context.Context, rawURL string) (*chatgptarchive.Result, error) {
	id, err := scraper.ParseShareID(rawURL)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	title := "ChatGPT-" + id
	dir := filepath.Join(f.root, title)
	duplicate := f.seen[id]
	f.seen[id] = true
	if !duplicate {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
		meta, _ := json.Marshal(map[string]any{"title": title, "url": rawURL, "markdown": title + ".md"})
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), meta, 0644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, title+".md"), []byte("# "+title), 0644); err != nil {
			return nil, err
		}
	}
	return &chatgptarchive.Result{Dir: dir, Title: title, ShareID: id, Images: 3, Messages: 2, Duplicate: duplicate}, nil
}

func newChatGPTTestServer(t *testing.T) *Server {
	t.Helper()
	logger := zerolog.Nop()
	s := New(t.TempDir(), "", nil, nil, &logger)
	s.SetChatGPTArchiver(&fakeChatGPTArchiver{root: t.TempDir(), seen: map[string]bool{}})
	return s
}

// TestChatGPTBatchAndZip 验证批量任务、进度与 ZIP。
func TestChatGPTBatchAndZip(t *testing.T) {
	s := newChatGPTTestServer(t)
	body := `{"urls":[
		"https://chatgpt.com/s/t_abc12345",
		{"url":"https://chatgpt.com/s/t_def67890","title":"标题"},
		"https://chatgpt.com/share/t_abc12345",
		"https://example.com/s/t_bad12345"
	]}`
	req := httptest.NewRequest(http.MethodPost, "/api/chatgpt/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleChatGPTBatchCreate(rec, req)
	var res struct {
		OK      bool   `json:"ok"`
		JobID   string `json:"job_id"`
		Total   int    `json:"total"`
		Dropped int    `json:"dropped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || !res.OK {
		t.Fatalf("创建任务失败: %v body=%s", err, rec.Body.String())
	}
	if res.Total != 2 || res.Dropped != 2 {
		t.Errorf("total/dropped = %d/%d, 期望 2/2", res.Total, res.Dropped)
	}

	deadline := time.Now().Add(5 * time.Second)
	var snap BatchSnapshot
	for time.Now().Before(deadline) {
		statusReq := httptest.NewRequest(http.MethodGet, "/api/batch/status?id="+res.JobID, nil)
		statusRec := httptest.NewRecorder()
		s.handleBatchStatus(statusRec, statusReq)
		var status struct {
			OK  bool          `json:"ok"`
			Job BatchSnapshot `json:"job"`
		}
		if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil || !status.OK {
			t.Fatalf("查询状态失败: %v", err)
		}
		snap = status.Job
		if snap.Finished {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !snap.Finished || snap.OK != 2 {
		t.Fatalf("任务结果 = %+v", snap)
	}
	if snap.Items[0].Images != 3 {
		t.Errorf("图片数 = %d, 期望 3", snap.Items[0].Images)
	}

	zipReq := httptest.NewRequest(http.MethodGet, "/api/batch/zip?id="+res.JobID, nil)
	zipRec := httptest.NewRecorder()
	s.handleBatchZip(zipRec, zipReq)
	if zipRec.Code != http.StatusOK {
		t.Fatalf("打包失败: %d %s", zipRec.Code, zipRec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(zipRec.Body.Bytes()), int64(zipRec.Body.Len()))
	if err != nil {
		t.Fatalf("解析 ZIP 失败: %v", err)
	}
	found := false
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".md") {
			found = true
		}
	}
	if !found {
		t.Errorf("ZIP 里没有 Markdown")
	}
}

// TestChatGPTOneZip 验证单篇下载接口返回 ZIP。
func TestChatGPTOneZip(t *testing.T) {
	s := newChatGPTTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/chatgpt/one", strings.NewReader(`{"url":"https://chatgpt.com/s/t_abc12345"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleChatGPTOne(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("单篇下载失败: code=%d ct=%s body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if _, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err != nil {
		t.Fatalf("解析 ZIP 失败: %v", err)
	}

	badRec := httptest.NewRecorder()
	s.handleChatGPTOne(badRec, httptest.NewRequest(http.MethodPost, "/api/chatgpt/one", strings.NewReader(`{"url":"https://example.com/s/x"}`)))
	if !strings.Contains(badRec.Body.String(), `"ok":false`) {
		t.Errorf("非法链接未返回错误: %s", badRec.Body.String())
	}

	// 导航模式(CSP 兜底)返回 HTML 错误页,而不是 JSON。
	navRec := httptest.NewRecorder()
	navReq := httptest.NewRequest(http.MethodGet, "/api/chatgpt/one?nav=1&url=https://example.com/s/x", nil)
	navReq.Header.Set("Accept", "text/html,application/xhtml+xml")
	s.handleChatGPTOne(navRec, navReq)
	if ct := navRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("导航模式 Content-Type = %q", ct)
	}
	if !strings.Contains(navRec.Body.String(), "下载失败") {
		t.Errorf("导航模式错误页缺少提示: %s", navRec.Body.String())
	}
}
