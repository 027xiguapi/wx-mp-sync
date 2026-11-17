// Package xhs 抓取小红书笔记的正文、图片与标签。
//
// 小红书笔记页会把笔记详情放在 window.__INITIAL_STATE__ 里
// (部分移动端页面用 __INITIAL_SSR_STATE__),这里直接解析这段服务端渲染数据,
// 不需要签名接口。笔记链接可能带 xsec_token,也可能是 xhslink.com 短链,
// 两种都支持;需要登录的页面可以在 Client.Cookie 里配置浏览器 Cookie。
package xhs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// defaultUserAgent 模拟 iPhone Safari。小红书的桌面端笔记页经常直接跳登录,
	// 而分享链接在移动端服务端渲染页面里能拿到完整的 __INITIAL_STATE__。
	defaultUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	// desktopUserAgent 是移动端解析失败时的兜底 UA。
	desktopUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// maxHTMLBytes 限制页面大小,避免异常响应占满内存。
	maxHTMLBytes = 24 << 20
	// maxImageBytes 限制单张图片大小。
	maxImageBytes = 12 << 20
	// maxRedirects 限制短链跳转次数。
	maxRedirects = 5
)

// errLoginWall 表示请求被小红书重定向到了登录页。
var errLoginWall = errors.New("小红书要求登录")

