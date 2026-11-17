package capture

import (
	"strings"
	"testing"
	"time"
)

// TestCaptureRequest 验证从文章 URL 中提取凭证。
func TestCaptureRequest(t *testing.T) {
	s := NewStore(time.Hour)
	raw := "https://mp.weixin.qq.com/s?__biz=MzY4MTE2MzM1MA==&mid=2247484796&idx=1&sn=abc" +
		"&key=daf9bdc5abc4e8d0&pass_ticket=rgF8rcv6LTnOxg&uin=MjYwMDcxNzEwMg%3D%3D"
	acct := s.CaptureRequest(raw, "sessionid=abc; wxuin=1")
	if acct == nil {
		t.Fatal("未捕获到账号")
	}
	if acct.Biz != "MzY4MTE2MzM1MA==" {
		t.Errorf("biz = %q", acct.Biz)
	}
	if acct.Uin != "MjYwMDcxNzEwMg==" {
		t.Errorf("uin 未解码/保存: %q", acct.Uin)
	}
	if acct.Key == "" || acct.PassTicket == "" || !acct.Usable {
		t.Errorf("凭证不完整: %+v", acct)
	}
	if acct.Cookie == "" {
		t.Errorf("未保存 Cookie")
	}
	if got, ok := s.Get("MzY4MTE2MzM1MA=="); !ok || got.Key != acct.Key {
		t.Errorf("Get = %+v, ok=%v", got, ok)
	}
}

// TestCaptureRequestIgnoresNonArticle 验证无关 URL 不会被记录。
func TestCaptureRequestIgnoresNonArticle(t *testing.T) {
	s := NewStore(time.Hour)
	for _, u := range []string{
		"https://mp.weixin.qq.com/mp/homepage",
		"https://mp.weixin.qq.com/s/abc",
		"https://mp.weixin.qq.com/s?__biz=MzA5",
		"https://example.com/?__biz=MzA5&key=k",
	} {
		if acct := s.CaptureRequest(u, ""); acct != nil {
			t.Errorf("%s 不应被捕获,得到 %+v", u, acct)
		}
	}
	if len(s.List()) != 0 {
		t.Errorf("列表应为空: %+v", s.List())
	}
}

// TestCaptureMergeAndNickname 验证多次捕获会合并字段,昵称可补全。
func TestCaptureMergeAndNickname(t *testing.T) {
	s := NewStore(time.Hour)
	s.CaptureRequest("https://mp.weixin.qq.com/s?__biz=MzA1&uin=u1", "")
	s.CaptureRequest("https://mp.weixin.qq.com/s?__biz=MzA1&key=k1&pass_ticket=p1", "")
	s.SetNickname("MzA1", "")
	s.SetNickname("MzA1", "测试公众号")

	acct, ok := s.Get("MzA1")
	if !ok {
		t.Fatal("账号丢失")
	}
	if acct.Uin != "u1" || acct.Key != "k1" || acct.PassTicket != "p1" {
		t.Errorf("字段未合并: %+v", acct)
	}
	if acct.Nickname != "测试公众号" {
		t.Errorf("昵称 = %q", acct.Nickname)
	}
	if !acct.Usable {
		t.Errorf("uin+key 齐备时应可用")
	}
}

// TestStoreExpiry 验证过期账号会被清理。
func TestStoreExpiry(t *testing.T) {
	s := NewStore(20 * time.Millisecond)
	s.CaptureRequest("https://mp.weixin.qq.com/s?__biz=MzA2&uin=u&key=k", "")
	time.Sleep(40 * time.Millisecond)
	if got := s.List(); len(got) != 0 {
		t.Errorf("过期账号未清理: %+v", got)
	}
}

// TestListOrder 验证列表按最近出现时间倒序。
func TestListOrder(t *testing.T) {
	s := NewStore(time.Hour)
	s.CaptureRequest("https://mp.weixin.qq.com/s?__biz=OLD&uin=u&key=k", "")
	time.Sleep(5 * time.Millisecond)
	s.CaptureRequest("https://mp.weixin.qq.com/s?__biz=NEW&uin=u&key=k", "")
	list := s.List()
	if len(list) != 2 || list[0].Biz != "NEW" {
		t.Fatalf("排序不正确: %+v", list)
	}
	if !strings.HasPrefix(list[0].SourceURL, "https://mp.weixin.qq.com/s?__biz=NEW") {
		t.Errorf("SourceURL = %q", list[0].SourceURL)
	}
}
