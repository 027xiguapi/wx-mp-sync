package doubao

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// mustJSON 把任意值转成 JSON 字符串(测试里构造嵌套 JSON 用)。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(data)
}

// fixtureResponse 构造一份包含文本、搜索图片、AI 生图和参考来源的接口响应。
func fixtureResponse(t *testing.T) []byte {
	t.Helper()

	tagValue := mustJSON(t, map[string]any{
		"videos": nil,
		"images": []any{
			map[string]any{
				"uri":           "labis/aaa",
				"thumb_url":     "https://p3-flow-imagex-sign.byteimg.com/thumb-a.jpg",
				"full_size_url": []string{"https://p3-flow-imagex-sign.byteimg.com/full-a.jpg"},
				"img_caption":   "航站楼外景",
			},
			map[string]any{
				"uri":       "labis/bbb",
				"thumb_url": "https://p3-flow-imagex-sign.byteimg.com/thumb-b.jpg",
			},
		},
	})
	textTag := map[string]any{
		"type":     102,
		"tag_info": mustJSON(t, map[string]any{"pos": 5, "value": tagValue}),
	}

	botContent := map[string]any{
		"text":      "这是**回答**正文。",
		"text_tags": []any{textTag},
		"search_references": []any{
			map[string]any{"text_card": map[string]any{"title": "参考一", "url": "https://example.com/a"}},
			map[string]any{"text_card": map[string]any{"title": "参考一(重复)", "url": "https://example.com/a"}},
		},
	}
	genContent := map[string]any{
		"text": "生成图片如下",
		"image": map[string]any{
			"url": "https://p3-flow-imagex-sign.byteimg.com/labis/rc_gen_image/0123456789abcdef0123456789abcdef~tplv-watermark.png?x-expires=1&x-signature=abc",
		},
	}

	payload := map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]any{
			"share_info": map[string]any{
				"share_id":     "xvABCD1234",
				"share_name":   "测试分享标题",
				"share_time":   "1793775893000",
				"share_status": 2,
				"user":         map[string]any{"nick_name": "测试用户"},
				"bot":          map[string]any{"name": "豆包"},
			},
			"message_snapshot": map[string]any{
				"message_list": []any{
					map[string]any{
						"message_id":    "1",
						"index_in_conv": "1",
						"user_type":     1,
						"content_type":  1,
						"content":       mustJSON(t, map[string]any{"text": "你好,帮我整理机场图片"}),
						"create_time":   "1793695892",
					},
					map[string]any{
						"message_id":    "2",
						"index_in_conv": "2",
						"user_type":     2,
						"content_type":  1,
						"content":       mustJSON(t, botContent),
						"create_time":   "1793695893",
					},
					map[string]any{
						"message_id":    "3",
						"index_in_conv": "3",
						"user_type":     2,
						"content_type":  1,
						"content":       mustJSON(t, genContent),
					},
				},
			},
		},
	}
	return []byte(mustJSON(t, payload))
}

