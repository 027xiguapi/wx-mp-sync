package naicha

// 机器人发帖接口(论坛侧):
//
//	POST {APIBase}/api/bot/posts
//	Content-Type: application/json
//	X-Bot-Secret: <BOT_API_SECRET>
//	{"bot":"...","boardSlug":"...","title":"...","content":"...","manualTags":["..."]}
//
// 机器人账号需在论坛后台创建(isBot=true),密钥需与论坛服务 .env 里的
// BOT_API_SECRET 完全一致。该接口不接受文件上传,配图只能以 Markdown
// 图片链接的形式写进正文。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// botPostPayload 是 POST /api/bot/posts 的请求体,字段与论坛接口保持一致。
type botPostPayload struct {
	Bot        string   `json:"bot"`
	BoardSlug  string   `json:"boardSlug"`
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	ManualTags []string `json:"manualTags,omitempty"`
}

// BotReady 表示是否配置了机器人密钥和机器人账号。
func (c *Client) BotReady() bool {
	if c == nil {
		return false
	}
	return c.cfg.BotSecret != "" && c.cfg.Bot != ""
}

// publishBot 通过 POST /api/bot/posts 发布帖子(请求头 X-Bot-Secret)。
// 机器人接口不支持上传图片,Images 里的原图地址会以 Markdown 形式写进正文。
func (c *Client) publishBot(ctx context.Context, post *PublishPost, key string) PublishResult {
	title := normalizeTitle(post.Title)
	// 与用户发帖一致:论坛标题有最小长度限制。
	if textLen(title) < 5 {
		title += " 笔记"
	}
	content := appendImageLinks(post.Content, post.Images)
	board := strings.TrimSpace(post.Board)
	if board == "" {
		board = c.cfg.Board
	}
	if board == "" {
		board = "new-releases"
	}
	tags := post.Tags
	if len(tags) == 0 {
		tags = c.cfg.Tags
	}

	payload := botPostPayload{
		Bot:        c.cfg.Bot,
		BoardSlug:  board,
		Title:      title,
		Content:    content,
		ManualTags: tags,
	}

	var created createPostResponse
	status, body, err := c.postJSONBot(ctx, "/api/bot/posts", payload, &created)
	if err != nil {
		c.log.Error().Err(err).Str("title", title).Msg("机器人发帖失败")
		return PublishResult{Message: err.Error()}
	}
	if status < 200 || status >= 300 {
		return PublishResult{Message: botAPIErrorMessage(status, body)}
	}

	postID := strings.TrimSpace(created.Data.ID)
	if postID == "" {
		return PublishResult{Message: "论坛返回的帖子 ID 为空"}
	}
	postURL := strings.TrimSpace(created.Data.URL)
	if strings.HasPrefix(postURL, "/") {
		postURL = c.cfg.APIBase + postURL
	}
	if postURL == "" {
		postURL = c.postURL(postID)
	}
	pending := !strings.EqualFold(created.Data.Status, "NORMAL")

	rec := Record{
		PostID:   postID,
		URL:      postURL,
		Title:    title,
		SyncedAt: time.Now().Format(time.RFC3339),
	}
	if key != "" {
		if err := c.state.Put(key, rec); err != nil {
			c.log.Warn().Err(err).Str("key", key).Msg("写入奶茶圈同步状态失败")
		}
	}

	c.log.Info().
		Str("title", title).
		Str("id", postID).
		Str("url", postURL).
		Int("images", len(post.Images)).
		Bool("pending", pending).
		Msg("机器人发帖成功")

	return PublishResult{
		OK:      true,
		ID:      postID,
		URL:     postURL,
		Title:   title,
		Images:  len(post.Images),
		Pending: pending,
		Message: created.Message,
	}
}

// postJSONBot 发送带 X-Bot-Secret 请求头的 JSON 请求。
func (c *Client) postJSONBot(ctx context.Context, path string, payload any, out any) (int, []byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIBase+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wx-mp-sync/naicha")
	req.Header.Set("X-Bot-Secret", c.cfg.BotSecret)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil && len(body) > 0 {
		_ = json.Unmarshal(body, out)
	}
	return resp.StatusCode, body, nil
}

// botAPIErrorMessage 把机器人接口的错误转换成可操作的提示。
func botAPIErrorMessage(status int, body []byte) string {
	message := apiMessage(body)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	switch status {
	case http.StatusUnauthorized:
		return "BOT_API_SECRET 不正确:请确认与论坛服务环境变量里的值一致(" + message + ")"
	case http.StatusServiceUnavailable:
		return "论坛未启用机器人接口:请在论坛服务的 .env 里配置 BOT_API_SECRET 后重启(" + message + ")"
	}
	return fmt.Sprintf("论坛发布失败(%d): %s", status, message)
}
