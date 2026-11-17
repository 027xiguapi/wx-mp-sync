package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// maxHTMLBytes 限制分享页 HTML 大小。
	maxHTMLBytes = 16 << 20
	// maxHTMLMessages 是单次合并消息数的上限,防御异常数据。
	maxHTMLMessages = 2000
)

// fnArgsRe 匹配服务端渲染数据脚本的 data-fn-args 属性(单双引号都兼容)。
var fnArgsRe = regexp.MustCompile(`(?s)data-fn-args=(?:'([^']*)'|"([^"]*)")`)

// fetchShareHTML 抓取分享页 HTML,并从服务端渲染数据里解析完整消息。
//
// 快照接口(/im/message/share/get)为了减小响应,会把消息的 content_block 清空,
// 只保留扁平文本;图片搜索、图文混排的图片只存在于页面 SSR 数据的 block 里,
// 所以优先从 HTML 里取,接口作为兜底。
func (c *Client) fetchShareHTML(ctx context.Context, shareID string) (*shareData, error) {
	base := strings.TrimRight(c.APIBase, "/")
	if base == "" {
		base = defaultAPIBase
	}
	pageURL := base + "/thread/" + shareID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("页面返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLBytes))
	if err != nil {
		return nil, err
	}
	return parseShareHTML(body)
}

// parseShareHTML 从分享页 HTML 里解析 share_info 与 message_snapshot。
func parseShareHTML(page []byte) (*shareData, error) {
	matches := fnArgsRe.FindAllSubmatch(page, -1)
	for _, match := range matches {
		raw := match[1]
		if len(raw) == 0 {
			raw = match[2]
		}
		if len(raw) == 0 {
			continue
		}
		// 属性值是 HTML 转义后的 JSON,先还原再解析。
		decoded := html.UnescapeString(string(raw))
		var args []json.RawMessage
		if err := json.Unmarshal([]byte(decoded), &args); err != nil {
			continue
		}
		for _, arg := range args {
			var holder struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(arg, &holder); err != nil || len(holder.Data) == 0 {
				continue
			}
			var data shareData
			if err := json.Unmarshal(holder.Data, &data); err != nil {
				continue
			}
			if data.ShareInfo.ShareID == "" && len(data.MessageSnapshot.MessageList) == 0 {
				continue
			}
			if len(data.MessageSnapshot.MessageList) > maxHTMLMessages {
				data.MessageSnapshot.MessageList = data.MessageSnapshot.MessageList[:maxHTMLMessages]
			}
			return &data, nil
		}
	}
	return nil, fmt.Errorf("页面里没有找到分享数据")
}

// mergeShareData 以 HTML 数据为主,补上接口里多出来的消息与分享信息。
func mergeShareData(primary, fallback *shareData) *shareData {
	if primary == nil {
		return fallback
	}
	if fallback == nil {
		return primary
	}
	if primary.ShareInfo.ShareID == "" {
		primary.ShareInfo = fallback.ShareInfo
	}
	if len(primary.MessageSnapshot.MessageList) == 0 {
		primary.MessageSnapshot = fallback.MessageSnapshot
		return primary
	}

	seen := make(map[int]bool, len(primary.MessageSnapshot.MessageList))
	apiByIndex := make(map[int]message, len(fallback.MessageSnapshot.MessageList))
	for _, m := range fallback.MessageSnapshot.MessageList {
		apiByIndex[messageIndex(m)] = m
	}
	for i := range primary.MessageSnapshot.MessageList {
		m := &primary.MessageSnapshot.MessageList[i]
		idx := messageIndex(*m)
		seen[idx] = true
		if apiMsg, ok := apiByIndex[idx]; ok && strings.TrimSpace(apiMsg.Content) != "" {
			m.ExtraContent = apiMsg.Content
			if len(m.ContentBlock) == 0 && len(apiMsg.ContentBlock) > 0 {
				m.ContentBlock = apiMsg.ContentBlock
			}
		}
	}
	added := 0
	for _, m := range fallback.MessageSnapshot.MessageList {
		idx := messageIndex(m)
		if seen[idx] {
			continue
		}
		seen[idx] = true
		primary.MessageSnapshot.MessageList = append(primary.MessageSnapshot.MessageList, m)
		added++
		if added >= maxHTMLMessages {
			break
		}
	}
	return primary
}
