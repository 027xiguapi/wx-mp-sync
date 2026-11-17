package proxy

import (
	"fmt"
	"strings"

	"github.com/ltaoo/echo"
)

// NewChatGPTInjectPlugin 在 ChatGPT 分享页右下角加「⬇ 下载本页」按钮,
// 点击后由本地管理服务抓取该分享并打包成 ZIP 下载。
// 同时把本地服务地址加入页面 CSP 的 connect-src,否则 fetch 会被浏览器拦截。
func NewChatGPTInjectPlugin(opts InjectOptions) *echo.Plugin {
	return newInjectPluginWithConnectSrc(opts, func(nonce string) string {
		return buildChatGPTInjection(opts.AdminBase, nonce)
	}, strings.TrimRight(opts.AdminBase, "/"))
}

// buildChatGPTInjection 构造 ChatGPT 分享下载按钮的注入脚本。
func buildChatGPTInjection(adminBase, nonce string) string {
	nonceAttr := ""
	if nonce != "" {
		nonceAttr = fmt.Sprintf(` nonce="%s"`, nonce)
	}
	script := `
(function(){
  if (window.__chatgptDlBooted) { return; }
  window.__chatgptDlBooted = true;
  var admin = '` + adminBase + `';
  var BTN_ID = 'chatgpt-dl-btn';
  var TOAST_ID = 'chatgpt-dl-toast';

  function isSharePage(){
    var host = location.hostname;
    if (host !== 'chatgpt.com' && host !== 'chat.openai.com') { return false; }
    return /^\/(s|share)\/[A-Za-z0-9_-]+/.test(location.pathname);
  }

  function toast(text, ok){
    var t = document.getElementById(TOAST_ID);
    if (!t) {
      t = document.createElement('div');
      t.id = TOAST_ID;
      t.style.cssText = 'position:fixed;right:24px;bottom:150px;z-index:2147483647;' +
        'max-width:320px;padding:10px 14px;border-radius:8px;color:#fff;font-size:13px;' +
        'line-height:1.5;box-shadow:0 4px 16px rgba(0,0,0,.25);opacity:0;' +
        'transition:opacity .2s;pointer-events:none;background:rgba(30,30,30,.92)';
      (document.body || document.documentElement).appendChild(t);
    }
    t.textContent = text;
    t.style.background = ok ? 'rgba(30,30,30,.92)' : 'rgba(200,40,40,.95)';
    t.style.opacity = '1';
    if (t.__hideTimer) { clearTimeout(t.__hideTimer); }
    t.__hideTimer = setTimeout(function(){ t.style.opacity = '0'; }, 4000);
  }

  function fileName(cd){
    cd = cd || '';
    var m = /filename\*=UTF-8''([^;]+)/i.exec(cd);
    if (m) { try { return decodeURIComponent(m[1]); } catch (e) {} }
    m = /filename="?([^";]+)"?/i.exec(cd);
    if (m) { return m[1]; }
    return 'chatgpt-share.zip';
  }

  function download(){
    var btn = document.getElementById(BTN_ID);
    if (!btn || btn.disabled) { return; }
    btn.disabled = true;
    btn.textContent = '下载中…';
    fetch(admin + '/api/chatgpt/one', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: location.href })
    }).then(function(res){
      var ct = (res.headers.get('Content-Type') || '').toLowerCase();
      if (ct.indexOf('application/json') !== -1 || !res.ok) {
        return res.json().catch(function(){ return {}; }).then(function(j){
          throw new Error((j && j.error) || ('HTTP ' + res.status));
        });
      }
      var name = fileName(res.headers.get('Content-Disposition'));
      return res.blob().then(function(b){ return { blob: b, name: name }; });
    }).then(function(out){
      var url = URL.createObjectURL(out.blob);
      var a = document.createElement('a');
      a.href = url;
      a.download = out.name;
      a.style.display = 'none';
      document.body.appendChild(a);
      a.click();
      setTimeout(function(){
        if (a.parentNode) { a.parentNode.removeChild(a); }
        URL.revokeObjectURL(url);
      }, 10000);
      btn.textContent = '✓ 已下载';
      toast('✓ 已打包下载:' + out.name, true);
      setTimeout(function(){ btn.textContent = '⬇ 下载本页'; btn.disabled = false; }, 4000);
    }).catch(function(err){
      var msg = String((err && (err.message || err)) || '');
      // 页面 CSP 或浏览器策略拦截了本地 fetch 时,退化为顶层导航直接下载。
      if (/failed to fetch|content security policy|refused to connect|networkerror/i.test(msg)) {
        toast('页面 CSP 拦截了本地请求,改为直接下载…', false);
        window.location.href = admin + '/api/chatgpt/one?nav=1&url=' + encodeURIComponent(location.href);
        return;
      }
      btn.textContent = '⬇ 下载本页';
      btn.disabled = false;
      toast('✗ ' + (msg || '下载失败'), false);
    });
  }

  function addButton(){
    var existing = document.getElementById(BTN_ID);
    if (!isSharePage()) {
      if (existing) { existing.style.display = 'none'; }
      return;
    }
    if (!document.body) { return; }
    if (existing) { existing.style.display = 'block'; return; }
    var b = document.createElement('button');
    b.id = BTN_ID;
    b.type = 'button';
    b.textContent = '⬇ 下载本页';
    b.title = '下载本页对话的正文与全部图片(ZIP)';
    b.style.cssText = 'position:fixed;right:24px;bottom:96px;z-index:2147483647;' +
      'padding:10px 16px;border:0;border-radius:20px;background:#10a37f;color:#fff;' +
      'font-size:14px;font-weight:600;cursor:pointer;box-shadow:0 6px 18px rgba(0,0,0,.22);' +
      'font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Microsoft YaHei",sans-serif';
    b.addEventListener('click', download);
    (document.body || document.documentElement).appendChild(b);
  }

  setInterval(addButton, 1200);
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', addButton);
  } else {
    addButton();
  }
})();
`
	return "<script" + nonceAttr + ">" + script + "</script>"
}
