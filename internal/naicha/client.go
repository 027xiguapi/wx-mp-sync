package naicha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Config 是同步文章到奶茶圈论坛的配置。
type Config struct {
	APIBase    string   // 论坛地址,如 http://localhost:3002
	Token      string   // POST_API_TOKEN;留空时读 NAICHA_POST_TOKEN / POST_API_TOKEN
	Category   string   // 分类,默认 discussion
	Tags       []string // 标签,最多 6 个
	Author     string   // 文章没有作者时的兜底昵称
	Auto       bool     // 保存文章后自动同步
	TimeoutSec int
	StatePath  string // 去重状态文件,一般放在输出目录下

	// 下面是「复用管理员登录」模式使用的新版奶茶圈接口配置:
	// POST /api/auth/login → POST /api/upload → POST /api/posts/create。
	Username string // 管理员用户名或邮箱
	Password string // 管理员密码
	Cookie   string // 可选:bbs_session 的值,或完整的 Cookie 请求头;配置后跳过登录
	Board    string // 发帖分类 boardSlug,默认 new-releases

	// 机器人模式:配置后优先走 POST /api/bot/posts(请求头 X-Bot-Secret),
	// 不需要管理员登录;该接口不支持上传图片,图片以原图链接写入正文。
	BotSecret string // 论坛服务端环境变量 BOT_API_SECRET 的值
	Bot       string // 机器人账号:用户名、昵称或 ID
}

// Client 负责把归档文章发布到奶茶圈。
type Client struct {
	cfg   Config
	log   *zerolog.Logger
	http  *http.Client
	state *stateStore

	sessionMu sync.Mutex
	session   string // Cookie 请求头的值,如 bbs_session=...
}

// Result 是一次同步的结果,直接返回给管理页。
type Result struct {
	OK        bool   `json:"ok"`
	Duplicate bool   `json:"duplicate,omitempty"`
	ID        int    `json:"id,omitempty"`
	PostID    string `json:"post_id,omitempty"`
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
	Images    int    `json:"images,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Message   string `json:"error,omitempty"`
}

// New 创建论坛同步客户端。调用方只在启用时创建。
func New(cfg Config, logger *zerolog.Logger) *Client {
	cfg.APIBase = strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/")
	if cfg.APIBase == "" {
		cfg.APIBase = "http://localhost:3002"
	}
	if strings.TrimSpace(cfg.Category) == "" {
		cfg.Category = "discussion"
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 60
	}
	if strings.TrimSpace(cfg.Token) == "" {
		cfg.Token = firstNonEmpty(os.Getenv("NAICHA_POST_TOKEN"), os.Getenv("POST_API_TOKEN"))
	}
	cfg.Username = strings.TrimSpace(cfg.Username)
	// 密码保持原样(首尾空格也可能是密码的一部分)。
	cfg.Board = strings.TrimSpace(cfg.Board)
	if cfg.Board == "" {
		cfg.Board = "new-releases"
	}
	cfg.Cookie = normalizeSessionCookie(cfg.Cookie)
	// 机器人密钥去掉首尾空白(从配置文件粘贴时容易带上换行/空格)。
	cfg.BotSecret = strings.TrimSpace(cfg.BotSecret)
	cfg.Bot = strings.TrimSpace(cfg.Bot)
	if logger == nil {
		l := zerolog.Nop()
		logger = &l
	}
	return &Client{
		cfg:   cfg,
		log:   logger,
		http:  &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second},
		state: newStateStore(cfg.StatePath),
	}
}

// APIBase 返回论坛地址,供管理页展示。
func (c *Client) APIBase() string {
	if c == nil {
		return ""
	}
	return c.cfg.APIBase
}

// Auto 表示保存文章后是否自动同步。
func (c *Client) Auto() bool {
	return c != nil && c.cfg.Auto
}

// Lookup 查询某个归档目录是否已经同步过。
func (c *Client) Lookup(dir string) (Record, bool) {
	if c == nil {
		return Record{}, false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Record{}, false
	}
	return c.state.Get(abs)
}

// LookupKey 按自定义去重键查询发布记录(例如 xhs:<noteId>)。
func (c *Client) LookupKey(key string) (Record, bool) {
	if c == nil {
		return Record{}, false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return Record{}, false
	}
	return c.state.Get(key)
}

// SyncDir 把 dir 里的文章发布到论坛。force 为 true 时忽略去重状态重新发布。
func (c *Client) SyncDir(ctx context.Context, dir string, force bool) Result {
	if c == nil {
		return Result{Message: "没有启用奶茶圈同步"}
	}
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return Result{Message: "文章目录无效: " + err.Error()}
	}
	if !force {
		if rec, ok := c.state.Get(abs); ok {
			return Result{
				OK:        true,
				Duplicate: true,
				ID:        rec.ID,
				PostID:    rec.PostID,
				URL:       rec.URL,
				Title:     rec.Title,
				Message:   "这篇文章已经同步过了",
			}
		}
	}

	article, err := LoadArticle(abs)
	if err != nil {
		return Result{Message: err.Error()}
	}
	post, err := PreparePost(article, c.cfg)
	if err != nil {
		return Result{Message: "整理文章失败: " + err.Error()}
	}

	c.log.Info().
		Str("title", post.Title).
		Int("images", len(post.Images)).
		Bool("truncated", post.Truncated).
		Msg("同步文章到奶茶圈")

	// 配置了管理员登录时,走新版论坛接口(上传图片 + /api/posts/create)。
	if c.SessionReady() {
		published := c.Publish(ctx, &PublishPost{
			Title:     post.Title,
			Content:   post.Content,
			Board:     c.cfg.Board,
			Tags:      post.Tags,
			Images:    post.Images,
			Source:    post.Source,
			DedupeKey: abs,
		}, force)
		return Result{
			OK:        published.OK,
			Duplicate: published.Duplicate,
			PostID:    published.ID,
			URL:       published.URL,
			Title:     published.Title,
			Images:    published.Images,
			Truncated: post.Truncated,
			Message:   published.Message,
		}
	}

	created, err := c.post(ctx, post)
	if err != nil {
		c.log.Error().Err(err).Str("title", post.Title).Msg("奶茶圈同步失败")
		return Result{Message: err.Error()}
	}

	rec := Record{
		ID:       created.ID,
		URL:      created.URL,
		Title:    post.Title,
		SyncedAt: time.Now().Format(time.RFC3339),
	}
	if err := c.state.Put(abs, rec); err != nil {
		c.log.Warn().Err(err).Str("dir", abs).Msg("写入奶茶圈同步状态失败")
	}
	c.log.Info().Str("title", post.Title).Int("id", created.ID).Str("url", created.URL).Msg("奶茶圈同步成功")

	return Result{
		OK:        true,
		ID:        created.ID,
		URL:       created.URL,
		Title:     post.Title,
		Images:    len(post.Images),
		Truncated: post.Truncated,
	}
}

// Created 是论坛返回的帖子信息。
type Created struct {
	ID    int
	URL   string
	Title string
}

// post 用 multipart 提交帖子;正文图片作为文件上传,外链图片作为 URL 字段提交。
func (c *Client) post(ctx context.Context, post *Post) (Created, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	writeField := func(key, value string) error {
		if value == "" {
			return nil
		}
		return mw.WriteField(key, value)
	}
	if err := writeField("title", post.Title); err != nil {
		return Created{}, err
	}
	if err := writeField("content", post.Content); err != nil {
		return Created{}, err
	}
	if err := writeField("category", post.Category); err != nil {
		return Created{}, err
	}
	if err := writeField("author", post.Author); err != nil {
		return Created{}, err
	}
	if len(post.Tags) > 0 {
		if err := writeField("tags", strings.Join(post.Tags, ",")); err != nil {
			return Created{}, err
		}
	}

	for _, img := range post.Images {
		if img.URL != "" {
			if err := mw.WriteField("images", img.URL); err != nil {
				return Created{}, err
			}
			continue
		}
		fw, err := mw.CreateFormFile("images", img.Filename)
		if err != nil {
			return Created{}, err
		}
		if _, err := fw.Write(img.Data); err != nil {
			return Created{}, err
		}
	}
	if err := mw.Close(); err != nil {
		return Created{}, err
	}

	endpoint := c.cfg.APIBase + "/api/posts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return Created{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", "wx-mp-sync/naicha")
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Created{}, fmt.Errorf("请求奶茶圈失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Created{}, forumError(resp.StatusCode, resp.Status, body)
	}

	created := parseCreated(resp, body)
	if created.URL == "" {
		// 帖子可能已经创建成功,只是响应体不完整;不要报错让用户重复发布。
		c.log.Warn().Int("status", resp.StatusCode).Str("body", truncateText(string(body), 200)).Msg("奶茶圈响应缺少帖子地址")
	}
	return created, nil
}

// parseCreated 解析 201 响应;兼容只有 Location 头的情况。
func parseCreated(resp *http.Response, body []byte) Created {
	var parsed struct {
		Post struct {
			ID    int    `json:"id"`
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"post"`
	}
	_ = json.Unmarshal(body, &parsed)

	created := Created{ID: parsed.Post.ID, URL: parsed.Post.URL, Title: parsed.Post.Title}
	if created.URL == "" {
		created.URL = absoluteLocation(resp)
	}
	if created.ID == 0 {
		if id := topicIDFromURL(created.URL); id > 0 {
			created.ID = id
		}
	}
	return created
}

