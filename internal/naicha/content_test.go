package naicha

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreparePostLimitsAndFooter(t *testing.T) {
	body := strings.Repeat("这是一段正文内容。", 700) // 5600 字,远超 3000
	article := &Article{
		Dir:      t.TempDir(),
		Title:    strings.Repeat("标", 100),
		Author:   "测试公众号",
		Creator:  "作者小茶",
		URL:      "https://mp.weixin.qq.com/s?__biz=MzA1&mid=100&idx=1&sn=abc&chksm=verylong&key=secret&scene=1",
		Markdown: body,
	}
	post, err := PreparePost(article, Config{Category: "coupon", Tags: []string{"公众号"}})
	if err != nil {
		t.Fatal(err)
	}
	if textLen(post.Title) > maxTitleUnits || textLen(post.Title) < minTitleUnits {
		t.Fatalf("title length = %d", textLen(post.Title))
	}
	if !strings.HasSuffix(post.Title, "…") {
		t.Fatalf("long title should be truncated: %q", post.Title)
	}
	if textLen(post.Content) > maxContentUnits {
		t.Fatalf("content length = %d, want <= %d", textLen(post.Content), maxContentUnits)
	}
	if !post.Truncated {
		t.Fatal("expected truncated flag")
	}
	if !strings.Contains(post.Content, "本文为节选") {
		t.Fatalf("missing truncation note: %q", tail(post.Content, 300))
	}
	if !strings.Contains(post.Content, "原文：") || !strings.Contains(post.Content, "mp.weixin.qq.com/s?") {
		t.Fatalf("missing source link: %q", tail(post.Content, 400))
	}
	if post.Category != "discussion" {
		t.Fatalf("category = %q, coupon should fall back to discussion", post.Category)
	}
	if post.Author != "作者小茶" {
		t.Fatalf("author = %q", post.Author)
	}
}

func TestPreparePostStripsImages(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "images", "a.png"))
	md := "开头文字\n\n![本地图](images/a.png)\n\n中间文字\n\n![外链图](https://cdn.example.com/b.jpg)\n\n结尾文字"
	post, err := PreparePost(&Article{Dir: dir, Title: "测试文章", Markdown: md}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(post.Content, "![") {
		t.Fatalf("image markdown should be removed: %q", post.Content)
	}
	if len(post.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(post.Images))
	}
	if len(post.Images[0].Data) == 0 || post.Images[0].Filename == "" {
		t.Fatalf("local image not loaded: %+v", post.Images[0])
	}
	if post.Images[1].URL != "https://cdn.example.com/b.jpg" {
		t.Fatalf("remote image = %+v", post.Images[1])
	}
	if !strings.Contains(post.Content, "开头文字") || !strings.Contains(post.Content, "结尾文字") {
		t.Fatalf("text lost: %q", post.Content)
	}
}

func TestPreparePostCoverWhenNoBodyImages(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "cover.png"))
	post, err := PreparePost(&Article{Dir: dir, Title: "封面测试", Markdown: "只有文字的正文内容。"}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(post.Images) != 1 {
		t.Fatalf("images = %d, want cover", len(post.Images))
	}
	if len(post.Images[0].Data) == 0 || !strings.HasPrefix(post.Images[0].Filename, "cover") {
		t.Fatalf("cover not loaded: %+v", post.Images[0])
	}
}

func TestPreparePostRejectsEmptyMarkdown(t *testing.T) {
	_, err := PreparePost(&Article{Dir: t.TempDir(), Title: "标题", Markdown: "   "}, Config{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeTags(t *testing.T) {
	tags := normalizeTags([]string{"#奶茶", " 奶 茶 ", "AI 中转", "这是一个超过十二个字的标签", "公众号", "返利", "推广", "讨论", "公告"})
	if len(tags) > maxTags {
		t.Fatalf("got %d tags", len(tags))
	}
	if tags[0] != "奶茶" {
		t.Fatalf("first tag = %q", tags[0])
	}
	for _, tag := range tags {
		if strings.Contains(tag, " ") {
			t.Fatalf("tag contains space: %q", tag)
		}
		if textLen(tag) > maxTagUnits {
			t.Fatalf("tag too long: %q (%d)", tag, textLen(tag))
		}
	}
	seen := map[string]bool{}
	for _, tag := range tags {
		if seen[tag] {
			t.Fatalf("duplicate tag %q", tag)
		}
		seen[tag] = true
	}
}

func TestCanonicalSourceURL(t *testing.T) {
	raw := "https://mp.weixin.qq.com/s?__biz=MzA1&mid=100&idx=1&sn=abc&chksm=verylong&key=secret&scene=1"
	got := canonicalSourceURL(raw)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "mp.weixin.qq.com" {
		t.Fatalf("host = %q", u.Host)
	}
	q := u.Query()
	if len(q) != 4 || q.Get("__biz") != "MzA1" || q.Get("sn") != "abc" {
		t.Fatalf("query not shortened: %s", got)
	}
	if strings.Contains(got, "chksm") || strings.Contains(got, "key") {
		t.Fatalf("tracking params kept: %s", got)
	}
}

func TestNormalizeTitleShort(t *testing.T) {
	if got := normalizeTitle("Hi"); textLen(got) < minTitleUnits {
		t.Fatalf("short title not padded: %q", got)
	}
	if got := normalizeTitle("   "); got == "" {
		t.Fatal("empty title not replaced")
	}
}

func TestTextLenCountsEmojiAsTwo(t *testing.T) {
	if got := textLen("茶🍵"); got != 3 {
		t.Fatalf("textLen = %d, want 3", got)
	}
	if got := truncateText("茶🍵茶", 2); got != "茶" {
		t.Fatalf("truncateText = %q", got)
	}
}

func TestResolveLocalPathRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	if _, ok := resolveLocalPath(dir, "../secret.png"); ok {
		t.Fatal("path traversal should be rejected")
	}
	if _, ok := resolveLocalPath(dir, "/etc/passwd"); ok {
		t.Fatal("absolute path should be rejected")
	}
	writePNG(t, filepath.Join(dir, "images", "a.png"))
	if p, ok := resolveLocalPath(dir, "images/a.png"); !ok || !strings.HasSuffix(p, "a.png") {
		t.Fatalf("valid path rejected: %q %v", p, ok)
	}
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
