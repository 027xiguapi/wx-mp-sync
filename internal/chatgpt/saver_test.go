package chatgpt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	scraper "wx-mp-sync/pkg/scraper/chatgpt"
)

// fakeFetcher 返回预置的分享内容。
type fakeFetcher struct {
	articles map[string]*scraper.Article
}

func (f *fakeFetcher) Fetch(_ context.Context, rawURL string) (*scraper.Article, error) {
	if a, ok := f.articles[rawURL]; ok {
		return a, nil
	}
	return nil, os.ErrNotExist
}

func imageServer(t *testing.T) *httptest.Server {
	t.Helper()
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
}

func newTestArticle(shareID string, imageURLs ...string) *scraper.Article {
	a := &scraper.Article{
		ShareID:   shareID,
		Title:     "测试 ChatGPT 分享",
		SourceURL: "https://chatgpt.com/s/" + shareID,
		CreatedAt: time.Date(2026, 11, 4, 16, 0, 0, 0, time.Local),
		Messages:  2,
	}
	var md strings.Builder
	md.WriteString("# 测试 ChatGPT 分享\n\n## 我\n\n你好\n\n## ChatGPT\n\n回答\n\n")
	for _, u := range imageURLs {
		a.Images = append(a.Images, scraper.Image{URL: u, Key: u})
		md.WriteString("![图片](" + u + ")\n\n")
	}
	a.Markdown = md.String()
	return a
}

// TestLiveChatGPTArchive 用真实分享链接验证完整归档(设置 CHATGPT_LIVE_URL 后才运行)。
func TestLiveChatGPTArchive(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("CHATGPT_LIVE_URL"))
	if rawURL == "" {
		t.Skip("未设置 CHATGPT_LIVE_URL,跳过真实 ChatGPT 归档测试")
	}
	client := scraper.NewClient()
	client.Proxy = strings.TrimSpace(os.Getenv("CHATGPT_PROXY"))
	archiver := NewArchiver(t.TempDir(), client, client.HTTPClient(), nil)
	result, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	t.Logf("目录=%s 图片=%d", result.Dir, result.Images)
	if result.Images == 0 {
		t.Error("没有下载到图片")
	}
	md, err := os.ReadFile(filepath.Join(result.Dir, result.Title+".md"))
	if err != nil {
		t.Fatalf("读取 Markdown 失败: %v", err)
	}
	if strings.Contains(string(md), "https://images.openai.com") {
		t.Error("Markdown 里仍有远程图片地址")
	}
}

func TestSaveChatGPTArchive(t *testing.T) {
	srv := imageServer(t)
	defer srv.Close()
	rawURL := "https://chatgpt.com/s/t_abc123"
	archiver := NewArchiver(t.TempDir(), &fakeFetcher{articles: map[string]*scraper.Article{
		rawURL: newTestArticle("t_abc123", srv.URL+"/1.png", srv.URL+"/2.png"),
	}}, srv.Client(), nil)

	result, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	if result.Duplicate || result.Images != 2 {
		t.Errorf("结果 = %+v", result)
	}
	if !strings.Contains(result.Dir, filepath.Join("ChatGPT", "2026-11", "测试 ChatGPT 分享")) {
		t.Errorf("目录 = %q", result.Dir)
	}

	md, err := os.ReadFile(filepath.Join(result.Dir, "测试 ChatGPT 分享.md"))
	if err != nil {
		t.Fatalf("读取 Markdown 失败: %v", err)
	}
	if strings.Contains(string(md), "http://127.0.0.1") {
		t.Errorf("Markdown 仍有远程图片地址:\n%s", md)
	}
	if !strings.Contains(string(md), "](images/01-") {
		t.Errorf("Markdown 未指向本地图片:\n%s", md)
	}

	data, _ := os.ReadFile(filepath.Join(result.Dir, "metadata.json"))
	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("解析 metadata 失败: %v", err)
	}
	if meta.ExternalID != "chatgpt:t_abc123" || meta.Images != 2 || meta.Source != "chatgpt" {
		t.Errorf("metadata = %+v", meta)
	}

	second, err := archiver.Save(context.Background(), rawURL)
	if err != nil || !second.Duplicate || second.Dir != result.Dir {
		t.Errorf("重复归档应跳过: %+v err=%v", second, err)
	}
}
