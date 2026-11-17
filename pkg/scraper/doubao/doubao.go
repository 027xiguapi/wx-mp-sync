// Package doubao 抓取豆包(doubao.com)分享的对话:正文、图片与参考来源。
//
// 豆包分享页(/thread/<id>)在服务端渲染时只带部分消息,图片也可能被懒加载省略。
// 页面自己的前端会调用内部快照接口 /im/message/share/get 拿到完整消息列表,
// 这里直接复用同一个接口:无需登录 Cookie,也不需要启动浏览器。
package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultAPIBase 是豆包网页版地址,内部快照接口都挂在它下面。
	defaultAPIBase = "https://www.doubao.com"
	// defaultUserAgent 模拟桌面 Chrome,接口对 UA 没有严格校验,但保持一致更稳。
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// maxResponseBytes 限制接口响应体大小,避免异常响应占满内存。
	maxResponseBytes = 32 << 20
)

var (
	// bareShareIDPattern 限制裸 id 的前缀,避免把随便一段文字当成分享 id。
	bareShareIDPattern = regexp.MustCompile(`^[xwcd][A-Za-z0-9_-]{5,80}$`)
	// pathShareIDPattern 从 /thread/<id> 路径里提取 id。
	pathShareIDPattern = regexp.MustCompile(`/(?:thread|share)/([A-Za-z0-9_-]{4,80})`)
	// generatedImagePattern 匹配 AI 生图 URL 里的图片 id(32 位十六进制)。
	generatedImagePattern = regexp.MustCompile(`rc_gen_image/([a-f0-9]{32})`)
	// generatedTplvPattern 匹配豆包的图片处理模板,用于换成无水印原图。
	generatedTplvPattern = regexp.MustCompile(`~tplv-[A-Za-z0-9_]+\.(?:png|jpe?g|webp)`)
	// urlPattern 从任意文本中提取 http(s) 链接。
	urlPattern = regexp.MustCompile(`https?://[^\s"'\\<>]+`)
)

// Client 调用豆包分享快照接口。
type Client struct {
	// HTTP 复用连接;为 nil 时使用带超时的默认客户端。
	HTTP *http.Client
	// APIBase 默认为 https://www.doubao.com,测试里可替换。
	APIBase string
	// UserAgent 默认为桌面 Chrome UA。
	UserAgent string
}

// NewClient 创建带默认配置的客户端。
func NewClient() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// DefaultUserAgent 返回抓取豆包时使用的桌面 Chrome UA。
func DefaultUserAgent() string {
	return defaultUserAgent
}

// ParseShareID 从豆包分享链接(或裸 id)中提取 share id。
func ParseShareID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("链接为空")
	}
	// 允许直接粘贴 id(豆包分享 id 以 x/w/c/d 开头)。
	if !strings.Contains(raw, "/") && !strings.Contains(raw, ":") {
		if bareShareIDPattern.MatchString(raw) {
			return raw, nil
		}
		return "", fmt.Errorf("不是有效的豆包分享 id")
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// 允许省略协议,例如 www.doubao.com/thread/xxx。
		u, err = url.Parse("https://" + strings.TrimLeft(raw, "/"))
	}
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("不是有效的链接")
	}
	host := strings.ToLower(u.Hostname())
	if host != "doubao.com" && !strings.HasSuffix(host, ".doubao.com") {
		return "", fmt.Errorf("不是豆包链接: %s", u.Hostname())
	}
	if m := pathShareIDPattern.FindStringSubmatch(u.Path); len(m) == 2 {
		return m[1], nil
	}
	return "", fmt.Errorf("链接里没有找到分享 id(应形如 /thread/xxxx)")
}

// CanonicalURL 返回分享链接的标准形式。
func CanonicalURL(shareID string) string {
	return defaultAPIBase + "/thread/" + shareID
}

// IsDoubaoURL 判断是否为豆包域名下的链接。
func IsDoubaoURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "doubao.com" || strings.HasSuffix(host, ".doubao.com")
}

