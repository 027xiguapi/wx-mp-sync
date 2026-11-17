package wxmp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeMsgList 构造一条 getmsg 响应。
func fakeMsgList(t *testing.T, hasMore int, nextOffset int, list []map[string]any) *httptest.Server {
	t.Helper()
	general, err := json.Marshal(map[string]any{"list": list})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := map[string]any{
		"ret":              0,
		"errmsg":           "ok",
		"general_msg_list": string(general),
		"can_msg_continue": hasMore,
		"next_offset":      nextOffset,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mp/profile_ext" || r.URL.Query().Get("action") != "getmsg" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Cookie") != "" {
			w.Header().Set("X-Saw-Cookie", "1")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// articleItem 构造一条推送项(可含多图文)。
func articleItem(datetime int, title, url string, subs ...map[string]any) map[string]any {
	return map[string]any{
		"comm_msg_info": map[string]any{"datetime": datetime, "type": 49},
		"app_msg_ext_info": map[string]any{
			"title":                   title,
			"content_url":             url,
			"cover":                   "https://mmbiz.qpic.cn/cover.jpg",
			"digest":                  "摘要 &amp; 说明",
			"author":                  "作者",
			"is_multi":                1,
			"multi_app_msg_item_list": subs,
		},
	}
}

// TestFetchLatestArticles 验证最新文章列表的解析、链接补全与数量限制。
func TestFetchLatestArticles(t *testing.T) {
	list := []map[string]any{
		articleItem(1790000000, "头条文章", "/s?__biz=MzA1&mid=1&idx=1&sn=a"),
		articleItem(1789990000, "多图文头条", "/s?__biz=MzA1&mid=2&idx=1&sn=b",
			map[string]any{"title": "次条一", "content_url": "/s?__biz=MzA1&mid=2&idx=2&sn=c", "author": "作者2"},
			map[string]any{"title": "次条二", "content_url": "/s?__biz=MzA1&mid=2&idx=3&sn=d"},
		),
	}
	srv := fakeMsgList(t, 0, 0, list)
	defer srv.Close()

	c := NewAccountClient(nil)
	c.BaseURL = srv.URL
	c.MaxPages = 2
	acct := &OfficialAccount{Biz: "MzA1", Uin: "u", Key: "k", PassTicket: "p", Cookie: "sessionid=abc"}

	items, err := c.FetchLatestArticles(acct, 10)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("文章数 = %d, 期望 4(1 头条 + 多图文 3 篇): %+v", len(items), items)
	}
	if items[0].Title != "头条文章" || !strings.HasPrefix(items[0].URL, "https://mp.weixin.qq.com/s?__biz=MzA1") {
		t.Errorf("第一条 = %+v", items[0])
	}
	if items[0].Datetime != 1790000000 {
		t.Errorf("时间 = %d", items[0].Datetime)
	}
	if items[2].URL != "https://mp.weixin.qq.com/s?__biz=MzA1&mid=2&idx=2&sn=c" {
		t.Errorf("多图文链接补全不正确: %s", items[2].URL)
	}
	if !strings.Contains(items[0].Digest, "摘要 & 说明") {
		t.Errorf("实体未解码: %q", items[0].Digest)
	}

	// limit 生效
	items, err = c.FetchLatestArticles(acct, 2)
	if err != nil || len(items) != 2 {
		t.Fatalf("limit 未生效: %d %v", len(items), err)
	}

	// Cookie 会带到请求里
	resp, err := c.FetchWithCookie(c.BuildMsgListURL(acct, 0), c.BuildMsgListReferer(acct), acct.Cookie)
	if err != nil {
		t.Fatalf("FetchWithCookie: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if got := resp.Header.Get("X-Saw-Cookie"); got != "1" {
		t.Errorf("请求未带 Cookie")
	}
}

// TestFetchMsgListError 验证 getmsg 返回错误时的处理。
func TestFetchMsgListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ret":-3,"errmsg":"no session","base_resp":{"ret":-3}}`)
	}))
	defer srv.Close()

	c := NewAccountClient(nil)
	c.BaseURL = srv.URL
	_, err := c.FetchLatestArticles(&OfficialAccount{Biz: "MzA1", Uin: "u", Key: "k"}, 5)
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if !strings.Contains(err.Error(), "no session") {
		t.Errorf("错误信息 = %v", err)
	}
}

// TestAbsoluteContentURL 验证链接补全与转义处理。
func TestAbsoluteContentURL(t *testing.T) {
	cases := map[string]string{
		"":                                      "",
		"/s?__biz=MzA1&amp;mid=1":               "https://mp.weixin.qq.com/s?__biz=MzA1&mid=1",
		"http://mp.weixin.qq.com/s?__biz=MzA1":  "http://mp.weixin.qq.com/s?__biz=MzA1",
		"https://mp.weixin.qq.com/s?__biz=MzA1": "https://mp.weixin.qq.com/s?__biz=MzA1",
	}
	for in, want := range cases {
		if got := absoluteContentURL(in); got != want {
			t.Errorf("absoluteContentURL(%q) = %q, 期望 %q", in, got, want)
		}
	}
}
