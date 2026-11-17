// Package chatgpt 抓取 ChatGPT 分享页(chatgpt.com/s/...):正文、图片与元数据。
//
// 分享页是 React Router 服务端渲染的,数据在
// window.__reactRouterContext.streamController.enqueue("...") 的 turbo-stream
// 里。这里直接解码流数据,不需要登录,也不启动浏览器。
package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://chatgpt.com"
	defaultUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	maxPageBytes   = 32 << 20
)

var (
	// shareIDPattern 是分享 id 的允许字符集(t_... 或旧版 UUID)。
	shareIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,120}$`)
	// enqueueRe 提取 streamController.enqueue("...") 里的 JS 字符串。
	enqueueRe = regexp.MustCompile(`streamController\.enqueue\("((?:\\.|[^"\\])*)"\)`)
	// asyncImageGroupRe 匹配正文里的图片组件标签。
	asyncImageGroupRe = regexp.MustCompile(`<AsyncImageGroup\b[^>]*/>`)
)

// Client 抓取 ChatGPT 分享页。
type Client struct {
	HTTP      *http.Client
	BaseURL   string
	UserAgent string
	// Proxy 是访问 ChatGPT 时使用的上级代理(如 http://127.0.0.1:7897)。
	Proxy string
}

// NewClient 创建带默认配置的客户端。
func NewClient() *Client {
	return &Client{}
}

// HTTPClient 返回用于访问 ChatGPT 的 HTTP 客户端(带上级代理)。
func (c *Client) HTTPClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy := strings.TrimSpace(c.Proxy); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
	c.HTTP = &http.Client{Transport: transport, Timeout: 40 * time.Second}
	return c.HTTP
}

// ParseShareID 从 ChatGPT 分享链接(或裸 id)中提取分享 id。
func ParseShareID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("链接为空")
	}
	if !strings.Contains(raw, "/") && !strings.Contains(raw, ":") {
		if shareIDPattern.MatchString(raw) {
			return raw, nil
		}
		return "", fmt.Errorf("不是有效的 ChatGPT 分享 id")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		u, err = url.Parse("https://" + strings.TrimLeft(raw, "/"))
	}
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("不是有效的链接")
	}
	host := strings.ToLower(u.Hostname())
	if host != "chatgpt.com" && host != "chat.openai.com" {
		return "", fmt.Errorf("不是 ChatGPT 链接: %s", u.Hostname())
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 2 && (parts[0] == "s" || parts[0] == "share") {
		id := parts[1]
		if shareIDPattern.MatchString(id) {
			return id, nil
		}
	}
	return "", fmt.Errorf("链接里没有找到分享 id(应形如 /s/xxxx 或 /share/xxxx)")
}

// CanonicalURL 返回分享链接的标准形式。
func CanonicalURL(id string) string {
	return defaultBaseURL + sharePath(id)
}

// sharePath 根据 id 形式返回页面路径。
func sharePath(id string) string {
	if strings.HasPrefix(id, "t_") {
		return "/s/" + id
	}
	return "/share/" + id
}

// Fetch 抓取并解析一个 ChatGPT 分享。
func (c *Client) Fetch(ctx context.Context, raw string) (*Article, error) {
	id, err := ParseShareID(raw)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}

	// 新版分享是 /s/<id>,旧分享是 /share/<uuid>;按 id 形式先试对应路径,再试另一个。
	paths := []string{sharePath(id)}
	if strings.HasPrefix(id, "t_") {
		paths = append(paths, "/share/"+id)
	} else {
		paths = append(paths, "/s/"+id)
	}

	var errs []string
	for _, p := range paths {
		var lastErr error
		// 分享页偶尔会返回登录/挑战页,重试一次通常能拿到 SSR 数据。
		for attempt := 0; attempt < 2; attempt++ {
			post, err := c.fetchPost(ctx, base+p)
			if err != nil {
				lastErr = err
				continue
			}
			article, err := buildArticle(post, id)
			if err != nil {
				lastErr = err
				continue
			}
			return article, nil
		}
		if lastErr != nil {
			errs = append(errs, lastErr.Error())
		}
	}
	if len(errs) == 0 {
		errs = append(errs, "页面没有返回分享数据")
	}
	return nil, fmt.Errorf("抓取 ChatGPT 分享失败: %s", strings.Join(errs, "; "))
}

// fetchPost 下载页面并解析出 post 对象。
func (c *Client) fetchPost(ctx context.Context, pageURL string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = defaultUA
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")

	client := c.HTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("页面返回 HTTP %d", resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil, err
	}
	return parseSharePage(page)
}

// parseSharePage 从分享页 HTML 里解出 post。
func parseSharePage(page []byte) (map[string]any, error) {
	m := enqueueRe.FindSubmatch(page)
	if len(m) < 2 {
		return nil, fmt.Errorf("页面里没有找到对话数据")
	}
	var escaped string
	if err := json.Unmarshal([]byte(`"`+string(m[1])+`"`), &escaped); err != nil {
		return nil, fmt.Errorf("解析页面流数据失败: %w", err)
	}
	root, err := decodeTurboStream(escaped)
	if err != nil {
		return nil, err
	}
	pp := findPostWithProfile(root, 0)
	if pp == nil {
		return nil, fmt.Errorf("页面数据里没有 postWithProfile")
	}
	post, ok := pp["post"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("页面数据里没有 post")
	}
	return post, nil
}

// findPostWithProfile 在解码后的数据里查找 postWithProfile。
func findPostWithProfile(v any, depth int) map[string]any {
	if depth > 12 {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		if pp, ok := x["postWithProfile"].(map[string]any); ok {
			return pp
		}
		for _, child := range x {
			if found := findPostWithProfile(child, depth+1); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range x {
			if found := findPostWithProfile(child, depth+1); found != nil {
				return found
			}
		}
	}
	return nil
}
