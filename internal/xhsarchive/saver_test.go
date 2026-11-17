package xhsarchive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wx-mp-sync/pkg/scraper/xhs"
)

// fakeFetcher 返回预置的笔记,图片下载由 archiver.download 替换。
type fakeFetcher struct {
	notes map[string]*xhs.Note
	err   error
}

func (f *fakeFetcher) Fetch(_ context.Context, rawURL string) (*xhs.Note, error) {
	if f.err != nil {
		return nil, f.err
	}
	note, ok := f.notes[rawURL]
	if !ok {
		return nil, os.ErrNotExist
	}
	return note, nil
}

func (f *fakeFetcher) DownloadImage(_ context.Context, _ string) ([]byte, string, error) {
	return nil, "", fmt.Errorf("测试应使用 archiver.download")
}

var testPNG = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}

// newTestNote 构造一篇带图片和话题标签的笔记。
func newTestNote(id string, imageURLs ...string) *xhs.Note {
	note := &xhs.Note{
		ID:          id,
		URL:         "https://www.xiaohongshu.com/discovery/item/" + id,
		Title:       "测试 标题/含非法字符",
		Desc:        "2款#茶百道咖啡[话题]# 新品11月19日上新\n\n#茶百道[话题]# #探店[话题]#",
		Author:      "测试用户",
		AuthorID:    "user-1",
		PublishedAt: "2026-11-04",
		Tags:        []string{"茶百道", "探店"},
	}
	for _, u := range imageURLs {
		note.Images = append(note.Images, xhs.Image{URL: u, Width: 1080, Height: 1440})
	}
	return note
}

// testDownloader 对包含 bad 的地址返回错误,其余返回 PNG。
func testDownloader(_ context.Context, imageURL string) ([]byte, string, error) {
	if strings.Contains(imageURL, "bad") {
		return nil, "", fmt.Errorf("模拟下载失败")
	}
	return testPNG, "image/png", nil
}

func TestSaveArchivesMarkdownAndImages(t *testing.T) {
	root := t.TempDir()
	rawURL := "https://www.xiaohongshu.com/discovery/item/abc123"
	archiver := NewArchiver(root, nil, nil)
	archiver.Client = &fakeFetcher{notes: map[string]*xhs.Note{
		rawURL: newTestNote("abc123", "https://img.example/1.png", "https://img.example/2.png"),
	}}
	archiver.download = testDownloader

	result, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	if result.Duplicate || result.Images != 2 || result.FailedImages != 0 {
		t.Errorf("结果 = %+v", result)
	}
	if !strings.Contains(result.Dir, filepath.Join("小红书", "2026-11", "测试 标题_含非法字符")) {
		t.Errorf("归档目录 = %q", result.Dir)
	}

	mdPath := filepath.Join(result.Dir, "测试 标题_含非法字符.md")
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("读取 Markdown 失败: %v", err)
	}
	text := string(md)
	if strings.Contains(text, "[话题]#") || strings.Contains(text, "#茶百道") {
		t.Errorf("话题标记未清理:\n%s", text)
	}
	if !strings.Contains(text, "2款新品11月19日上新") {
		t.Errorf("正文不正确:\n%s", text)
	}
	if strings.Contains(text, "img.example") {
		t.Errorf("Markdown 里仍保留远程图片地址:\n%s", text)
	}
	if !strings.Contains(text, "](images/01-") || !strings.Contains(text, "](images/02-") {
		t.Errorf("Markdown 未指向本地图片:\n%s", text)
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
	if meta.ExternalID != "xhs:abc123" || meta.Source != "xhs" || meta.NoteID != "abc123" {
		t.Errorf("metadata = %+v", meta)
	}
	if meta.Images != 2 || meta.Author != "测试用户" || meta.PublishTime != "2026-11-04" {
		t.Errorf("metadata = %+v", meta)
	}
	if len(meta.Tags) != 2 || meta.Tags[0] != "茶百道" {
		t.Errorf("tags = %+v", meta.Tags)
	}
}

