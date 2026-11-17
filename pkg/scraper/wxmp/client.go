package wxmp

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// OfficialAccount 描述一个用于 getmsg 接口拉取的公众号账号凭证。
type OfficialAccount struct {
	Name        string
	Biz         string
	Uin         string
	Key         string
	PassTicket  string
	AppmsgToken string
	AuthorID    string
	MaxArticles int
	// Cookie 是微信客户端的会话 Cookie(可选)。新版 getmsg 只带 URL 参数
	// 可能返回 "no session",带上客户端 Cookie 成功率更高。
	Cookie string
}

// OfficialMsgListResp 对应 getmsg 接口的返回结构。
type OfficialMsgListResp struct {
	Ret        int    `json:"ret"`
	ErrMsg     string `json:"errmsg"`
	MsgList    string `json:"general_msg_list"`
	HasMore    int    `json:"can_msg_continue"`
	MsgCount   int    `json:"msg_count"`
	NextOffset int    `json:"next_offset"`
}

// CommonMsgInfo 描述推送列表里每条消息的公共信息。
type CommonMsgInfo struct {
	Id       int    `json:"id"`
	Type     int    `json:"type"`
	Datetime int    `json:"datetime"`
	Fakeid   string `json:"fakeid"`
	Status   int    `json:"status"`
	Content  string `json:"content"`
}

// OfficialArticle 描述列表中的一篇文章。
type OfficialArticle struct {
	Title      string `json:"title"`
	Digest     string `json:"digest"`
	ContentUrl string `json:"content_url"`
	SourceUrl  string `json:"source_url"`
	Cover      string `json:"cover"`
	Author     string `json:"author"`
	Fileid     int    `json:"fileid"`
}

// OfficialAccountMsg 描述一条推送,可能包含多图文。
type OfficialAccountMsg struct {
	Title               string            `json:"title"`
	Digest              string            `json:"digest"`
	ContentUrl          string            `json:"content_url"`
	SourceUrl           string            `json:"source_url"`
	Cover               string            `json:"cover"`
	Author              string            `json:"author"`
	IsMulti             int               `json:"is_multi"`
	MultiAppMsgItemList []OfficialArticle `json:"multi_app_msg_item_list"`
}

// OfficialAccountMsgListRespItem 是 general_msg_list.list 中的一项。
type OfficialAccountMsgListRespItem struct {
	MsgExtInfo    OfficialAccountMsg `json:"app_msg_ext_info"`
	CommonMsgInfo CommonMsgInfo      `json:"comm_msg_info"`
}

// AccountClient 通过 getmsg 接口拉取公众号的文章列表。
type AccountClient struct {
	logger   *zerolog.Logger
	Timeout  time.Duration
	MaxPages int
	// BaseURL 是接口根地址,默认 https://mp.weixin.qq.com(测试可替换)。
	BaseURL string
}

// NewAccountClient 创建账号列表客户端。
func NewAccountClient(logger *zerolog.Logger) *AccountClient {
	return &AccountClient{
		logger:   logger,
		Timeout:  15 * time.Second,
		MaxPages: 20,
		BaseURL:  "https://mp.weixin.qq.com",
	}
}

// baseURL 返回接口根地址(去掉末尾斜杠)。
func (c *AccountClient) baseURL() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return "https://mp.weixin.qq.com"
	}
	return base
}

// BuildMsgListURL 构建 getmsg 请求 URL。
func (c *AccountClient) BuildMsgListURL(acct *OfficialAccount, offset int) string {
	u := c.baseURL() + "/mp/profile_ext"
	query := url.Values{}
	query.Set("action", "getmsg")
	query.Set("__biz", acct.Biz)
	query.Set("uin", acct.Uin)
	query.Set("key", acct.Key)
	query.Set("pass_ticket", acct.PassTicket)
	query.Set("wxtoken", "")
	query.Set("x5", "0")
	query.Set("count", "10")
	query.Set("offset", strconv.Itoa(offset))
	query.Set("f", "json")
	return u + "?" + query.Encode()
}

// BuildMsgListReferer 构建 getmsg 请求的 Referer。
func (c *AccountClient) BuildMsgListReferer(acct *OfficialAccount) string {
	params := url.Values{}
	params.Set("action", "home")
	params.Set("__biz", acct.Biz)
	params.Set("scene", "124")
	params.Set("uin", acct.Uin)
	params.Set("key", acct.Key)
	params.Set("devicetype", "UnifiedPCWindows")
	params.Set("version", "f2541022")
	params.Set("lang", "zh_CN")
	params.Set("a8scene", "1")
	params.Set("acctmode", "0")
	params.Set("pass_ticket", acct.PassTicket)
	return c.baseURL() + "/mp/profile_ext?" + params.Encode()
}

// Fetch 发起带微信 PC UA 的请求。
func (c *AccountClient) Fetch(targetURL, referer string) (*http.Response, error) {
	return c.FetchWithCookie(targetURL, referer, "")
}

