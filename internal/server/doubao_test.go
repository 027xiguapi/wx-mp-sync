package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	doubaoarchive "wx-mp-sync/internal/doubao"
	scraper "wx-mp-sync/pkg/scraper/doubao"
)

// fakeDoubaoArchiver 在临时目录里生成假的归档结果。
type fakeDoubaoArchiver struct {
	root string
	mu   sync.Mutex
	seen map[string]bool
	fail bool
}

func (f *fakeDoubaoArchiver) Save(_ context.Context, rawURL string) (*doubaoarchive.Result, error) {
	shareID, err := scraper.ParseShareID(rawURL)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, os.ErrPermission
	}
	title := "豆包-" + shareID
	dir := filepath.Join(f.root, title)
	duplicate := f.seen[shareID]
	f.seen[shareID] = true
	if !duplicate {
		if err := os.MkdirAll(filepath.Join(dir, "images"), 0755); err != nil {
			return nil, err
		}
		meta := map[string]any{
			"title":    title,
			"url":      rawURL,
			"markdown": title + ".md",
			"images":   1,
		}
		data, _ := json.Marshal(meta)
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, title+".md"), []byte("# "+title), 0644); err != nil {
			return nil, err
		}
	}
	return &doubaoarchive.Result{Dir: dir, Title: title, ShareID: shareID, Images: 1, Messages: 2, Duplicate: duplicate}, nil
}

// newDoubaoTestServer 构造带 fake 归档器的管理服务。
func newDoubaoTestServer(t *testing.T) (*Server, *fakeDoubaoArchiver) {
	t.Helper()
	logger := zerolog.Nop()
	s := New(t.TempDir(), "", nil, nil, &logger)
	fake := &fakeDoubaoArchiver{root: t.TempDir(), seen: map[string]bool{}}
	s.SetDoubaoArchiver(fake)
	return s, fake
}

// postDoubaoBatch 调用豆包批量创建接口。
func postDoubaoBatch(t *testing.T, s *Server, payload string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/doubao/batch", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleDoubaoBatchCreate(rec, req)
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
	}
	return res
}

