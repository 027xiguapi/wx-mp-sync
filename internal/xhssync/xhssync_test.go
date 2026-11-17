package xhssync

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wx-mp-sync/pkg/scraper/xhs"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func TestBuildPost(t *testing.T) {
	note := &xhs.Note{
		ID:          "abc123",
		URL:         "https://www.xiaohongshu.com/explore/abc123",
		Title:       "好喝",
		Desc:        "今天喝了一杯桂花乌龙。",
		Author:      "奶茶小分队",
		PublishedAt: "2025-11-04",
		Tags:        []string{"奶茶", "#探店", "奶茶"},
	}
	post := BuildPost(note, Options{Board: "discussion", Tags: []string{"小红书", "测评"}})
	if post == nil {
		t.Fatal("nil post")
	}
	if post.Title != "好喝 笔记" {
		t.Fatalf("title = %q", post.Title)
	}
	if !strings.Contains(post.Content, "桂花乌龙") {
		t.Fatalf("content = %q", post.Content)
	}
	for _, banned := range []string{"来源：", "原文：", "---", note.URL} {
		if strings.Contains(post.Content, banned) {
			t.Fatalf("content should not contain %q: %q", banned, post.Content)
		}
	}
	if post.Board != "discussion" || post.DedupeKey != "xhs:abc123" {
		t.Fatalf("post meta = %+v", post)
	}
	if len(post.Tags) != 4 || post.Tags[0] != "小红书" {
		t.Fatalf("tags = %+v", post.Tags)
	}
	for _, want := range []string{"奶茶", "探店", "测评"} {
		if !containsTag(post.Tags, want) {
			t.Errorf("tags missing %q: %+v", want, post.Tags)
		}
	}
}

func TestStripXHSHashtags(t *testing.T) {
	input := "雀斑，是甜攒满的记号☀️\n" +
		"我们把带着雀斑的「维纳斯苹果」做进咖啡\n" +
		"2款#茶百道咖啡[话题]# 新品11月19日上新\n\n" +
		"#茶百道[话题]# #茶百道小雀斑美式[话题]# #茶百道小雀斑拿铁[话题]#\n\n" +
		"![图1](/uploads/posts/a.webp)"
	got := stripXHSHashtags(input)
	for _, banned := range []string{"[话题]#", "#茶百道", "#茶百道咖啡"} {
		if strings.Contains(got, banned) {
			t.Errorf("未删除 %q: %q", banned, got)
		}
	}
	if !strings.Contains(got, "2款新品11月19日上新") {
		t.Errorf("正文空格未清理: %q", got)
	}
	if !strings.Contains(got, "维纳斯苹果") || !strings.Contains(got, "![图1]") {
		t.Errorf("正文或图片被误删: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("空行未合并: %q", got)
	}
}

func TestBuildPostStripsHashtags(t *testing.T) {
	note := &xhs.Note{
		ID:     "abc123",
		URL:    "https://www.xiaohongshu.com/explore/abc123",
		Title:  "咖啡新品！2款维纳斯黄金苹果特调！",
		Desc:   "2款#茶百道咖啡[话题]# 新品11月19日上新\n\n#茶百道[话题]# #茶百道小雀斑美式[话题]#",
		Author: "茶百道ChaPanda",
		Tags:   []string{"茶百道", "茶百道小雀斑美式"},
	}
	post := BuildPost(note, Options{})
	if strings.Contains(post.Content, "[话题]#") || strings.Contains(post.Content, "#茶百道") {
		t.Fatalf("content still has hashtags: %q", post.Content)
	}
	if !strings.Contains(post.Content, "2款新品11月19日上新") {
		t.Fatalf("content lost: %q", post.Content)
	}
	if !containsTag(post.Tags, "茶百道") || !containsTag(post.Tags, "茶百道小雀斑美式") {
		t.Fatalf("tags lost: %+v", post.Tags)
	}
}

func TestBuildPostFallbackTitle(t *testing.T) {
	note := &xhs.Note{Desc: "第一行是标题\n第二行是正文", Images: []xhs.Image{{URL: "https://example.com/a.jpg"}}}
	post := BuildPost(note, Options{})
	if post.Title != "第一行是标题" {
		t.Fatalf("title = %q", post.Title)
	}
	if !strings.Contains(post.Content, "第二行是正文") {
		t.Fatalf("content = %q", post.Content)
	}
}

func containsTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func TestDownloadImages(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString(tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://www.xiaohongshu.com/" {
			t.Errorf("referer = %q", r.Header.Get("Referer"))
		}
		if r.URL.Path == "/bad.jpg" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	}))
	defer server.Close()

	client := &xhs.Client{HTTP: server.Client()}
	note := &xhs.Note{Images: []xhs.Image{
		{URL: server.URL + "/a.jpg"},
		{URL: server.URL + "/bad.jpg"},
		{URL: server.URL + "/b.jpg"},
	}}
	images, failed, err := DownloadImages(context.Background(), client, note, 18)
	if err != nil {
		t.Fatalf("DownloadImages: %v", err)
	}
	if len(images) != 2 || failed != 1 {
		t.Fatalf("images=%d failed=%d", len(images), failed)
	}
	if images[0].Filename != "xhs-01.png" || len(images[0].Data) == 0 {
		t.Fatalf("first image = %+v", images[0])
	}
}

func TestDownloadImagesAllFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := &xhs.Client{HTTP: server.Client()}
	_, failed, err := DownloadImages(context.Background(), client, &xhs.Note{
		Images: []xhs.Image{{URL: server.URL + "/a.jpg"}},
	}, 18)
	if err == nil || failed != 1 {
		t.Fatalf("expected all-failed error, got err=%v failed=%d", err, failed)
	}
}
