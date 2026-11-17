package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wx-mp-sync/internal/capture"
	"wx-mp-sync/pkg/scraper/wxmp"
)

// TestDiagCapturesNickname 验证页面诊断会上报公众号昵称。
func TestDiagCapturesNickname(t *testing.T) {
	s, _ := newTestServer(t)
	store := capture.NewStore(time.Hour)
	s.SetCapture(store)

	body := `{"stage":"mount","biz":"MzA1","nick":"诊断号","links":0}`
	req := httptest.NewRequest(http.MethodPost, "/api/diag", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleDiag(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	acct, ok := store.Get("MzA1")
	if !ok || acct.Nickname != "诊断号" {
		t.Fatalf("昵称未捕获: %+v ok=%v", acct, ok)
	}
}

// TestAccountsList 验证账号列表接口(不返回凭证)。
func TestAccountsList(t *testing.T) {
	s, _ := newTestServer(t)
	store := capture.NewStore(time.Hour)
	s.SetCapture(store)

	get := func() []accountView {
		rec := httptest.NewRecorder()
		s.handleAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
		var res struct {
			OK       bool          `json:"ok"`
			Accounts []accountView `json:"accounts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || !res.OK {
			t.Fatalf("响应 = %s", rec.Body.String())
		}
		return res.Accounts
	}

	if got := get(); len(got) != 0 {
		t.Fatalf("初始应为空: %+v", got)
	}

	store.CaptureRequest("https://mp.weixin.qq.com/s?__biz=MzA1&uin=u&key=k&pass_ticket=p", "sessionid=abc")
	store.SetNickname("MzA1", "测试号")
	list := get()
	if len(list) != 1 || list[0].Biz != "MzA1" || list[0].Nickname != "测试号" || !list[0].Usable || !list[0].HasCookie {
		t.Fatalf("列表 = %+v", list)
	}
	if strings.Contains(recBodyOf(t, s.handleAccounts), "pass_ticket") {
		t.Errorf("接口不应返回凭证字段")
	}
}

// recBodyOf 调用账号列表接口并返回原始响应体。
func recBodyOf(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	return rec.Body.String()
}

// TestAccountLatest 验证用捕获的凭证拉取最新文章。
func TestAccountLatest(t *testing.T) {
	s, _ := newTestServer(t)
	store := capture.NewStore(time.Hour)
	s.SetCapture(store)
	store.CaptureRequest("https://mp.weixin.qq.com/s?__biz=MzA1&uin=u&key=k&pass_ticket=p", "")
	store.SetNickname("MzA1", "测试号")

	var gotAcct *wxmp.OfficialAccount
	s.fetchLatest = func(acct *wxmp.OfficialAccount, limit int) ([]wxmp.FeedArticle, error) {
		gotAcct = acct
		if limit != 5 {
			t.Errorf("limit = %d", limit)
		}
		return []wxmp.FeedArticle{
			{Title: "第一篇", URL: "https://mp.weixin.qq.com/s?__biz=MzA1&mid=1&idx=1", Datetime: 1790000000},
			{Title: "第二篇", URL: "https://mp.weixin.qq.com/s?__biz=MzA1&mid=2&idx=1"},
		}, nil
	}

	rec := httptest.NewRecorder()
	s.handleAccountLatest(rec, httptest.NewRequest(http.MethodGet, "/api/accounts/latest?biz=MzA1&limit=5", nil))
	var res struct {
		OK    bool                `json:"ok"`
		Count int                 `json:"count"`
		Items []latestArticleView `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析失败: %v body=%s", err, rec.Body.String())
	}
	if !res.OK || res.Count != 2 || len(res.Items) != 2 {
		t.Fatalf("响应 = %s", rec.Body.String())
	}
	if res.Items[0].Title != "第一篇" || res.Items[0].DateTime == "" {
		t.Errorf("第一条 = %+v", res.Items[0])
	}
	if res.Items[1].DateTime != "" {
		t.Errorf("无时间时不应格式化: %+v", res.Items[1])
	}
	if gotAcct == nil || gotAcct.Biz != "MzA1" || gotAcct.Cookie != "" {
		t.Errorf("传给 getmsg 的账号 = %+v", gotAcct)
	}
	if gotAcct.Name != "测试号" || gotAcct.Key != "k" || gotAcct.Uin != "u" {
		t.Errorf("账号字段 = %+v", gotAcct)
	}
}

// TestAccountLatestErrors 验证参数缺失、账号不存在与 getmsg 失败时的提示。
func TestAccountLatestErrors(t *testing.T) {
	s, _ := newTestServer(t)
	store := capture.NewStore(time.Hour)
	s.SetCapture(store)
	store.CaptureRequest("https://mp.weixin.qq.com/s?__biz=MzA1&uin=u&key=k", "")

	cases := []struct {
		name    string
		url     string
		wantErr string
		code    int
	}{
		{"缺少 biz", "/api/accounts/latest", "缺少 biz", http.StatusBadRequest},
		{"账号不存在", "/api/accounts/latest?biz=MzZZ", "没有捕获到", http.StatusNotFound},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		s.handleAccountLatest(rec, httptest.NewRequest(http.MethodGet, c.url, nil))
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.wantErr) {
			t.Errorf("%s: code=%d body=%s", c.name, rec.Code, rec.Body.String())
		}
	}

	s.fetchLatest = func(*wxmp.OfficialAccount, int) ([]wxmp.FeedArticle, error) {
		return nil, &captureError{"getmsg 接口返回错误 ret=-3 errmsg=no session"}
	}
	rec := httptest.NewRecorder()
	s.handleAccountLatest(rec, httptest.NewRequest(http.MethodGet, "/api/accounts/latest?biz=MzA1", nil))
	if !strings.Contains(rec.Body.String(), "会话已失效") {
		t.Errorf("未给出会话失效提示: %s", rec.Body.String())
	}
}

// captureError 是测试用的错误类型。
type captureError struct{ msg string }

func (e *captureError) Error() string { return e.msg }
