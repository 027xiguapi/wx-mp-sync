package naicha

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestBotPostPayloadJSON 验证请求体字段与论坛 /api/bot/posts 的 curl 示例一致。
func TestBotPostPayloadJSON(t *testing.T) {
	payload := botPostPayload{
		Bot:        "quanxiaoer",
		BoardSlug:  "your-board",
		Title:      "采集标题",
		Content:    "采集正文",
		ManualTags: []string{"新品"},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["bot"] != "quanxiaoer" || got["boardSlug"] != "your-board" {
		t.Errorf("bot/boardSlug = %+v", got)
	}
	if got["title"] != "采集标题" || got["content"] != "采集正文" {
		t.Errorf("title/content = %+v", got)
	}
	tags, ok := got["manualTags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "新品" {
		t.Errorf("manualTags = %+v", got["manualTags"])
	}
}

// TestPublishBotDefaultBoard 验证没有配置分类时 boardSlug 使用 new-releases。
func TestPublishBotDefaultBoard(t *testing.T) {
	var gotBoard string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			BoardSlug string `json:"boardSlug"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotBoard = body.BoardSlug
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    0,
			"message": "发布成功",
			"data":    map[string]any{"id": "post-1", "status": "NORMAL", "url": "/post/post-1"},
		})
	}))
	defer server.Close()

	client := New(Config{
		APIBase:    server.URL,
		Bot:        "bot-account",
		BotSecret:  "secret",
		TimeoutSec: 10,
		StatePath:  filepath.Join(t.TempDir(), "state.json"),
	}, nil)
	res := client.Publish(context.Background(), &PublishPost{
		Title:     "默认分类测试",
		Content:   "正文",
		DedupeKey: "xhs:default-board",
	}, false)
	if !res.OK {
		t.Fatalf("Publish failed: %s", res.Message)
	}
	if gotBoard != "new-releases" {
		t.Errorf("boardSlug = %q, 期望 new-releases", gotBoard)
	}
}

// TestBotPostPayloadOmitsEmptyTags 验证没有标签时不发送 manualTags。
func TestBotPostPayloadOmitsEmptyTags(t *testing.T) {
	data, err := json.Marshal(botPostPayload{
		Bot:       "bot",
		BoardSlug: "board",
		Title:     "标题",
		Content:   "正文",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "manualTags") {
		t.Errorf("空标签不应出现在请求体: %s", data)
	}
}
