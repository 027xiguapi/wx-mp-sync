// Package capture 保存插件从微信请求里自动捕获的公众号凭证与会话信息。
//
// 背景:微信「搜一搜」页面是本地页面(weixin://resourceid/Search/app.html),
// 通过原生 bridge 取数据,HTTP 代理无法注入按钮;但公众号文章页、历史消息页
// 的请求 URL 里带着 __biz / uin / key / pass_ticket,可以用来调用 getmsg
// 拉取该号的最新文章列表。
package capture

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxAccounts 是保留的公众号数量上限。
const maxAccounts = 50

// Account 是一个被捕获的公众号及其会话凭证。
type Account struct {
	Biz        string    `json:"biz"`
	Nickname   string    `json:"nickname"`
	Uin        string    `json:"uin"`
	Key        string    `json:"key"`
	PassTicket string    `json:"pass_ticket"`
	Cookie     string    `json:"-"`
	LastSeen   time.Time `json:"last_seen"`
	SourceURL  string    `json:"-"`
	Usable     bool      `json:"usable"`
}

// Store 是线程安全的账号凭证存储,按 __biz 去重。
type Store struct {
	mu    sync.Mutex
	items map[string]*Account
	ttl   time.Duration
}

// NewStore 创建存储。ttl 内没有再次出现的凭证视为过期。
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Store{items: map[string]*Account{}, ttl: ttl}
}

// CaptureRequest 从一次请求(URL + Cookie 头)里捕获凭证。
// 只有带 __biz 的公众号相关 URL 才会被记录;返回被更新的账号。
func (s *Store) CaptureRequest(rawURL, cookie string) *Account {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "mp.weixin.qq.com" && host != "weixin.qq.com" {
		return nil
	}
	q := parsed.Query()
	biz := strings.TrimSpace(q.Get("__biz"))
	if biz == "" {
		return nil
	}
	uin := strings.TrimSpace(q.Get("uin"))
	key := strings.TrimSpace(q.Get("key"))
	passTicket := strings.TrimSpace(q.Get("pass_ticket"))
	if key == "" && uin == "" && passTicket == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()

	acct, ok := s.items[biz]
	if !ok {
		acct = &Account{Biz: biz}
		s.items[biz] = acct
	}
	if uin != "" {
		acct.Uin = uin
	}
	if key != "" {
		acct.Key = key
	}
	if passTicket != "" {
		acct.PassTicket = passTicket
	}
	if cookie != "" && cookie != acct.Cookie {
		acct.Cookie = cookie
	}
	acct.LastSeen = time.Now()
	acct.SourceURL = truncate(rawURL, 300)
	acct.Usable = acct.Key != "" && acct.Uin != ""
	snapshot := copyAccount(acct)
	return &snapshot
}

// BizFromURL 从公众号链接里取 __biz 参数(取不到返回空串)。
func BizFromURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Query().Get("__biz"))
}

// SetNickname 记录公众号昵称(通常来自文章页的 cgiDataNew 诊断上报)。
func (s *Store) SetNickname(biz, nickname string) {
	biz = strings.TrimSpace(biz)
	nickname = strings.TrimSpace(nickname)
	if biz == "" || nickname == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, ok := s.items[biz]
	if !ok {
		acct = &Account{Biz: biz, LastSeen: time.Now()}
		s.items[biz] = acct
	}
	acct.Nickname = nickname
}

// Get 返回指定公众号的凭证副本。
func (s *Store) Get(biz string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, ok := s.items[strings.TrimSpace(biz)]
	if !ok {
		return Account{}, false
	}
	return copyAccount(acct), true
}

// List 返回全部账号,按最近出现时间倒序。
func (s *Store) List() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	out := make([]Account, 0, len(s.items))
	for _, acct := range s.items {
		out = append(out, copyAccount(acct))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// pruneLocked 清理过期账号与超出上限的旧账号(调用方需持锁)。
func (s *Store) pruneLocked() {
	now := time.Now()
	for biz, acct := range s.items {
		if now.Sub(acct.LastSeen) > s.ttl {
			delete(s.items, biz)
		}
	}
	if len(s.items) <= maxAccounts {
		return
	}
	oldest := ""
	var oldestTime time.Time
	for biz, acct := range s.items {
		if oldest == "" || acct.LastSeen.Before(oldestTime) {
			oldest = biz
			oldestTime = acct.LastSeen
		}
	}
	if oldest != "" {
		delete(s.items, oldest)
	}
}

// copyAccount 返回账号副本,避免调用方直接改内部状态。
func copyAccount(acct *Account) Account {
	return *acct
}

// truncate 截断日志用的长 URL。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
