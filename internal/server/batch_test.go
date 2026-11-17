package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wx-mp-sync/pkg/scraper/wxmp"
)

// postBatch 调用批量创建接口并解析响应。
func postBatch(t *testing.T, s *Server, payload string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/batch", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleBatchCreate(rec, req)
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
	}
	return res
}

// getStatus 查询批量任务状态。
func getStatus(t *testing.T, s *Server, id string) BatchSnapshot {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/batch/status?id="+id, nil)
	rec := httptest.NewRecorder()
	s.handleBatchStatus(rec, req)
	var res struct {
		OK  bool          `json:"ok"`
		Job BatchSnapshot `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || !res.OK {
		t.Fatalf("查询状态失败: %v body=%s", err, rec.Body.String())
	}
	return res.Job
}

// waitBatch 等待任务结束,超时则失败。
func waitBatch(t *testing.T, s *Server, id string) BatchSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := getStatus(t, s, id)
		if snap.Finished {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("批量任务 %s 超时未完成", id)
	return BatchSnapshot{}
}

// TestNormalizeBatchURLs 验证链接清洗、去重与非法链接过滤。
func TestNormalizeBatchURLs(t *testing.T) {
	in := []batchURL{
		{URL: " https://mp.weixin.qq.com/s/aaa ", Title: "A"},
		{URL: "https://mp.weixin.qq.com/s/aaa"},
		{URL: "javascript:alert(1)"},
		{URL: ""},
		{URL: "https://mp.weixin.qq.com/s?__biz=Mz&mid=1&idx=1", Title: "B"},
	}
	items, dropped := normalizeBatchURLs(in)
	if len(items) != 2 {
		t.Fatalf("条目数 = %d, 期望 2 (%+v)", len(items), items)
	}
	if items[0].URL != "https://mp.weixin.qq.com/s/aaa" || items[0].Title != "A" {
		t.Errorf("第一条 = %+v", items[0])
	}
	if items[0].State != batchStatePending || items[1].Title != "B" {
		t.Errorf("条目状态/标题不正确: %+v", items)
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, 期望 2", dropped)
	}
	if _, dropped := normalizeBatchURLs([]batchURL{{URL: "ftp://example.com/a"}}); dropped != 1 {
		t.Errorf("非 http(s) 链接应被丢弃: dropped=%d", dropped)
	}
}

// TestBatchURLUnmarshal 验证 urls 同时支持字符串与对象两种写法。
func TestBatchURLUnmarshal(t *testing.T) {
	var req struct {
		URLs []batchURL `json:"urls"`
	}
	body := `{"urls":["https://mp.weixin.qq.com/s/a",{"url":"https://mp.weixin.qq.com/s/b","title":"标题B"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(req.URLs) != 2 || req.URLs[0].URL == "" || req.URLs[1].Title != "标题B" {
		t.Fatalf("解析结果 = %+v", req.URLs)
	}
}

// TestUniqueFolderName 验证 ZIP 内同名目录自动加序号(大小写不敏感)。
func TestUniqueFolderName(t *testing.T) {
	used := map[string]bool{}
	names := []string{
		uniqueFolderName("标题", used),
		uniqueFolderName("标题", used),
		uniqueFolderName("标题", used),
		uniqueFolderName("", used),
		uniqueFolderName("", used),
	}
	want := []string{"标题", "标题-2", "标题-3", "article", "article-2"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("第 %d 个 = %q, 期望 %q", i, names[i], want[i])
		}
	}
}

// fakeArticle 在输出目录下造一篇假文章并返回目录。
func fakeArticle(t *testing.T, root, title string) string {
	t.Helper()
	dir := filepath.Join(root, "测试号", "2026-10", title)
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>"+title+"</html>"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "a.jpg"), []byte("img"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	meta := fmt.Sprintf(`{"title":%q,"url":"https://mp.weixin.qq.com/s/x"}`, title)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(meta), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

// TestBatchCreateStatusZip 覆盖批量任务完整流程:创建 → 逐篇归档 → 打包。
func TestBatchCreateStatusZip(t *testing.T) {
	s, root := newTestServer(t)
	s.SetBatchDelay(time.Millisecond)

	var calls int32
	s.saveURL = func(rawURL string) (string, bool, *wxmp.WechatOfficialArticle, error) {
		n := atomic.AddInt32(&calls, 1)
		if strings.HasSuffix(rawURL, "bad") {
			return "", false, nil, fmt.Errorf("抓取失败")
		}
		title := fmt.Sprintf("文章%d", n)
		dir := fakeArticle(t, root, title)
		return dir, rawURL == "https://mp.weixin.qq.com/s/dup", &wxmp.WechatOfficialArticle{Title: title}, nil
	}

	res := postBatch(t, s, `{"urls":[
	  "https://mp.weixin.qq.com/s/one",
	  {"url":"https://mp.weixin.qq.com/s/dup","title":"重复"},
	  "https://mp.weixin.qq.com/s/bad",
	  "https://mp.weixin.qq.com/s/one"
	]}`)
	if res["ok"] != true {
		t.Fatalf("创建任务失败: %v", res)
	}
	id, _ := res["job_id"].(string)
	if id == "" || res["total"].(float64) != 3 {
		t.Fatalf("响应 = %v", res)
	}

	snap := waitBatch(t, s, id)
	if snap.OK != 1 || snap.Skipped != 1 || snap.Failed != 1 || snap.Done != 3 {
		t.Fatalf("统计 = ok:%d skip:%d fail:%d done:%d", snap.OK, snap.Skipped, snap.Failed, snap.Done)
	}
	if snap.Items[2].State != batchStateFailed || snap.Items[2].Error == "" {
		t.Errorf("失败条目未记录原因: %+v", snap.Items[2])
	}

	req := httptest.NewRequest(http.MethodGet, "/api/batch/zip?id="+id, nil)
	rec := httptest.NewRecorder()
	s.handleBatchZip(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("打包状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Header().Get("X-Batch-Ok") != "2" {
		t.Errorf("X-Batch-Ok = %q", rec.Header().Get("X-Batch-Ok"))
	}

	names := zipNames(t, rec.Body.Bytes())
	for name := range names {
		if !strings.Contains(name, "/") && name != "_manifest.json" {
			t.Errorf("ZIP 根目录出现散落文件: %s", name)
		}
	}
	for _, want := range []string{
		"_manifest.json",
		"文章1/index.html",
		"文章1/images/a.jpg",
		"文章2/index.html",
	} {
		if !names[want] {
			t.Errorf("ZIP 缺少 %s,实际: %v", want, names)
		}
	}

	// 清单里应记录失败条目
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	var manifest struct {
		Total int `json:"total"`
		Items []struct {
			State string `json:"state"`
			Error string `json:"error"`
		} `json:"items"`
	}
	for _, f := range zr.File {
		if f.Name != "_manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开清单失败: %v", err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("解析清单失败: %v", err)
		}
	}
	if manifest.Total != 3 || len(manifest.Items) != 3 {
		t.Fatalf("清单 = %+v", manifest)
	}
}

// TestBatchZipEmpty 验证没有任何成功文章时返回 JSON 错误而不是空 ZIP。
func TestBatchZipEmpty(t *testing.T) {
	s, _ := newTestServer(t)
	s.saveURL = func(string) (string, bool, *wxmp.WechatOfficialArticle, error) {
		return "", false, nil, fmt.Errorf("抓取失败")
	}
	res := postBatch(t, s, `{"urls":["https://mp.weixin.qq.com/s/bad"]}`)
	id := res["job_id"].(string)
	waitBatch(t, s, id)

	req := httptest.NewRequest(http.MethodGet, "/api/batch/zip?id="+id, nil)
	rec := httptest.NewRecorder()
	s.handleBatchZip(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["ok"] != false {
		t.Fatalf("响应 = %s", rec.Body.String())
	}
}

// TestBatchCancel 验证取消后剩余条目状态为 canceled。
func TestBatchCancel(t *testing.T) {
	s, root := newTestServer(t)
	release := make(chan struct{})
	s.saveURL = func(string) (string, bool, *wxmp.WechatOfficialArticle, error) {
		<-release
		dir := fakeArticle(t, root, "第一篇")
		return dir, false, &wxmp.WechatOfficialArticle{Title: "第一篇"}, nil
	}

	res := postBatch(t, s, `{"urls":[
	  "https://mp.weixin.qq.com/s/a",
	  "https://mp.weixin.qq.com/s/b",
	  "https://mp.weixin.qq.com/s/c"
	]}`)
	id := res["job_id"].(string)

	// 等第一篇进入 running,再取消。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := getStatus(t, s, id)
		if len(snap.Items) > 0 && snap.Items[0].State == batchStateRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/batch/cancel?id="+id, nil)
	cancelRec := httptest.NewRecorder()
	s.handleBatchCancel(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", cancelRec.Code, cancelRec.Body.String())
	}
	close(release)

	snap := waitBatch(t, s, id)
	if !snap.Canceled {
		t.Errorf("任务未标记取消")
	}
	for _, item := range snap.Items[1:] {
		if item.State != batchStateCanceled {
			t.Errorf("取消后条目状态 = %q, 期望 canceled", item.State)
		}
	}
}

// TestBatchCreateRejectsBadInput 验证缺少链接 / 非法请求被拒绝。
func TestBatchCreateRejectsBadInput(t *testing.T) {
	s, _ := newTestServer(t)
	for _, payload := range []string{`{"urls":[]}`, `{"urls":["not-a-url"]}`, `not-json`} {
		res := postBatch(t, s, payload)
		if res["ok"] != false {
			t.Errorf("%s 应被拒绝,实际 %v", payload, res)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/batch", nil)
	rec := httptest.NewRecorder()
	s.handleBatchCreate(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 状态码 = %d, 期望 405", rec.Code)
	}
}
