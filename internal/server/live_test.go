package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"wx-mp-sync/internal/config"
	"wx-mp-sync/internal/store"
	"wx-mp-sync/internal/sync"
	"wx-mp-sync/pkg/scraper/wxmp"
)

// TestLiveSogou 用真实搜狗跳转链接验证"还原真实链接 + 归档"(WXMP_LIVE_SOGOU 未设置时跳过)。
func TestLiveSogou(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("WXMP_LIVE_SOGOU"))
	if raw == "" {
		t.Skip("未设置 WXMP_LIVE_SOGOU,跳过搜狗链接测试")
	}
	resolved, err := wxmp.ResolveSogouLink(raw)
	if err != nil {
		t.Fatalf("还原搜狗链接失败: %v", err)
	}
	t.Logf("resolved: %s", resolved)
	if !strings.Contains(resolved, "mp.weixin.qq.com") {
		t.Fatalf("还原结果不是公众号链接: %s", resolved)
	}

	root := t.TempDir()
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).Level(zerolog.DebugLevel)
	cfg := config.New("test", &logger)
	cfg.OutputDir = root
	st, err := store.New(filepath.Join(root, ".state.json"))
	if err != nil {
		t.Fatal(err)
	}
	saver := sync.NewArticleSaver(cfg, st, &logger)
	dir, dup, article, err := saver.SaveFromURL(raw)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	if article == nil || strings.TrimSpace(article.Title) == "" {
		t.Fatalf("文章标题为空: %+v", article)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	if len(files) == 0 {
		t.Errorf("缺少 Markdown")
	}
	imgs, _ := filepath.Glob(filepath.Join(dir, "images", "*"))
	t.Logf("title=%q duplicate=%v dir=%s markdown=%d images=%d", article.Title, dup, dir, len(files), len(imgs))
}

// TestLiveBatch 用真实文章链接跑一遍批量下载归档(WXMP_LIVE_URL 未设置时跳过)。
func TestLiveBatch(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("WXMP_LIVE_URL"))
	if raw == "" {
		t.Skip("未设置 WXMP_LIVE_URL,跳过真实抓取测试")
	}
	root := t.TempDir()
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).Level(zerolog.DebugLevel)
	cfg := config.New("test", &logger)
	cfg.OutputDir = root
	st, err := store.New(filepath.Join(root, ".state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(root, "", sync.NewArticleSaver(cfg, st, &logger), nil, &logger)
	s.SetBatchDelay(200 * time.Millisecond)

	payload, _ := json.Marshal(map[string]any{"urls": []string{raw}})
	req := httptest.NewRequest(http.MethodPost, "/api/batch", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	s.handleBatchCreate(rec, req)
	t.Logf("create: %s", rec.Body.String())
	var created struct {
		OK    bool   `json:"ok"`
		JobID string `json:"job_id"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !created.OK {
		t.Fatalf("创建失败: %v %s", err, rec.Body.String())
	}

	deadline := time.Now().Add(90 * time.Second)
	var snap BatchSnapshot
	for time.Now().Before(deadline) {
		snap = getStatus(t, s, created.JobID)
		if snap.Finished {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !snap.Finished {
		t.Fatalf("任务超时: %+v", snap)
	}
	t.Logf("job: ok=%d skip=%d fail=%d", snap.OK, snap.Skipped, snap.Failed)
	for _, it := range snap.Items {
		t.Logf("  %s %s err=%s", it.State, it.Title, it.Error)
		if it.Error != "" {
			t.Fatalf("抓取失败: %s", it.Error)
		}
	}

	// 验证归档产物:HTML + Markdown + 图片目录
	if len(snap.Items) == 0 || snap.Items[0].Dir == "" {
		t.Fatalf("没有归档目录: %+v", snap)
	}
	dir := snap.Items[0].Dir
	files, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	if len(files) == 0 {
		t.Errorf("缺少 Markdown: %v", files)
	} else {
		data, _ := os.ReadFile(files[0])
		if len(data) < 50 {
			t.Errorf("Markdown 内容过短: %d 字节", len(data))
		}
		t.Logf("markdown=%s (%d bytes)", filepath.Base(files[0]), len(data))
	}
	imgs, _ := filepath.Glob(filepath.Join(dir, "images", "*"))
	t.Logf("images=%d", len(imgs))
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		t.Errorf("缺少 index.html: %v", err)
	}

	zipReq := httptest.NewRequest(http.MethodGet, "/api/batch/zip?id="+created.JobID, nil)
	zipRec := httptest.NewRecorder()
	s.handleBatchZip(zipRec, zipReq)
	if zipRec.Code != http.StatusOK {
		t.Fatalf("打包失败: %d %s", zipRec.Code, zipRec.Body.String())
	}
	t.Logf("zip=%d bytes, ok=%s", zipRec.Body.Len(), zipRec.Header().Get("X-Batch-Ok"))
}