func TestSaveDuplicateAndRename(t *testing.T) {
	root := t.TempDir()
	archiver := NewArchiver(root, nil, nil)
	rawURL := "https://www.xiaohongshu.com/discovery/item/abc123"
	archiver.Client = &fakeFetcher{notes: map[string]*xhs.Note{
		rawURL: newTestNote("abc123", "https://img.example/1.png"),
	}}
	archiver.download = testDownloader

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

	otherURL := "https://www.xiaohongshu.com/discovery/item/def456"
	archiver.Client = &fakeFetcher{notes: map[string]*xhs.Note{
		otherURL: newTestNote("def456", "https://img.example/1.png"),
	}}
	third, err := archiver.Save(context.Background(), otherURL)
	if err != nil {
		t.Fatalf("同名归档失败: %v", err)
	}
	if third.Dir == first.Dir || !strings.HasSuffix(third.Dir, "测试 标题_含非法字符-2") {
		t.Errorf("同名笔记应另建目录: first=%q third=%q", first.Dir, third.Dir)
	}
}

func TestSaveKeepsRemoteURLWhenImageFails(t *testing.T) {
	root := t.TempDir()
	rawURL := "https://www.xiaohongshu.com/discovery/item/abc123"
	archiver := NewArchiver(root, nil, nil)
	archiver.Client = &fakeFetcher{notes: map[string]*xhs.Note{
		rawURL: newTestNote("abc123", "https://img.example/1.png", "https://img.example/bad.png"),
	}}
	archiver.download = testDownloader

	result, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	if result.Images != 1 || result.FailedImages != 1 {
		t.Errorf("结果 = %+v", result)
	}

	md, err := os.ReadFile(filepath.Join(result.Dir, "测试 标题_含非法字符.md"))
	if err != nil {
		t.Fatalf("读取 Markdown 失败: %v", err)
	}
	text := string(md)
	if !strings.Contains(text, "](images/01-") {
		t.Errorf("成功图片未本地化:\n%s", text)
	}
	if !strings.Contains(text, "https://img.example/bad.png") {
		t.Errorf("失败图片应保留远程地址:\n%s", text)
	}
}

func TestSaveReArchivesIncomplete(t *testing.T) {
	root := t.TempDir()
	rawURL := "https://www.xiaohongshu.com/discovery/item/abc123"
	archiver := NewArchiver(root, nil, nil)
	archiver.Client = &fakeFetcher{notes: map[string]*xhs.Note{
		rawURL: newTestNote("abc123", "https://img.example/1.png"),
	}}

	archiver.download = func(_ context.Context, _ string) ([]byte, string, error) {
		return nil, "", fmt.Errorf("模拟全部失败")
	}
	first, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("第一次归档失败: %v", err)
	}
	if first.Images != 0 || first.FailedImages != 1 {
		t.Fatalf("第一次结果 = %+v", first)
	}

	archiver.download = testDownloader
	second, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("第二次归档失败: %v", err)
	}
	if second.Duplicate || second.Dir != first.Dir || second.Images != 1 {
		t.Errorf("不完整归档应复用目录补齐: first=%+v second=%+v", first, second)
	}
}

func TestSaveNoContent(t *testing.T) {
	root := t.TempDir()
	rawURL := "https://www.xiaohongshu.com/discovery/item/abc123"
	archiver := NewArchiver(root, nil, nil)
	archiver.Client = &fakeFetcher{notes: map[string]*xhs.Note{
		rawURL: {ID: "abc123"},
	}}
	archiver.download = testDownloader

	if _, err := archiver.Save(context.Background(), rawURL); err == nil {
		t.Fatal("空笔记应返回错误")
	}
}

func TestSaveVideoMetadata(t *testing.T) {
	root := t.TempDir()
	rawURL := "https://www.xiaohongshu.com/discovery/item/video123"
	note := &xhs.Note{
		ID:          "video123",
		URL:         rawURL,
		Title:       "视频笔记",
		Desc:        "看视频",
		PublishedAt: "2026-11-04",
		VideoURL:    "https://video.example/v.mp4",
	}
	archiver := NewArchiver(root, nil, nil)
	archiver.Client = &fakeFetcher{notes: map[string]*xhs.Note{rawURL: note}}
	archiver.download = testDownloader

	result, err := archiver.Save(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	md, err := os.ReadFile(filepath.Join(result.Dir, "视频笔记.md"))
	if err != nil {
		t.Fatalf("读取 Markdown 失败: %v", err)
	}
	if !strings.Contains(string(md), "[视频](https://video.example/v.mp4)") {
		t.Errorf("Markdown 缺少视频链接:\n%s", md)
	}

	data, err := os.ReadFile(filepath.Join(result.Dir, "metadata.json"))
	if err != nil {
		t.Fatalf("读取 metadata 失败: %v", err)
	}
	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("解析 metadata 失败: %v", err)
	}
	if meta.VideoURL != "https://video.example/v.mp4" {
		t.Errorf("video_url = %q", meta.VideoURL)
	}
}
