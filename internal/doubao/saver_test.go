package doubao

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

	scraper "wx-mp-sync/pkg/scraper/doubao"
)

// fakeFetcher 返回预置的分享内容。
type fakeFetcher struct {
	articles map[string]*scraper.Article
	err      error
}

func (f *fakeFetcher) Fetch(_ context.Context, rawURL string) (*scraper.Article, error) {
	if f.err != nil {
		return nil, f.err
	}
	article, ok := f.articles[rawURL]
	if !ok {
		return nil, os.ErrNotExist
	}
	return article, nil
}

// imageServer 提供若干测试图片,404 的路径用于模拟下载失败。
func imageServer(t *testing.T) *httptest.Server {
	t.Helper()
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "bad") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
}

// newTestArticle 构造一份带两张图片的分享。
func newTestArticle(shareID string, imageURLs ...string) *scraper.Article {
	article := &scraper.Article{
		ShareID:   shareID,
		Title:     "测试 标题/含非法字符",
		Author:    "测试用户",
		SourceURL: "https://www.doubao.com/thread/" + shareID,
		CreatedAt: time.Date(2026, 11, 4, 15, 4, 0, 0, time.Local),
		Messages: []scraper.ChatMessage{
			{Role: "我", Text: "你好"},
			{Role: "豆包", Text: "这是回答"},
		},
	}
	var md strings.Builder
	md.WriteString("# 测试 标题/含非法字符\n\n## 我\n\n你好\n\n## 豆包\n\n这是回答\n\n")
	for _, u := range imageURLs {
		article.Images = append(article.Images, scraper.Image{URL: u, Caption: "图", Kind: "search"})
		md.WriteString("![图](" + u + ")\n\n")
	}
	article.Markdown = md.String()
	return article
}

// TestSaveArchivesMarkdownAndImages 验证目录结构、图片本地化与 metadata。
func TestSaveArchivesMarkdownAndImages(t *testing.T) {
	srv := imageServer(t)
	defer srv.Close()

	img1 := srv.URL + "/1.png"
	img2 := srv.URL + "/2.png"
	rawURL := "https://www.doubao.com/thread/abc123"
	archiver := NewArchiver(t.TempDir(), nil)
	archiver.Client = &fakeFetcher{articles: map[string]*scraper.Article{
		rawURL: newTestArticle("abc123", img1, img2),
	}}

	result, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	if result.Duplicate || result.Images != 2 {
		t.Errorf("结果 = %+v", result)
	}
	if !strings.Contains(result.Dir, filepath.Join("豆包", "2026-11", "测试 标题_含非法字符")) {
		t.Errorf("归档目录 = %q", result.Dir)
	}

	mdPath := filepath.Join(result.Dir, "测试 标题_含非法字符.md")
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("读取 Markdown 失败: %v", err)
	}
	if strings.Contains(string(md), img1) || strings.Contains(string(md), img2) {
		t.Errorf("Markdown 里仍保留远程图片地址:\n%s", md)
	}
	if !strings.Contains(string(md), "](images/01-") || !strings.Contains(string(md), "](images/02-") {
		t.Errorf("Markdown 未指向本地图片:\n%s", md)
	}

	images, err := os.ReadDir(filepath.Join(result.Dir, "images"))
	if err != nil || len(images) != 2 {
		t.Fatalf("images 目录 = %v, err=%v", images, err)
	}

	data, err := os.ReadFile(filepath.Join(result.Dir, "metadata.json"))
	if err != nil {
		t.Fatalf("读取 metadata 失败: %v", err)
	}
	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("解析 metadata 失败: %v", err)
	}
	if meta.ExternalID != "doubao:abc123" || meta.Images != 2 || meta.Source != "doubao" {
		t.Errorf("metadata = %+v", meta)
	}
	if meta.PublishTime != "2026年11月04日 15:04" {
		t.Errorf("publish_time = %q", meta.PublishTime)
	}
}

// TestSaveDuplicateAndRename 验证重复分享复用目录,同名不同分享自动加序号。
func TestSaveDuplicateAndRename(t *testing.T) {
	srv := imageServer(t)
	defer srv.Close()
	root := t.TempDir()
	archiver := NewArchiver(root, nil)
	rawURL := "https://www.doubao.com/thread/abc123"
	archiver.Client = &fakeFetcher{articles: map[string]*scraper.Article{
		rawURL: newTestArticle("abc123", srv.URL+"/1.png"),
	}}

	first, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("第一次归档失败: %v", err)
	}
	second, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("第二次归档失败: %v", err)
	}
	if !second.Duplicate || second.Dir != first.Dir {
		t.Errorf("重复归档应复用目录: first=%+v second=%+v", first, second)
	}

	otherURL := "https://www.doubao.com/thread/def456"
	archiver.Client = &fakeFetcher{articles: map[string]*scraper.Article{
		otherURL: newTestArticle("def456", srv.URL+"/1.png"),
	}}
	third, err := archiver.Save(context.Background(), otherURL)
	if err != nil {
		t.Fatalf("同名归档失败: %v", err)
	}
	if third.Dir == first.Dir || !strings.HasSuffix(third.Dir, "测试 标题_含非法字符-2") {
		t.Errorf("同名文章应另建目录: first=%q third=%q", first.Dir, third.Dir)
	}
}

