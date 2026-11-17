package proxy

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ltaoo/echo"

	"wx-mp-sync/internal/capture"
)

// TestCapturePlugin 验证代理插件会从请求 URL 与 Cookie 头里捕获凭证。
func TestCapturePlugin(t *testing.T) {
	store := capture.NewStore(time.Hour)
	plugin := NewCapturePlugin(store)

	req := httptest.NewRequest("GET",
		"https://mp.weixin.qq.com/s?__biz=MzA1&mid=1&idx=1&uin=u&key=k&pass_ticket=p", nil)
	req.Header.Set("Cookie", "sessionid=abc; wxuin=1")
	plugin.OnRequest(&echo.Context{Req: req})

	acct, ok := store.Get("MzA1")
	if !ok {
		t.Fatal("未捕获到账号")
	}
	if acct.Uin != "u" || acct.Key != "k" || acct.PassTicket != "p" {
		t.Errorf("凭证不完整: %+v", acct)
	}
	if acct.Cookie == "" {
		t.Errorf("Cookie 未捕获")
	}
}

// TestCapturePluginNilStore 验证未启用时插件被禁用而不报错。
func TestCapturePluginNilStore(t *testing.T) {
	plugin := NewCapturePlugin(nil)
	if !plugin.Disabled {
		t.Errorf("store 为 nil 时应禁用插件")
	}
}