var (
	// notePathPattern 从 /explore/<id>、/discovery/item/<id>、/note/<id> 里提取笔记 ID。
	notePathPattern = regexp.MustCompile(`/(?:explore|discovery/item|note)/([A-Za-z0-9]{16,32})`)
	// bareNoteIDPattern 匹配裸笔记 ID(通常 24 位十六进制)。
	bareNoteIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{16,32}$`)
	// jsRedirectPattern 匹配短链页面里的 JS 跳转。
	jsRedirectPattern = regexp.MustCompile(`(?i)(?:location(?:\.href)?\s*=\s*|window\.location\.replace\(\s*)["']([^"']+)["']`)
	// metaRefreshPattern 匹配 <meta http-equiv="refresh" content="0;url=...">。
	metaRefreshPattern = regexp.MustCompile(`(?i)<meta[^>]+http-equiv=["']?refresh["']?[^>]+content=["'][^"']*url=([^"'>\s]+)`)
	// canonicalNoteIDPattern 匹配 24 位十六进制笔记 ID。
	canonicalNoteIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{24}$`)
)

// Image 是一张笔记配图。
type Image struct {
	URL    string
	Width  int
	Height int
}

// Note 是小红书笔记的正文数据。
type Note struct {
	ID          string
	URL         string
	Title       string
	Desc        string
	Author      string
	AuthorID    string
	Type        string
	PublishedAt string
	Images      []Image
	Tags        []string
	VideoURL    string
}

// Client 抓取小红书笔记页面。
type Client struct {
	// HTTP 复用连接;为 nil 时使用带超时的默认客户端。
	HTTP *http.Client
	// UserAgent 默认为桌面 Chrome UA。
	UserAgent string
	// Cookie 是可选的浏览器 Cookie,用于需要登录的笔记。
	Cookie string
}

// NewClient 创建带默认配置的客户端。
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// DefaultUserAgent 返回抓取小红书时使用的移动端 Safari UA。
func DefaultUserAgent() string {
	return defaultUserAgent
}

// userAgents 返回抓取笔记页时依次尝试的 UA。
// 显式配置了 UserAgent 时只使用配置值,避免覆盖用户自己的选择。
func (c *Client) userAgents() []string {
	if c != nil && strings.TrimSpace(c.UserAgent) != "" {
		return []string{strings.TrimSpace(c.UserAgent)}
	}
	return []string{defaultUserAgent, desktopUserAgent}
}

// IsXHSURL 判断链接是否属于小红书域名。
func IsXHSURL(raw string) bool {
	u, err := parseLooseURL(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "xiaohongshu.com" || strings.HasSuffix(host, ".xiaohongshu.com") ||
		host == "xhslink.com" || strings.HasSuffix(host, ".xhslink.com")
}

// ParseNoteID 从完整链接、短链或裸 ID 里提取笔记 ID。
// 短链需要联网解析,请使用 Client.Fetch;这里只处理路径里带 ID 的链接。
func ParseNoteID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("链接为空")
	}
	if !strings.Contains(raw, "/") && !strings.Contains(raw, ":") {
		if bareNoteIDPattern.MatchString(raw) {
			return raw, nil
		}
		return "", fmt.Errorf("不是有效的小红书笔记 ID")
	}
	u, err := parseLooseURL(raw)
	if err != nil {
		return "", fmt.Errorf("不是有效的链接")
	}
	if m := notePathPattern.FindStringSubmatch(u.Path); len(m) == 2 {
		return m[1], nil
	}
	if id := strings.TrimSpace(u.Query().Get("noteId")); bareNoteIDPattern.MatchString(id) {
		return id, nil
	}
	return "", fmt.Errorf("链接里没有找到笔记 ID(应形如 /explore/xxxx)")
}

// CanonicalURL 返回笔记的标准地址。
func CanonicalURL(noteID string) string {
	return "https://www.xiaohongshu.com/explore/" + noteID
}

// Fetch 抓取并解析一篇小红书笔记。raw 可以是完整链接、xhslink 短链或裸 ID。
func (c *Client) Fetch(ctx context.Context, raw string) (*Note, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("链接为空")
	}

	if id, err := ParseNoteID(raw); err == nil && !strings.Contains(raw, "/") {
		raw = CanonicalURL(id)
	}

	pageURL, noteID, err := c.resolveURL(ctx, raw)
	if err != nil {
		return nil, err
	}

	var (
		lastErr   error
		loginWall bool
	)
	for _, ua := range c.userAgents() {
		html, err := c.fetchHTMLAs(ctx, pageURL, ua)
		if err != nil {
			lastErr = err
			if errors.Is(err, errLoginWall) {
				loginWall = true
			}
			continue
		}

		note, err := ParseNoteHTML(html, noteID)
		if err != nil {
			lastErr = err
			if looksLikeLoginWall(html) {
				loginWall = true
			}
			continue
		}
		note.URL = pageURL
		if note.ID == "" {
			note.ID = noteID
		}
		if note.ID == "" {
			return nil, fmt.Errorf("没有解析到笔记 ID")
		}
		return note, nil
	}

	if loginWall {
		return nil, fmt.Errorf("小红书要求登录或触发了风控:可在浏览器打开该笔记后重试,或在 config.yaml 的 naicha_sync.xhs_cookie 里配置 Cookie")
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("没有解析到小红书笔记数据")
}

// DownloadImage 下载一张笔记配图,返回图片数据和 MIME。
func (c *Client) DownloadImage(ctx context.Context, imageURL string) ([]byte, string, error) {
	imageURL = normalizeImageURL(imageURL)
	if imageURL == "" {
		return nil, "", fmt.Errorf("图片地址为空")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, "", err
	}
	c.setHeaders(req, "https://www.xiaohongshu.com/")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("下载图片失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("下载图片失败: HTTP %d", resp.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && !strings.HasPrefix(contentType, "image/") {
		return nil, "", fmt.Errorf("图片地址返回的不是图片: %s", contentType)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取图片失败: %w", err)
	}
	if len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("图片超过 %dMB", maxImageBytes>>20)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("图片内容为空")
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}
	return data, contentType, nil
}

// resolveURL 解析短链并提取笔记 ID。
func (c *Client) resolveURL(ctx context.Context, raw string) (string, string, error) {
	u, err := parseLooseURL(raw)
	if err != nil {
		return "", "", fmt.Errorf("不是有效的链接")
	}
	host := strings.ToLower(u.Hostname())
	if host == "xhslink.com" || strings.HasSuffix(host, ".xhslink.com") {
		resolved, err := c.followShortLink(ctx, u.String())
		if err != nil {
			return "", "", err
		}
		u, err = parseLooseURL(resolved)
		if err != nil {
			return "", "", fmt.Errorf("短链跳转地址无效: %s", resolved)
		}
	}

	host = strings.ToLower(u.Hostname())
	if host != "xiaohongshu.com" && !strings.HasSuffix(host, ".xiaohongshu.com") {
		return "", "", fmt.Errorf("不是小红书链接: %s", u.Hostname())
	}

	noteID, err := ParseNoteID(u.String())
	if err != nil {
		return "", "", err
	}
	return u.String(), noteID, nil
}

// followShortLink 跟随 xhslink.com 的 HTTP/JS/meta 跳转,返回小红书链接。
func (c *Client) followShortLink(ctx context.Context, raw string) (string, error) {
	current := raw
	for i := 0; i < maxRedirects; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return "", err
		}
		c.setHeaders(req, "")
		client := *c.httpClient()
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("打开小红书短链失败: %w", err)
		}

		location := strings.TrimSpace(resp.Header.Get("Location"))
		if location != "" && resp.StatusCode >= 300 && resp.StatusCode < 400 {
			resp.Body.Close()
			next, err := resolveReference(current, location)
			if err != nil {
				return "", err
			}
			if isXHSHost(next) {
				return next, nil
			}
			current = next
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		resp.Body.Close()

		if next := findRedirectInHTML(string(body), current); next != "" {
			return next, nil
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return "", fmt.Errorf("小红书短链没有跳转到笔记页,请改用完整笔记链接")
		}
		return "", fmt.Errorf("打开小红书短链失败: HTTP %d", resp.StatusCode)
	}
	return "", fmt.Errorf("小红书短链跳转次数过多")
}

// fetchHTMLAs 用指定 UA 请求笔记页,便于在移动端/桌面端页面之间回退。
func (c *Client) fetchHTMLAs(ctx context.Context, pageURL, userAgent string) (string, error) {
	clone := *c
	clone.UserAgent = userAgent
	return clone.fetchHTML(ctx, pageURL)
}

// fetchHTML 请求笔记页 HTML。
func (c *Client) fetchHTML(ctx context.Context, pageURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	c.setHeaders(req, "https://www.xiaohongshu.com/")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("打开小红书笔记失败: %w", err)
	}
	defer resp.Body.Close()

	// 跟随重定向后落在 /login 说明被判定为需要登录。
	if resp.Request != nil && resp.Request.URL != nil && strings.HasPrefix(strings.ToLower(resp.Request.URL.Path), "/login") {
		return "", errLoginWall
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("打开小红书笔记失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取小红书页面失败: %w", err)
	}
	if len(body) > maxHTMLBytes {
		return "", fmt.Errorf("小红书页面超过 %dMB", maxHTMLBytes>>20)
	}
	return string(body), nil
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) setHeaders(req *http.Request, referer string) {
	ua := ""
	cookie := ""
	if c != nil {
		ua = strings.TrimSpace(c.UserAgent)
		cookie = strings.TrimSpace(c.Cookie)
	}
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.6")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

func parseLooseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("链接为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("不是有效的链接")
	}
	return u, nil
}

func resolveReference(base, ref string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return baseURL.ResolveReference(refURL).String(), nil
}

func isXHSHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != "xiaohongshu.com" && !strings.HasSuffix(host, ".xiaohongshu.com") {
		return false
	}
	_, err = ParseNoteID(raw)
	return err == nil
}

func findRedirectInHTML(body, base string) string {
	if m := jsRedirectPattern.FindStringSubmatch(body); len(m) == 2 {
		if next, err := resolveReference(base, strings.TrimSpace(m[1])); err == nil {
			return next
		}
	}
	if m := metaRefreshPattern.FindStringSubmatch(body); len(m) == 2 {
		if next, err := resolveReference(base, strings.TrimSpace(m[1])); err == nil {
			return next
		}
	}
	return ""
}

func normalizeImageURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	if strings.HasPrefix(raw, "http://") {
		return "https://" + strings.TrimPrefix(raw, "http://")
	}
	return raw
}

func looksLikeLoginWall(html string) bool {
	lower := strings.ToLower(html)
	for _, marker := range []string{"登录后查看", "当前笔记暂时无法浏览", "请登录", "安全验证", "note not found"} {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// NormalizeNoteID 校验并返回小写笔记 ID。
func NormalizeNoteID(raw string) string {
	raw = strings.TrimSpace(raw)
	if canonicalNoteIDPattern.MatchString(raw) {
		return strings.ToLower(raw)
	}
	return raw
}
