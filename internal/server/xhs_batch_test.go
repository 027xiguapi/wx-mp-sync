package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	xhsarchive "wx-mp-sync/internal/xhsarchive"
	"wx-mp-sync/pkg/scraper/xhs"
)

// fakeXHSArchiver 用临时目录模拟小红书归档。
type fakeXHSArchiver struct {
	root  string
	mu    sync.Mutex
	seen  map[string]bool
	fail  bool
	calls int
}

func (f *fakeXHSArchiver) Save(_ context.Context, rawURL string) (*xhsarchive.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, fmt.Errorf("模拟失败")
	}
	id, err := xhs.ParseNoteID(rawURL)
	if err != nil {
		return nil, err
	}
	title := "笔记-" + id[:6]
	dir := filepath.Join(f.root, "小红书", "2026-11", title)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, title+".md"), []byte("# "+title), 0644); err != nil {
		return nil, err
	}
	meta := fmt.Sprintf(`{"title":%q,"url":%q,"markdown":%q}`, title, rawURL, title+".md")
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(meta), 0644); err != nil {
		return nil, err
	}
	duplicate := f.seen[id]
	f.seen[id] = true
	return &xhsarchive.Result{Dir: dir, Title: title, NoteID: id, Images: 1, Duplicate: duplicate}, nil
}

// newXHSTestServer 构造带 fake 归档器的管理服务。
func newXHSTestServer(t *testing.T) (*Server, *fakeXHSArchiver) {
	t.Helper()
	logger := zerolog.Nop()
	s := New(t.TempDir(), "", nil, nil, &logger)
	s.SetBatchDelay(time.Millisecond)
	fake := &fakeXHSArchiver{root: t.TempDir(), seen: map[string]bool{}}
	s.SetXHSArchiver(fake)
	return s, fake
}

func postXHSBatch(t *testing.T, s *Server, payload string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/xhs/batch", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleXHSBatchCreate(rec, req)
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
	}
	return res
}

// TestNormalizeXHSBatchURLs 验证从分享文本提取链接、保留 token、按笔记 ID 去重。
func TestNormalizeXHSBatchURLs(t *testing.T) {
	const idA = "6af55bb7000000002b01a361"
	const idB = "6aeb09dd0000000003016c8f"
	in := []batchURL{
		{URL: "看看这个 https://www.xiaohongshu.com/discovery/item/" + idA + "?xsec_token=abc，复制后打开"},
		{URL: "https://www.xiaohongshu.com/discovery/item/" + idA + "?xsec_token=other"},
		{URL: "https://xhslink.com/a/abcdef"},
		{URL: "https://example.com/not-xhs"},
		{URL: "https://www.xiaohongshu.com/explore/" + idB},
	}
	items, dropped := normalizeXHSBatchURLs(in)
	if len(items) != 3 {
		t.Fatalf("条目数 = %d, 期望 3 (%+v)", len(items), items)
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, 期望 2", dropped)
	}
	if !strings.Contains(items[0].URL, "xsec_token=abc") || strings.Contains(items[0].URL, "复制后打开") {
		t.Errorf("完整链接与 token 未保留: %q", items[0].URL)
	}
	if items[2].URL != "https://www.xiaohongshu.com/explore/"+idB {
		t.Errorf("第三条 = %q", items[2].URL)
	}
}

// TestXHSBatchAndZip 验证创建任务、进度与 ZIP 打包。
func TestXHSBatchAndZip(t *testing.T) {
	s, fake := newXHSTestServer(t)
	const idA = "6af55bb7000000002b01a361"
	const idB = "6aeb09dd0000000003016c8f"
	payload := `{"urls":[
		"https://www.xiaohongshu.com/discovery/item/` + idA + `?xsec_token=abc",
		{"url":"https://www.xiaohongshu.com/discovery/item/` + idB + `?xsec_token=def","title":"页面标题"},
		"https://www.xiaohongshu.com/discovery/item/` + idA + `?xsec_token=other",
		"https://example.com/not-xhs"
	]}`
	res := postXHSBatch(t, s, payload)
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
	snap := waitBatch(t, s, jobID)
	if snap.OK != 2 || snap.Failed != 0 {
		t.Fatalf("任务结果 = %+v", snap)
	}
	if snap.Items[0].Images != 1 || snap.Items[0].Title == "" {
		t.Errorf("条目信息不完整: %+v", snap.Items[0])
	}
	if fake.calls != 2 {
		t.Errorf("归档调用次数 = %d, 期望 2", fake.calls)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/xhs/zip?id="+jobID, nil)
	rec := httptest.NewRecorder()
	s.handleBatchZip(rec, req)
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
		"笔记-6af55b/笔记-6af55b.md",
		"笔记-6af55b/metadata.json",
		"笔记-6aeb09/笔记-6aeb09.md",
	} {
		if !names[want] {
			t.Errorf("ZIP 缺少 %q,实际: %v", want, names)
		}
	}
	manifest := readZipFile(t, zr, "_manifest.json")
	if !strings.Contains(manifest, `"total": 2`) || !strings.Contains(manifest, idA) {
		t.Errorf("清单内容不正确: %s", manifest)
	}
}

// TestXHSBatchDuplicate 验证第二次下载同一篇笔记标记为 skipped。
func TestXHSBatchDuplicate(t *testing.T) {
	s, _ := newXHSTestServer(t)
	const id = "6af55bb7000000002b01a361"
	payload := `{"urls":["https://www.xiaohongshu.com/discovery/item/` + id + `?xsec_token=a"]}`

	first := postXHSBatch(t, s, payload)
	if snap := waitBatch(t, s, first["job_id"].(string)); snap.OK != 1 {
		t.Fatalf("第一次任务结果 = %+v", snap)
	}
	second := postXHSBatch(t, s, payload)
	if snap := waitBatch(t, s, second["job_id"].(string)); snap.Skipped != 1 {
		t.Fatalf("第二次任务结果 = %+v", snap)
	}
}

// TestXHSBatchValidation 验证未初始化、空链接和非法链接的提示。
func TestXHSBatchValidation(t *testing.T) {
	logger := zerolog.Nop()
	s := New(t.TempDir(), "", nil, nil, &logger)
	res := postXHSBatch(t, s, `{"urls":["https://www.xiaohongshu.com/explore/6af55bb7000000002b01a361"]}`)
	if ok, _ := res["ok"].(bool); ok {
		t.Errorf("归档器未初始化时应失败: %v", res)
	}

	s, _ = newXHSTestServer(t)
	res = postXHSBatch(t, s, `{"urls":[]}`)
	if ok, _ := res["ok"].(bool); ok {
		t.Errorf("空链接应失败: %v", res)
	}
	res = postXHSBatch(t, s, `{"urls":["https://example.com/a"]}`)
	if ok, _ := res["ok"].(bool); ok {
		t.Errorf("非法链接应失败: %v", res)
	}
}

// TestXHSBatchFailure 验证归档失败会记录到任务状态。
func TestXHSBatchFailure(t *testing.T) {
	s, fake := newXHSTestServer(t)
	fake.fail = true
	res := postXHSBatch(t, s, `{"urls":["https://www.xiaohongshu.com/explore/6af55bb7000000002b01a361"]}`)
	jobID, _ := res["job_id"].(string)
	snap := waitBatch(t, s, jobID)
	if snap.Failed != 1 || snap.Items[0].Error == "" {
		t.Fatalf("任务结果 = %+v", snap)
	}
}