// TestParseShareID 验证各种写法的分享链接都能取出 id。
func TestParseShareID(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://www.doubao.com/thread/xvIjVHl6raE0DnXVX", "xvIjVHl6raE0DnXVX", false},
		{"https://www.doubao.com/thread/xvIjVHl6raE0DnXVX?from=copy", "xvIjVHl6raE0DnXVX", false},
		{"www.doubao.com/thread/xvIjVHl6raE0DnXVX", "xvIjVHl6raE0DnXVX", false},
		{"  xvIjVHl6raE0DnXVX  ", "xvIjVHl6raE0DnXVX", false},
		{"https://www.doubao.com/chat/123", "", true},
		{"https://example.com/thread/abc", "", true},
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

// TestParseShareResponse 验证标题、角色、正文、图片与参考来源。
func TestParseShareResponse(t *testing.T) {
	article, err := ParseShareResponse(fixtureResponse(t))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if article.Title != "测试分享标题" || article.Author != "测试用户" {
		t.Errorf("标题/作者 = %q / %q", article.Title, article.Author)
	}
	if article.ShareID != "xvABCD1234" || article.SourceURL != "https://www.doubao.com/thread/xvABCD1234" {
		t.Errorf("分享 id/链接 = %q / %q", article.ShareID, article.SourceURL)
	}
	if len(article.Messages) != 3 {
		t.Fatalf("消息数 = %d, 期望 3", len(article.Messages))
	}
	if article.Messages[0].Role != "我" || article.Messages[1].Role != "豆包" {
		t.Errorf("角色 = %q / %q", article.Messages[0].Role, article.Messages[1].Role)
	}
	if len(article.Images) != 3 {
		t.Fatalf("图片数 = %d, 期望 3 (%+v)", len(article.Images), article.Images)
	}
	if article.Images[0].URL != "https://p3-flow-imagex-sign.byteimg.com/full-a.jpg" {
		t.Errorf("图片 0 应优先使用原图: %q", article.Images[0].URL)
	}
	if !strings.Contains(article.Images[2].URL, "~tplv-a9rns2rl98-image_raw_b.png") {
		t.Errorf("生图 URL 未替换成原图模板: %q", article.Images[2].URL)
	}
	if len(article.References) != 1 || article.References[0].Title != "参考一" {
		t.Errorf("参考来源未去重: %+v", article.References)
	}

	md := article.Markdown
	for _, want := range []string{
		"# 测试分享标题", "## 我", "## 豆包", "你好,帮我整理机场图片",
		"这是**回答**正文。", "![航站楼外景](https://p3-flow-imagex-sign.byteimg.com/full-a.jpg)",
		"## 参考来源", "[参考一](https://example.com/a)",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown 缺少 %q\n---\n%s", want, md)
		}
	}
}

// TestParseShareResponseAPIError 验证接口错误码会转成可读错误。
func TestParseShareResponseAPIError(t *testing.T) {
	_, err := ParseShareResponse([]byte(`{"code":710020102,"message":"分享已失效"}`))
	if err == nil || !strings.Contains(err.Error(), "分享已失效") {
		t.Fatalf("期望返回分享已失效,得到 %v", err)
	}
}

// TestParseAttachedImage 验证上传图片会被收集,搜索引用缩略图不会。
func TestParseAttachedImage(t *testing.T) {
	payload := map[string]any{
		"code": 0,
		"data": map[string]any{
			"share_info": map[string]any{"share_id": "xvATTACH", "share_name": "附件图片"},
			"message_snapshot": map[string]any{
				"message_list": []any{
					map[string]any{
						"index_in_conv": "1",
						"user_type":     2,
						"content": mustJSON(t, map[string]any{
							"text": "看这张图",
							"image": map[string]any{
								"url": "https://p3-flow-imagex-sign.byteimg.com/labis/upload.jpg?x=1&y=2",
							},
							"search_references": []any{
								map[string]any{"text_card": map[string]any{
									"title":     "参考",
									"url":       "https://example.com/a",
									"image_url": "https://p3-flow-imagex-sign.byteimg.com/thumb.jpg",
								}},
							},
						}),
					},
				},
			},
		},
	}
	article, err := ParseShareResponse([]byte(mustJSON(t, payload)))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(article.Images) != 1 {
		t.Fatalf("图片数 = %d, 期望 1 (%+v)", len(article.Images), article.Images)
	}
	if !strings.Contains(article.Images[0].URL, "upload.jpg") {
		t.Errorf("收集到了错误的图片: %q", article.Images[0].URL)
	}
	if strings.Contains(article.Markdown, "thumb.jpg") {
		t.Errorf("搜索引用缩略图不应出现在正文: %s", article.Markdown)
	}
}