// Fetch 抓取并解析一个豆包分享链接。
// 优先解析分享页 HTML 里的完整 SSR 数据(带图片 block),失败或缺少正文时回退到快照接口。
func (c *Client) Fetch(ctx context.Context, raw string) (*Article, error) {
	shareID, err := ParseShareID(raw)
	if err != nil {
		return nil, err
	}

	var errs []string
	var pageData *shareData
	if data, err := c.fetchShareHTML(ctx, shareID); err == nil {
		pageData = data
	} else {
		errs = append(errs, "页面: "+err.Error())
	}

	var apiData *shareData
	if data, err := c.fetchAPI(ctx, shareID); err == nil {
		apiData = data
	} else {
		errs = append(errs, "接口: "+err.Error())
	}

	data := mergeShareData(pageData, apiData)
	if data == nil {
		if len(errs) == 0 {
			errs = append(errs, "页面和接口都没有返回内容")
		}
		return nil, fmt.Errorf("抓取豆包分享失败: %s", strings.Join(errs, "; "))
	}

	article, buildErr := buildArticle(data)
	if buildErr != nil {
		// HTML 数据异常时再退回纯接口数据。
		if apiData != nil && apiData != data {
			if fallback, err := buildArticle(apiData); err == nil {
				article, buildErr = fallback, nil
			}
		}
		if buildErr != nil {
			return nil, buildErr
		}
	}
	if article.ShareID == "" {
		article.ShareID = shareID
	}
	if article.SourceURL == "" {
		article.SourceURL = CanonicalURL(shareID)
	}
	if len(article.Messages) == 0 && strings.TrimSpace(article.Title) == "" {
		return nil, fmt.Errorf("分享里没有可下载的内容")
	}
	return article, nil
}

// fetchAPI 调用豆包快照接口,返回原始 data。
func (c *Client) fetchAPI(ctx context.Context, shareID string) (*shareData, error) {
	var errs []string
	for _, ep := range c.endpoints(shareID) {
		body, err := c.do(ctx, ep)
		if err != nil {
			errs = append(errs, ep.path+": "+err.Error())
			continue
		}
		data, err := parseShareResponseData(body)
		if err != nil {
			errs = append(errs, ep.path+": "+err.Error())
			continue
		}
		if data.ShareInfo.ShareID == "" {
			data.ShareInfo.ShareID = shareID
		}
		if len(data.MessageSnapshot.MessageList) == 0 && strings.TrimSpace(data.ShareInfo.ShareName) == "" {
			errs = append(errs, ep.path+": 分享里没有可下载的内容")
			continue
		}
		return data, nil
	}
	if len(errs) == 0 {
		errs = append(errs, "接口没有返回内容")
	}
	return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
}

// endpoint 描述一次快照接口调用。
type endpoint struct {
	method string
	path   string
	query  url.Values
	body   any
}

// endpoints 返回按优先级排列的接口候选。
// x 开头的分享用 IM 接口;w 开头用网页话题快照;其余走移动端通用接口。
func (c *Client) endpoints(shareID string) []endpoint {
	aidQuery := url.Values{}
	aidQuery.Set("aid", "497858")
	aidQuery.Set("device_platform", "web")

	im := endpoint{
		method: http.MethodPost,
		path:   "/im/message/share/get",
		body: map[string]any{
			"share_id":                   shareID,
			"need_bot_info":              true,
			"need_lazy_load_big_message": true,
			"need_lazy_load_thread_msg":  true,
		},
	}
	snapshot := endpoint{
		method: http.MethodPost,
		path:   "/samantha/thread/share/snapshot/get",
		query:  aidQuery,
		body:   map[string]any{"share_id": shareID, "need_bot": false},
	}
	aliceQuery := url.Values{}
	aliceQuery.Set("aid", "497858")
	aliceQuery.Set("device_platform", "web")
	aliceQuery.Set("samantha_web", "0")
	alice := endpoint{
		method: http.MethodPost,
		path:   "/alice/message/share/get",
		query:  aliceQuery,
		body:   map[string]any{"share_id": shareID},
	}

	switch shareID[0] {
	case 'x':
		return []endpoint{im, alice}
	case 'w':
		return []endpoint{snapshot, im, alice}
	case 'c', 'd':
		// 收藏夹/书签分享的数据结构不同,这里不做支持,给出明确提示。
		return nil
	default:
		return []endpoint{alice, im}
	}
}

