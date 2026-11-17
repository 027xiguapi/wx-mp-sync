package proxy

import (
	"github.com/ltaoo/echo"

	"wx-mp-sync/internal/capture"
)

// NewCapturePlugin 从 mp.weixin.qq.com 的请求里捕获公众号凭证。
//
// 微信「搜一搜」等本地页面无法注入按钮,但用户打开公众号文章/历史消息时,
// 请求 URL 里带着 __biz / uin / key / pass_ticket,Cookie 头里带着会话。
// 捕获这些信息后,管理页就能调用 getmsg 拉取该公众号的最新文章并批量下载。
func NewCapturePlugin(store *capture.Store) *echo.Plugin {
	plugin := &echo.Plugin{Match: "mp.weixin.qq.com"}
	if store == nil {
		plugin.Disabled = true
		return plugin
	}
	plugin.OnRequest = func(ctx *echo.Context) {
		if ctx.Req == nil || ctx.Req.URL == nil {
			return
		}
		host := ctx.Req.Host
		if host == "" {
			host = ctx.Req.URL.Host
		}
		if host == "" {
			return
		}
		raw := "https://" + host + ctx.Req.URL.RequestURI()
		store.CaptureRequest(raw, ctx.Req.Header.Get("Cookie"))
	}
	return plugin
}