// TestFetchWithServer 验证请求会带上分享 id 与完整快照参数。
func TestFetchWithServer(t *testing.T) {
	var gotPath, gotBody string
	response := fixtureResponse(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	}))
	defer srv.Close()

	client := &Client{APIBase: srv.URL, HTTP: srv.Client()}
	article, err := client.Fetch(context.Background(), "https://www.doubao.com/thread/xvABCD1234")
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if gotPath != "/im/message/share/get" {
		t.Errorf("请求路径 = %q", gotPath)
	}
	for _, want := range []string{"xvABCD1234", "need_lazy_load_big_message", "need_bot_info"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("请求体缺少 %q: %s", want, gotBody)
		}
	}
	if article.Title != "测试分享标题" {
		t.Errorf("标题 = %q", article.Title)
	}
}

// makeSharePage 构造一份带 SSR 数据脚本的分享页 HTML。
func makeSharePage(t *testing.T, data map[string]any) []byte {
	t.Helper()
	args := []any{
		"thread_(token)/page",
		"shareInfo",
		map[string]any{"isMobileShareId": true, "data": data},
	}
	encoded := mustJSON(t, args)
	return []byte(`<html><head><script data-script-src="modern-run-router-data-fn" data-fn-args='` +
		encoded + `'></script></head><body></body></html>`)
}

// htmlShareData 构造 share_info + 单条消息的页面数据。
func htmlShareData(t *testing.T, content string, blocks []any) map[string]any {
	t.Helper()
	return map[string]any{
		"share_info": map[string]any{
			"share_id": "xvHTML1234", "share_name": "页面分享", "share_time": "1793775893000",
			"user": map[string]any{"nick_name": "页面用户"},
		},
		"message_snapshot": map[string]any{
			"message_list": []any{
				map[string]any{
					"message_id": "1", "index_in_conv": "2", "user_type": 2, "content_type": 1,
					"content":       content,
					"content_block": blocks,
				},
			},
		},
	}
}

// TestParseHTMLRichMediaImages 验证页面 SSR 里的图文混排图集会被解析,
// 且与搜索卡片同时存在时只取图集,不重复。
func TestParseHTMLRichMediaImages(t *testing.T) {
	richMedia := map[string]any{"rich_media_layout_block": map[string]any{"media": []any{
		map[string]any{"type": 1, "image": map[string]any{
			"item_id":       "labis/g1",
			"uri":           "labis/g1",
			"thumb_url":     "https://p3-flow-imagex-sign.byteimg.com/labis/g1~tplv.jpeg",
			"full_size_url": []string{"https://aka.doubaocdn.com/l/G1"},
		}},
	}}}
	searchCard := map[string]any{"search_query_result_block": map[string]any{"results": []any{
		map[string]any{"image_card": map[string]any{
			"uri":           "https://aka.doubaocdn.com/l/C1",
			"thumb_url":     "https://p3-flow-imagex-sign.byteimg.com/labis/c1~tplv.jpeg",
			"full_size_url": []string{"https://aka.doubaocdn.com/l/C1"},
		}},
	}}}
	blocks := []any{
		map[string]any{"block_type": 10000, "content": map[string]any{"text_block": map[string]any{"text": "回答正文"}}},
		map[string]any{"block_type": 10084, "content": richMedia},
		map[string]any{"block_type": 10025, "content": searchCard},
	}
	page := makeSharePage(t, htmlShareData(t, mustJSON(t, blocks), blocks))

	data, err := parseShareHTML(page)
	if err != nil {
		t.Fatalf("解析页面失败: %v", err)
	}
	article, err := buildArticle(data)
	if err != nil {
		t.Fatalf("构建文章失败: %v", err)
	}
	if len(article.Images) != 1 {
		t.Fatalf("图片数 = %d, 期望 1 (%+v)", len(article.Images), article.Images)
	}
	if article.Images[0].URL != "https://aka.doubaocdn.com/l/G1" {
		t.Errorf("应优先使用图文混排图集,得到 %q", article.Images[0].URL)
	}
	if !strings.Contains(article.Markdown, "回答正文") || !strings.Contains(article.Markdown, "](https://aka.doubaocdn.com/l/G1)") {
		t.Errorf("Markdown 内容不正确:\n%s", article.Markdown)
	}
}