// do 执行一次接口请求并返回响应体。
func (c *Client) do(ctx context.Context, ep endpoint) ([]byte, error) {
	base := strings.TrimRight(c.APIBase, "/")
	if base == "" {
		base = defaultAPIBase
	}
	u := base + ep.path
	if len(ep.query) > 0 {
		u += "?" + ep.query.Encode()
	}

	var bodyReader io.Reader
	if ep.body != nil {
		data, err := json.Marshal(ep.body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, ep.method, u, bodyReader)
	if err != nil {
		return nil, err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Origin", base)
	if id := shareIDFromBody(ep); id != "" {
		req.Header.Set("Referer", CanonicalURL(id))
	}
	if ep.body != nil {
		req.Header.Set("Content-Type", "application/json; encoding=utf-8")
	}

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("接口返回 HTTP %d", resp.StatusCode)
	}
	return data, nil
}

// shareIDFromBody 取出请求体里的 share_id,用于拼接 Referer。
func shareIDFromBody(ep endpoint) string {
	if m, ok := ep.body.(map[string]any); ok {
		if id, ok := m["share_id"].(string); ok {
			return id
		}
	}
	return ""
}

// flexString 兼容接口里字符串/数字两种时间与序号写法。
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*f = ""
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(string(data))
	return nil
}

// apiEnvelope 是接口的最外层响应。
type apiEnvelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// shareData 是 data 字段里用到的部分。
type shareData struct {
	ShareInfo       shareInfo `json:"share_info"`
	MessageSnapshot struct {
		MessageList       messageList `json:"message_list"`
		ThreadMessageList messageList `json:"thread_message_list"`
	} `json:"message_snapshot"`
}

// messageList 兼容接口把空列表返回成 {} 的情况。
type messageList []message

// UnmarshalJSON 忽略对象形式的空值,只接受数组。
func (l *messageList) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) || data[0] == '{' {
		return nil
	}
	var list []message
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*l = list
	return nil
}

type shareInfo struct {
	ShareID    string     `json:"share_id"`
	ShareName  string     `json:"share_name"`
	ShareTime  flexString `json:"share_time"`
	ShareState int        `json:"share_status"`
	User       struct {
		Nickname string `json:"nick_name"`
	} `json:"user"`
	Bot struct {
		Name string `json:"name"`
	} `json:"bot"`
}

type message struct {
	MessageID    string          `json:"message_id"`
	IndexInConv  flexString      `json:"index_in_conv"`
	UserType     int             `json:"user_type"`
	ContentType  int             `json:"content_type"`
	Content      string          `json:"content"`
	ContentBlock json.RawMessage `json:"content_block"`
	CreateTime   flexString      `json:"create_time"`
	// ExtraContent 是合并时补充的接口版 content(页面 SSR 数据里可能缺参考来源)。
	ExtraContent string `json:"-"`
}

// ParseShareResponse 解析快照接口返回的 JSON。
func ParseShareResponse(body []byte) (*Article, error) {
	data, err := parseShareResponseData(body)
	if err != nil {
		return nil, err
	}
	return buildArticle(data)
}

// parseShareResponseData 解析快照接口响应里的 data 字段。
func parseShareResponseData(body []byte) (*shareData, error) {
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("解析接口响应失败: %w", err)
	}
	if env.Code != 0 {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = strings.TrimSpace(env.Msg)
		}
		if msg == "" {
			msg = fmt.Sprintf("错误码 %d", env.Code)
		}
		return nil, fmt.Errorf("豆包接口返回错误: %s", msg)
	}
	if len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		return nil, fmt.Errorf("豆包接口没有返回数据")
	}

	var data shareData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("解析分享数据失败: %w", err)
	}
	return &data, nil
}

// parseUnixSeconds 解析秒级时间戳。
func parseUnixSeconds(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// parseUnixMillis 解析毫秒级时间戳。
func parseUnixMillis(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}