// getDoubaoStatus 查询豆包任务进度。
func getDoubaoStatus(t *testing.T, s *Server, id string) DoubaoSnapshot {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/doubao/status?id="+id, nil)
	rec := httptest.NewRecorder()
	s.handleDoubaoStatus(rec, req)
	var res struct {
		OK  bool           `json:"ok"`
		Job DoubaoSnapshot `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || !res.OK {
		t.Fatalf("查询状态失败: %v body=%s", err, rec.Body.String())
	}
	return res.Job
}

// waitDoubao 等待任务结束。
func waitDoubao(t *testing.T, s *Server, id string) DoubaoSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := getDoubaoStatus(t, s, id)
		if snap.Finished {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("豆包任务 %s 超时未完成", id)
	return DoubaoSnapshot{}
}

// TestDoubaoBatchAndZip 验证批量创建、进度、ZIP 内容与清单。
func TestDoubaoBatchAndZip(t *testing.T) {
	s, _ := newDoubaoTestServer(t)
	payload := `{"urls":[
		"https://www.doubao.com/thread/xvAAA111",
		{"url":"https://www.doubao.com/thread/xvBBB222","title":"页面标题"},
		"https://www.doubao.com/thread/xvAAA111?from=copy",
		"https://example.com/thread/nope"
	]}`
	res := postDoubaoBatch(t, s, payload)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("创建任务失败: %v", res)
	}
	if total, _ := res["total"].(float64); total != 2 {
		t.Errorf("去重后条数 = %v, 期望 2", res["total"])
	}
	if dropped, _ := res["dropped"].(float64); dropped != 2 {
		t.Errorf("dropped = %v, 期望 2", res["dropped"])
	}

	jobID, _ := res["job_id"].(string)
	snap := waitDoubao(t, s, jobID)
	if snap.OK != 2 || snap.Failed != 0 {
		t.Fatalf("任务结果 = %+v", snap)
	}
	if snap.Items[0].Images != 1 || snap.Items[0].Title == "" {
		t.Errorf("条目信息不完整: %+v", snap.Items[0])
	}

	req := httptest.NewRequest(http.MethodGet, "/api/doubao/zip?id="+jobID, nil)
	rec := httptest.NewRecorder()
	s.handleDoubaoZip(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("打包失败: code=%d body=%s", rec.Code, rec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("解析 ZIP 失败: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{
		"_manifest.json",
		"豆包-xvAAA111/metadata.json",
		"豆包-xvAAA111/豆包-xvAAA111.md",
	} {
		if !names[want] {
			t.Errorf("ZIP 缺少 %q,实际: %v", want, names)
		}
	}

	manifest := readZipFile(t, zr, "_manifest.json")
	if !strings.Contains(manifest, `"total": 2`) || !strings.Contains(manifest, "xvAAA111") {
		t.Errorf("清单内容不正确: %s", manifest)
	}
}

// readZipFile 读取 ZIP 里的某个文件内容。
func readZipFile(t *testing.T, zr *zip.Reader, name string) string {
	t.Helper()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", name, err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		return string(data)
	}
	t.Fatalf("ZIP 里没有 %s", name)
	return ""
}

// TestDoubaoBatchFailures 验证失败条目的状态与空链接报错。
func TestDoubaoBatchFailures(t *testing.T) {
	s, fake := newDoubaoTestServer(t)
	fake.fail = true
	res := postDoubaoBatch(t, s, `{"urls":["https://www.doubao.com/thread/xvAAA111"]}`)
	jobID, _ := res["job_id"].(string)
	snap := waitDoubao(t, s, jobID)
	if snap.Failed != 1 || snap.Items[0].Error == "" {
		t.Errorf("失败任务快照 = %+v", snap)
	}

	// 没有可用链接时直接报错。
	res = postDoubaoBatch(t, s, `{"urls":["not-a-url","https://example.com/x"]}`)
	if ok, _ := res["ok"].(bool); ok {
		t.Errorf("非法链接应返回失败: %v", res)
	}

	// 打包没有成功条目时返回 JSON 404。
	req := httptest.NewRequest(http.MethodGet, "/api/doubao/zip?id="+jobID, nil)
	rec := httptest.NewRecorder()
	s.handleDoubaoZip(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("无成功条目时打包状态码 = %d", rec.Code)
	}
}

// TestLiveDoubaoHandler 用真实分享链接验证路由、归档与列表(设置 DOUBAO_LIVE_URL 后才运行)。
func TestLiveDoubaoHandler(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("DOUBAO_LIVE_URL"))
	if raw == "" {
		t.Skip("未设置 DOUBAO_LIVE_URL,跳过真实豆包接口测试")
	}
	logger := zerolog.Nop()
	outDir := t.TempDir()
	s := New(outDir, "", nil, nil, &logger)
	s.SetDoubaoArchiver(doubaoarchive.NewArchiver(outDir, &logger))
	handler := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/doubao/one", strings.NewReader(`{"url":"`+raw+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("单篇下载失败: code=%d ct=%s body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("解析 ZIP 失败: %v", err)
	}
	hasMD, hasMeta, images := false, false, 0
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".md") {
			hasMD = true
		}
		if strings.HasSuffix(f.Name, "metadata.json") {
			hasMeta = true
		}
		if strings.HasPrefix(f.Name, "images/") {
			images++
		}
	}
	if !hasMD || !hasMeta || images == 0 {
		t.Fatalf("ZIP 内容不完整: md=%v meta=%v images=%d", hasMD, hasMeta, images)
	}
	t.Logf("ZIP 内图片 %d 张", images)

	// 归档后应该出现在管理页列表里。
	listReq := httptest.NewRequest(http.MethodGet, "/api/list", nil)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	var listRes struct {
		OK   bool          `json:"ok"`
		List []ArticleInfo `json:"list"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listRes); err != nil || !listRes.OK {
		t.Fatalf("列表接口失败: %v body=%s", err, listRec.Body.String())
	}
	found := false
	for _, a := range listRes.List {
		if a.HTML == "" && a.Markdown != "" && strings.Contains(a.URL, "doubao.com") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("列表里没有豆包归档: %+v", listRes.List)
	}
}

// TestDoubaoOneZip 验证单篇下载接口直接返回 ZIP。
func TestDoubaoOneZip(t *testing.T) {
	s, _ := newDoubaoTestServer(t)
	body := `{"url":"https://www.doubao.com/thread/xvCCC333"}`
	req := httptest.NewRequest(http.MethodPost, "/api/doubao/one", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleDoubaoOne(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("单篇下载失败: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("解析 ZIP 失败: %v", err)
	}
	if len(zr.File) == 0 {
		t.Fatal("ZIP 为空")
	}

	// 非法链接返回 JSON 错误。
	rec = httptest.NewRecorder()
	s.handleDoubaoOne(rec, httptest.NewRequest(http.MethodPost, "/api/doubao/one", strings.NewReader(`{"url":"https://example.com/x"}`)))
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Errorf("非法链接未返回错误: %s", rec.Body.String())
	}
}
