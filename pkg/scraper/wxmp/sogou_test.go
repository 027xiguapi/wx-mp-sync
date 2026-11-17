package wxmp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsSogouLink 验证搜狗跳转链接判定。
func TestIsSogouLink(t *testing.T) {
	cases := map[string]bool{
		"https://weixin.sogou.com/link?url=abc&type=2": true,
		"http://weixin.sogou.com/link?url=abc":         true,
		"https://weixin.sogou.com/weixin?type=2":       false,
		"https://mp.weixin.qq.com/s/abc":               false,
		"https://example.com/link?url=abc":             false,
		"":                                             false,
	}
	for raw, want := range cases {
		if got := IsSogouLink(raw); got != want {
			t.Errorf("IsSogouLink(%q) = %v, 期望 %v", raw, got, want)
		}
	}
}

// sogouRedirectPage 模拟真实搜狗跳转页:链接被拆成多段并插入 @ 干扰。
const sogouRedirectPage = `<html><body><script>
var url = '';
url += 'https://mp.';
url += 'weixin.qq.c';
url += 'om/s?src=11';
url += '&amp;timestamp=';
url += '1792461377&';
url += '@ver=7037&amp;si';
url += 'gnature=CR2j5lMVVq';
url += 'm&new=1';
url.replace("@", "");
window.location.replace(url)
</script></body></html>`

// TestParseSogouRedirect 验证拆分链接能被还原、@ 被去掉、HTML 实体被解码。
func TestParseSogouRedirect(t *testing.T) {
	got, err := ParseSogouRedirect(sogouRedirectPage)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := "https://mp.weixin.qq.com/s?src=11&timestamp=1792461377&ver=7037&signature=CR2j5lMVVqm&new=1"
	if got != want {
		t.Errorf("解析结果 = %q\n期望 %q", got, want)
	}
	if !strings.Contains(got, "mp.weixin.qq.com/s") {
		t.Errorf("解析结果不是文章链接: %q", got)
	}
}

// TestParseSogouRedirectLocation 验证 location.replace 形式的兜底解析。
func TestParseSogouRedirectLocation(t *testing.T) {
	page := `<script>window.location.replace("https://mp.weixin.qq.com/s?src=11\x26timestamp=1")</script>`
	got, err := ParseSogouRedirect(page)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != "https://mp.weixin.qq.com/s?src=11&timestamp=1" {
		t.Errorf("解析结果 = %q", got)
	}
}

// TestParseSogouRedirectEmpty 验证无法解析时返回错误。
func TestParseSogouRedirectEmpty(t *testing.T) {
	if _, err := ParseSogouRedirect("<html>什么都没有</html>"); err == nil {
		t.Fatal("空页应当返回错误")
	}
}

// TestResolveSogouLink 验证 HTTP 层:正常解析与验证码拦截。
func TestResolveSogouLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if strings.Contains(r.URL.RawQuery, "captcha") {
			_, _ = io.WriteString(w, `<html>请输入验证码 <img src="/antispider/"></html>`)
			return
		}
		_, _ = io.WriteString(w, sogouRedirectPage)
	}))
	defer srv.Close()

	got, err := resolveSogouLinkWithClient(srv.Client(), srv.URL+"/link?url=abc")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(got, "mp.weixin.qq.com/s?src=11") {
		t.Errorf("解析结果 = %q", got)
	}

	if _, err := resolveSogouLinkWithClient(srv.Client(), srv.URL+"/link?url=abc&captcha=1"); err == nil {
		t.Fatal("验证码页应当返回错误")
	} else if !strings.Contains(err.Error(), "验证码") {
		t.Errorf("错误信息不明确: %v", err)
	}

	if _, err := ResolveSogouLink("https://example.com/link?url=abc"); err == nil {
		t.Fatal("非搜狗链接应当返回错误")
	}
}
