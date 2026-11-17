package chatgpt

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestDecodeTurboStream 验证值表引用/对象键解码。
func TestDecodeTurboStream(t *testing.T) {
	stream := `[{"_1":2,"_3":4},"title","Hello","items",[5,6],"a","b"]`
	got, err := decodeTurboStream(stream)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("根节点类型 = %T", got)
	}
	if obj["title"] != "Hello" {
		t.Errorf("title = %v", obj["title"])
	}
	items, ok := obj["items"].([]any)
	if !ok || len(items) != 2 || items[0] != "a" || items[1] != "b" {
		t.Errorf("items = %#v", obj["items"])
	}
}

// TestDecodeTurboSpecial 验证 null/undefined 常量。
func TestDecodeTurboSpecial(t *testing.T) {
	got, err := decodeTurboStream(`[{"_1":2,"_3":4},"a",-5,"b",-7]`)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	obj := got.(map[string]any)
	if v, ok := obj["a"]; !ok || v != nil {
		t.Errorf("a 应为 null: %#v", obj)
	}
	if v, ok := obj["b"]; !ok || v != nil {
		t.Errorf("b 应为 undefined: %#v", obj)
	}
}

// TestParseShareID 验证分享链接解析。
func TestParseShareID(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://chatgpt.com/s/t_6aeae61031908191bd5750a2f9d3ffd4", "t_6aeae61031908191bd5750a2f9d3ffd4", false},
		{"https://chatgpt.com/share/68b0c1e2-1234-4abc-9def-0123456789ab", "68b0c1e2-1234-4abc-9def-0123456789ab", false},
		{"chatgpt.com/s/t_abcdefgh", "t_abcdefgh", false},
		{"t_6aeae61031908191bd5750a2f9d3ffd4", "t_6aeae61031908191bd5750a2f9d3ffd4", false},
		{"https://example.com/s/t_abcdefgh", "", true},
		{"https://chatgpt.com/c/abc", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := ParseShareID(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseShareID(%q) 期望报错,得到 %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ParseShareID(%q) = %q, %v; 期望 %q", c.in, got, err, c.want)
		}
	}
}

// TestBuildArticle 验证正文、图片组件替换与标题回退。
func TestBuildArticle(t *testing.T) {
	tag := `<AsyncImageGroup query={["A","B"]} numPerQuery={1}/>`
	text := "机场照片汇总\n\n" + tag + "\n\n结尾说明。"
	start := len([]rune("机场照片汇总\n\n"))
	end := start + len([]rune(tag))
	post := map[string]any{
		"id":        "t_test1234",
		"posted_at": float64(1793779216),
		"og_title":  "看看这段聊天",
		"attachments": []any{
			map[string]any{
				"kind": "message_slice",
				"messages": []any{
					map[string]any{
						"author": map[string]any{"role": "assistant"},
						"content": map[string]any{
							"parts": []any{text},
						},
						"metadata": map[string]any{
							"genui_components": []any{
								map[string]any{
									"start_index": float64(start),
									"end_index":   float64(end),
									"safe_urls": []any{
										map[string]any{"url": "https://source.example.com/page"},
										map[string]any{"url": "https://images.openai.com/static-rsc-4/AAA?purpose=fullsize"},
										map[string]any{"url": "https://images.openai.com/static-rsc-4/BBB?purpose=inline"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	article, err := buildArticle(post, "t_test1234")
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}
	if article.Title != "机场照片汇总" {
		t.Errorf("标题 = %q", article.Title)
	}
	if article.Messages != 1 || len(article.Images) != 1 {
		t.Fatalf("消息/图片 = %d/%d, 期望 1/1", article.Messages, len(article.Images))
	}
	if article.Images[0].URL != "https://images.openai.com/static-rsc-4/AAA?purpose=fullsize" {
		t.Errorf("应只保留 fullsize 图片: %q", article.Images[0].URL)
	}
	if strings.Contains(article.Markdown, "AsyncImageGroup") {
		t.Errorf("Markdown 里仍有 AsyncImageGroup 标签:\n%s", article.Markdown)
	}
	if !strings.Contains(article.Markdown, "](https://images.openai.com/static-rsc-4/AAA?purpose=fullsize)") {
		t.Errorf("Markdown 缺少图片:\n%s", article.Markdown)
	}
	if !strings.Contains(article.Markdown, "## ChatGPT") {
		t.Errorf("Markdown 缺少角色标题:\n%s", article.Markdown)
	}
}

// TestLiveChatGPT 用真实分享链接验证(设置 CHATGPT_LIVE_URL 后才运行)。
func TestLiveChatGPT(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("CHATGPT_LIVE_URL"))
	if raw == "" {
		t.Skip("未设置 CHATGPT_LIVE_URL,跳过真实 ChatGPT 链接测试")
	}
	client := NewClient()
	client.Proxy = strings.TrimSpace(os.Getenv("CHATGPT_PROXY"))
	article, err := client.Fetch(context.Background(), raw)
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	t.Logf("标题=%q 消息=%d 图片=%d Markdown=%d 字符", article.Title, article.Messages, len(article.Images), len(article.Markdown))
	if article.Messages == 0 || strings.TrimSpace(article.Markdown) == "" {
		t.Fatal("没有解析到正文")
	}
}