// FetchWithCookie 发起带微信 PC UA 与会话 Cookie 的请求。
func (c *AccountClient) FetchWithCookie(targetURL, referer, cookie string) (*http.Response, error) {
	client := &http.Client{Timeout: c.Timeout}
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("referer", referer)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36 NetType/WIFI MicroMessenger/7.0.20.1781(0x6700143B) WindowsWechat(0x63090a13) UnifiedPCWindowsWechat(0xf2541022) XWEB/16467 Flue")
	if strings.TrimSpace(cookie) != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// FetchMsgList 抓取指定 offset 的推送列表。
func (c *AccountClient) FetchMsgList(acct *OfficialAccount, offset int) (*OfficialMsgListResp, error) {
	targetURL := c.BuildMsgListURL(acct, offset)
	referer := c.BuildMsgListReferer(acct)
	resp, err := c.FetchWithCookie(targetURL, referer, acct.Cookie)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var data OfficialMsgListResp
	if err := json.Unmarshal(respBytes, &data); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if data.Ret != 0 {
		msg := data.ErrMsg
		if strings.TrimSpace(msg) == "" {
			msg = "未知错误"
		}
		return nil, fmt.Errorf("getmsg 接口返回错误 ret=%d errmsg=%s", data.Ret, msg)
	}
	return &data, nil
}

// absoluteContentURL 把 getmsg 返回的相对/转义链接补全为完整 URL。
func absoluteContentURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	u = html.UnescapeString(u)
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return "https://mp.weixin.qq.com" + u
}

// FeedArticle 是公众号推送列表里的一篇文章(用于"最新文章"选择)。
type FeedArticle struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Cover    string `json:"cover"`
	Digest   string `json:"digest"`
	Author   string `json:"author"`
	Datetime int64  `json:"datetime"` // 推送时间(unix 秒),0 表示未知
}

// FetchLatestArticles 分页拉取账号最新的文章列表(按推送时间从新到旧)。
func (c *AccountClient) FetchLatestArticles(acct *OfficialAccount, limit int) ([]FeedArticle, error) {
	if limit <= 0 {
		limit = 20
	}
	var out []FeedArticle
	seen := map[string]bool{}
	offset := 0
	maxPages := c.MaxPages
	if maxPages <= 0 {
		maxPages = 20
	}

	appendArticle := func(title, contentURL, cover, digest, author string, datetime int64) {
		u := absoluteContentURL(contentURL)
		if u == "" || seen[u] || len(out) >= limit {
			return
		}
		seen[u] = true
		out = append(out, FeedArticle{
			Title:    strings.TrimSpace(html.UnescapeString(title)),
			URL:      u,
			Cover:    strings.TrimSpace(cover),
			Digest:   strings.TrimSpace(html.UnescapeString(digest)),
			Author:   strings.TrimSpace(html.UnescapeString(author)),
			Datetime: datetime,
		})
	}

	for page := 0; page < maxPages && len(out) < limit; page++ {
		data, err := c.FetchMsgList(acct, offset)
		if err != nil {
			return out, err
		}
		var msgList struct {
			List []OfficialAccountMsgListRespItem `json:"list"`
		}
		if err := json.Unmarshal([]byte(data.MsgList), &msgList); err != nil {
			return out, fmt.Errorf("解析推送列表失败: %w", err)
		}
		for _, item := range msgList.List {
			datetime := int64(item.CommonMsgInfo.Datetime)
			info := item.MsgExtInfo
			appendArticle(info.Title, info.ContentUrl, info.Cover, info.Digest, info.Author, datetime)
			for _, sub := range info.MultiAppMsgItemList {
				appendArticle(sub.Title, sub.ContentUrl, sub.Cover, sub.Digest, sub.Author, datetime)
			}
			if len(out) >= limit {
				break
			}
		}
		if data.HasMore == 0 || data.NextOffset == offset {
			break
		}
		offset = data.NextOffset
	}
	return out, nil
}

// FetchAllMsgURLs 分页拉取账号全部文章 URL(受 MaxArticles 限制)。
func (c *AccountClient) FetchAllMsgURLs(acct *OfficialAccount) ([]string, error) {
	var urls []string
	offset := 0
	buildURL := absoluteContentURL

	for page := 0; page < c.MaxPages; page++ {
		data, err := c.FetchMsgList(acct, offset)
		if err != nil {
			return urls, err
		}
		var msgList struct {
			List []OfficialAccountMsgListRespItem `json:"list"`
		}
		if err := json.Unmarshal([]byte(data.MsgList), &msgList); err != nil {
			return urls, fmt.Errorf("解析推送列表失败: %w", err)
		}
		for _, item := range msgList.List {
			if u := buildURL(item.MsgExtInfo.ContentUrl); u != "" {
				urls = append(urls, u)
			}
			for _, sub := range item.MsgExtInfo.MultiAppMsgItemList {
				if u := buildURL(sub.ContentUrl); u != "" {
					urls = append(urls, u)
				}
			}
		}
		if acct.MaxArticles > 0 && len(urls) >= acct.MaxArticles {
			urls = urls[:acct.MaxArticles]
			break
		}
		if data.HasMore == 0 {
			break
		}
		offset = data.NextOffset
	}
	return urls, nil
}