// TestParseHTMLSearchCards 验证没有图集时退到搜索卡片图片。
func TestParseHTMLSearchCards(t *testing.T) {
	searchCard := map[string]any{"search_query_result_block": map[string]any{"results": []any{
		map[string]any{"image_card": map[string]any{
			"uri":           "https://aka.doubaocdn.com/l/C2",
			"thumb_url":     "https://p3-flow-imagex-sign.byteimg.com/labis/c2~tplv.jpeg",
			"full_size_url": []string{"https://aka.doubaocdn.com/l/C2"},
		}},
	}}}
	blocks := []any{
		map[string]any{"block_type": 10000, "content": map[string]any{"text_block": map[string]any{"text": "搜索回答"}}},
		map[string]any{"block_type": 10025, "content": searchCard},
	}
	page := makeSharePage(t, htmlShareData(t, mustJSON(t, blocks), blocks))
	data, err := parseShareHTML(page)
	if err != nil {
		t.Fatalf("解析页面失败: %v", err)
	}
	article, err := buildArticle(data)
	if err != nil {
		t.Fatalf("构建文章失败: %v", err)
	}
	if len(article.Images) != 1 || article.Images[0].URL != "https://aka.doubaocdn.com/l/C2" {
		t.Errorf("搜索卡片图片解析失败: %+v", article.Images)
	}
}

// TestParseHTMLMetaImages 验证老分享把配图挂在 text_block.meta_info 里的情况。
func TestParseHTMLMetaImages(t *testing.T) {
	tagInfo := mustJSON(t, map[string]any{"pos": 6, "media": []any{
		map[string]any{"type": 1, "image": map[string]any{
			"item_id":       "labis/m1",
			"uri":           "labis/m1",
			"thumb_url":     "https://p3-flow-imagex-sign.byteimg.com/labis/m1~tplv.jpeg",
			"full_size_url": []string{"https://aka.doubaocdn.com/l/M1"},
		}},
	}})
	blocks := []any{
		map[string]any{
			"block_type": 10000,
			"content":    map[string]any{"text_block": map[string]any{"text": "老分享正文"}},
			"meta_info":  []any{map[string]any{"type": 202, "tag_info": tagInfo}},
		},
	}
	page := makeSharePage(t, htmlShareData(t, mustJSON(t, blocks), blocks))
	data, err := parseShareHTML(page)
	if err != nil {
		t.Fatalf("解析页面失败: %v", err)
	}
	article, err := buildArticle(data)
	if err != nil {
		t.Fatalf("构建文章失败: %v", err)
	}
	if len(article.Images) != 1 || article.Images[0].URL != "https://aka.doubaocdn.com/l/M1" {
		t.Errorf("meta_info 配图解析失败: %+v", article.Images)
	}
	if !strings.Contains(article.Markdown, "老分享正文") {
		t.Errorf("正文缺失:\n%s", article.Markdown)
	}
}

func TestLiveDoubao(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("DOUBAO_LIVE_URL"))
	if raw == "" {
		t.Skip("未设置 DOUBAO_LIVE_URL,跳过真实豆包链接测试")
	}
	client := NewClient()
	article, err := client.Fetch(context.Background(), raw)
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	t.Logf("标题=%q 作者=%q 消息=%d 图片=%d 参考=%d",
		article.Title, article.Author, len(article.Messages), len(article.Images), len(article.References))
	if len(article.Messages) == 0 {
		t.Fatal("没有解析到消息")
	}
	if strings.TrimSpace(article.Markdown) == "" {
		t.Fatal("Markdown 为空")
	}
}