// TestSaveKeepsRemoteURLWhenImageFails 验证图片下载失败时保留远程链接。
func TestSaveKeepsRemoteURLWhenImageFails(t *testing.T) {
	srv := imageServer(t)
	defer srv.Close()
	bad := srv.URL + "/bad.png"
	rawURL := "https://www.doubao.com/thread/abc123"
	archiver := NewArchiver(t.TempDir(), nil)
	archiver.Client = &fakeFetcher{articles: map[string]*scraper.Article{
		rawURL: newTestArticle("abc123", bad),
	}}

	result, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	if result.Images != 0 {
		t.Errorf("失败图片不应计数: %+v", result)
	}
	md, _ := os.ReadFile(filepath.Join(result.Dir, "测试 标题_含非法字符.md"))
	if !strings.Contains(string(md), bad) {
		t.Errorf("图片下载失败时应保留远程链接:\n%s", md)
	}
}

// TestSaveNoContent 验证空分享返回错误。
func TestSaveNoContent(t *testing.T) {
	rawURL := "https://www.doubao.com/thread/abc123"
	archiver := NewArchiver(t.TempDir(), nil)
	archiver.Client = &fakeFetcher{articles: map[string]*scraper.Article{
		rawURL: {ShareID: "abc123", Title: "空"},
	}}
	if _, err := archiver.Save(context.Background(), rawURL); err == nil {
		t.Fatal("空分享应返回错误")
	}
}

// TestSaveReArchivesIncomplete 验证上次只归档了正文(图片 0 张)时会重新归档补齐图片。
func TestSaveReArchivesIncomplete(t *testing.T) {
	srv := imageServer(t)
	defer srv.Close()
	rawURL := "https://www.doubao.com/thread/abc123"
	root := t.TempDir()
	archiver := NewArchiver(root, nil)

	// 第一次:图片地址不可用,归档出来 images=0。
	archiver.Client = &fakeFetcher{articles: map[string]*scraper.Article{
		rawURL: newTestArticle("abc123", srv.URL+"/bad.png"),
	}}
	first, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("第一次归档失败: %v", err)
	}
	if first.Images != 0 {
		t.Fatalf("第一次应没有图片: %+v", first)
	}

	// 第二次:同一分享能拿到两张图,应重新归档而不是跳过。
	archiver.Client = &fakeFetcher{articles: map[string]*scraper.Article{
		rawURL: newTestArticle("abc123", srv.URL+"/1.png", srv.URL+"/2.png"),
	}}
	second, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("第二次归档失败: %v", err)
	}
	if second.Duplicate {
		t.Errorf("图片数不足的旧归档不应判定为重复: %+v", second)
	}
	if second.Dir != first.Dir || second.Images != 2 {
		t.Errorf("应在原目录补齐图片: first=%+v second=%+v", first, second)
	}
	md, _ := os.ReadFile(filepath.Join(second.Dir, "测试 标题_含非法字符.md"))
	if !strings.Contains(string(md), "](images/01-") || !strings.Contains(string(md), "](images/02-") {
		t.Errorf("重新归档后 Markdown 未指向本地图片:\n%s", md)
	}
}

// TestLiveSave 用真实分享链接验证完整归档(设置 DOUBAO_LIVE_URL 后才运行)。
func TestLiveSave(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("DOUBAO_LIVE_URL"))
	if rawURL == "" {
		t.Skip("未设置 DOUBAO_LIVE_URL,跳过真实豆包归档测试")
	}
	root := t.TempDir()
	archiver := NewArchiver(root, nil)
	result, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	t.Logf("目录=%s 图片=%d 消息=%d", result.Dir, result.Images, result.Messages)
	if result.Images == 0 {
		t.Error("没有下载到图片")
	}
	md, err := os.ReadFile(filepath.Join(result.Dir, result.Title+".md"))
	if err != nil {
		t.Fatalf("读取 Markdown 失败: %v", err)
	}
	if strings.Contains(string(md), "byteimg.com") {
		t.Error("Markdown 里仍有豆包远程图片地址")
	}
	if _, err := os.Stat(filepath.Join(result.Dir, "metadata.json")); err != nil {
		t.Errorf("缺少 metadata.json: %v", err)
	}
}
