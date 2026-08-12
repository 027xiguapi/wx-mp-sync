package proxy

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ltaoo/echo"
)

var cspNonceReg = regexp.MustCompile(`'nonce-([^']+)'`)

// InjectOptions 配置注入插件的目标域名与管理服务地址。
type InjectOptions struct {
	// Match 是 echo 的域名匹配模式,如 "mp.weixin.qq.com"。
	Match string
	// AdminBase 是本地管理服务地址,如 "http://127.0.0.1:8080"。
	AdminBase string
}

// NewInjectPlugin 返回一个 echo 插件:拦截匹配域名的 text/html 响应,
// 在 </body> 前注入"同步"按钮。
func NewInjectPlugin(opts InjectOptions) *echo.Plugin {
	return &echo.Plugin{
		Match: opts.Match,
		OnResponse: func(ctx *echo.Context) {
			if ctx.Res == nil {
				return
			}
			contentType := strings.ToLower(ctx.GetResponseHeader("Content-Type"))
			if !strings.Contains(contentType, "text/html") {
				return
			}
			body, err := ctx.GetResponseBody()
			if err != nil {
				return
			}
			idx := strings.LastIndex(body, "</body>")
			if idx < 0 {
				return
			}
			injection := buildInjection(opts.AdminBase, ctx.GetResponseHeader("Content-Security-Policy"))
			ctx.SetResponseBody(body[:idx] + injection + body[idx:])
		},
	}
}

// buildInjection 构造注入的按钮 HTML 与脚本。
// 样式全部通过 JS 设置,规避页面 CSP 对 <style>/style 属性的限制。
// 若响应带 CSP nonce,则把 nonce 加到 script 标签上以保证执行。
func buildInjection(adminBase, csp string) string {
	nonceAttr := ""
	if m := cspNonceReg.FindStringSubmatch(csp); len(m) > 1 {
		nonceAttr = fmt.Sprintf(` nonce="%s"`, m[1])
	}

	script := `
(function(){
  if (window.__wxMpSyncInjected) { return; }
  window.__wxMpSyncInjected = true;
  var btn = document.getElementById('wx-mp-sync-btn');
  if (!btn) { return; }
  function css(el, styles){
    for (var k in styles) { el.style[k] = styles[k]; }
  }
  css(btn, {
    position:'fixed', right:'14px', bottom:'120px', zIndex:'2147483647',
    display:'flex', alignItems:'center', gap:'6px',
    padding:'8px 14px', borderRadius:'20px', background:'#07c160',
    color:'#fff', fontSize:'14px', lineHeight:'1', cursor:'pointer',
    boxShadow:'0 2px 8px rgba(0,0,0,.25)', fontFamily:'-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif',
    userSelect:'none', WebkitUserSelect:'none'
  });
  function setState(cls, text){
    btn.className = cls || '';
    btn.textContent = text;
  }
  btn.addEventListener('click', function(){
    var payload = {
      url: location.href,
      title: (window.cgiDataNew && window.cgiDataNew.title) || document.title,
      nickname: (window.cgiDataNew && window.cgiDataNew.nick_name) || '',
      cgiDataNew: window.cgiDataNew || null
    };
    setState('syncing', '同步中…');
    fetch('` + adminBase + `/api/save', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify(payload)
    }).then(function(r){ return r.json(); }).then(function(res){
      if (res && res.ok) {
        var base = res.duplicate ? '✓ 已存在' : '✓ 已保存';
        if (res.bot && res.bot.ok) { base += '·已推送'; }
        else if (res.bot && !res.bot.ok) { base += '·推送失败'; }
        setState('done', base);
      } else {
        setState('err', '✗ ' + ((res && res.error) || '保存失败'));
      }
    }).catch(function(){
      setState('err', '✗ 本地程序未运行');
    });
    setTimeout(function(){ setState('', '⬇ 同步'); }, 3500);
  });
})();
`

	return `<div id="wx-mp-sync-btn">⬇ 同步</div>` +
		`<script` + nonceAttr + `>` + script + `</script>`
}