// absoluteLocation 把响应里的 Location 转成绝对地址。
func absoluteLocation(resp *http.Response) string {
	loc := strings.TrimSpace(resp.Header.Get("Location"))
	if loc == "" {
		return ""
	}
	reqURL := resp.Request.URL
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	return reqURL.ResolveReference(u).String()
}

// topicIDFromURL 从 /t/42 里取帖子 ID。
func topicIDFromURL(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "t" {
			if id, err := strconv.Atoi(parts[i+1]); err == nil {
				return id
			}
		}
	}
	return 0
}

// forumError 把论坛的错误 JSON 转成中文提示。
func forumError(statusCode int, status string, body []byte) error {
	var apiErr struct {
		Error  string            `json:"error"`
		Errors map[string]string `json:"errors"`
	}
	_ = json.Unmarshal(body, &apiErr)

	detail := strings.TrimSpace(apiErr.Error)
	if len(apiErr.Errors) > 0 {
		keys := make([]string, 0, len(apiErr.Errors))
		for key := range apiErr.Errors {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fields := make([]string, 0, len(keys))
		for _, key := range keys {
			fields = append(fields, key+"："+apiErr.Errors[key])
		}
		if detail != "" {
			detail += "（" + strings.Join(fields, "；") + "）"
		} else {
			detail = strings.Join(fields, "；")
		}
	}

	switch statusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("奶茶圈鉴权失败(401)：请在 naicha_sync.token 或环境变量 NAICHA_POST_TOKEN 配置 POST_API_TOKEN")
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("提交内容太大(413)：最多 6 张图片、每张 5MB")
	case http.StatusUnsupportedMediaType:
		return fmt.Errorf("奶茶圈不接受该请求格式(415)")
	case http.StatusTooManyRequests:
		return fmt.Errorf("奶茶圈限流(429)：同一 IP 每小时最多 10 篇，请稍后再试")
	}
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	if detail == "" {
		detail = status
	}
	return fmt.Errorf("奶茶圈发布失败(%s)：%s", status, detail)
}
