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
	logger  *zerolog.Logger
	Timeout time.Duration
	MaxPages int
}

// NewAccountClient 创建账号列表客户端。
func NewAccountClient(logger *zerolog.Logger) *AccountClient {
	return &AccountClient{
		logger:   logger,
		Timeout:  15 * time.Second,
		MaxPages: 20,
	}
}

// BuildMsgListURL 构建 getmsg 请求 URL。
func (c *AccountClient) BuildMsgListURL(acct *OfficialAccount, offset int) string {
	u := "https://mp.weixin.qq.com/mp/profile_ext"
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
	return "https://mp.weixin.qq.com/mp/profile_ext?" + params.Encode()
}

// Fetch 发起带微信 PC UA 的请求。
func (c *AccountClient) Fetch(targetURL, referer string) (*http.Response, error) {
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
	resp, err := c.Fetch(targetURL, referer)
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

// FetchAllMsgURLs 分页拉取账号全部文章 URL(受 MaxArticles 限制)。
func (c *AccountClient) FetchAllMsgURLs(acct *OfficialAccount) ([]string, error) {
	var urls []string
	offset := 0
	buildURL := func(u string) string {
		if u == "" {
			return ""
		}
		u = html.UnescapeString(u)
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			return u
		}
		return "https://mp.weixin.qq.com" + u
	}

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
