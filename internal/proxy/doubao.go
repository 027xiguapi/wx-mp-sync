package proxy

import (
	"fmt"

	"github.com/ltaoo/echo"
)

// NewDoubaoInjectPlugin 返回一个注入插件:在豆包分享页右下角加「⬇ 下载本页」按钮,
// 点击后由本地管理服务抓取该分享并打包成 ZIP 下载。
func NewDoubaoInjectPlugin(opts InjectOptions) *echo.Plugin {
	return newInjectPlugin(opts, func(nonce string) string {
		return buildDoubaoInjection(opts.AdminBase, nonce)
	})
}

// buildDoubaoInjection 构造豆包下载按钮的注入脚本。
// 脚本只在 /thread/<id> 分享页显示按钮,SPA 切换路由后会自动补挂。
func buildDoubaoInjection(adminBase, nonce string) string {
	nonceAttr := ""
	if nonce != "" {
		nonceAttr = fmt.Sprintf(` nonce="%s"`, nonce)
	}
	script := `
(function(){
  if (window.__doubaoDlBooted) { return; }
  window.__doubaoDlBooted = true;
  var admin = '` + adminBase + `';
  var BTN_ID = 'doubao-dl-btn';
  var TOAST_ID = 'doubao-dl-toast';

  function isSharePage(){
    var host = location.hostname;
    if (host !== 'doubao.com' && host.slice(-11) !== '.doubao.com') { return false; }
    return /^\/thread\/[A-Za-z0-9_-]+/.test(location.pathname);
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
    return 'doubao-article.zip';
  }

  function download(){
    var btn = document.getElementById(BTN_ID);
    if (!btn || btn.disabled) { return; }
    btn.disabled = true;
    btn.textContent = '下载中…';
    fetch(admin + '/api/doubao/one', {
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
      btn.textContent = '⬇ 下载本页';
      btn.disabled = false;
      toast('✗ ' + ((err && err.message) || '下载失败'), false);
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
      'padding:10px 16px;border:0;border-radius:20px;background:#ff6a00;color:#fff;' +
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
