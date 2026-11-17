package proxy

import (
	"fmt"
	"strings"

	"github.com/ltaoo/echo"
)

// NewXHSInjectPlugin 在小红书笔记页右下角加「⬆ 同步到奶茶圈」按钮,
// 点击后把页面里的笔记数据发给本地管理服务,由它下载图片并发布到奶茶圈。
// 同时把本地服务地址加入页面 CSP 的 connect-src,避免 fetch 被拦截。
func NewXHSInjectPlugin(opts InjectOptions) *echo.Plugin {
	return newInjectPluginWithConnectSrc(opts, func(nonce string) string {
		return buildXHSInjection(opts.AdminBase, nonce)
	}, strings.TrimRight(opts.AdminBase, "/"))
}

// buildXHSInjection 构造小红书同步按钮的注入脚本。
func buildXHSInjection(adminBase, nonce string) string {
	nonceAttr := ""
	if nonce != "" {
		nonceAttr = fmt.Sprintf(` nonce="%s"`, nonce)
	}
	script := `
(function(){
  if (window.__xhsSyncBooted) { return; }
  window.__xhsSyncBooted = true;
  var admin = '` + adminBase + `';
  var BTN_ID = 'xhs-naicha-btn';
  var TOAST_ID = 'xhs-naicha-toast';
  var MAX_IMAGES = 18;

  function isNotePage(){
    var host = location.hostname;
    if (host !== 'xiaohongshu.com' && host.slice(-16) !== '.xiaohongshu.com') { return false; }
    return /^\/(explore|discovery\/item|note)\/[A-Za-z0-9]+/.test(location.pathname);
  }

  function noteIdFromPath(){
    var m = /^\/(?:explore|discovery\/item|note)\/([A-Za-z0-9]+)/.exec(location.pathname);
    return m ? m[1] : '';
  }

  function toast(text, ok, link){
    var t = document.getElementById(TOAST_ID);
    if (!t) {
      t = document.createElement('div');
      t.id = TOAST_ID;
      t.style.cssText = 'position:fixed;right:24px;bottom:150px;z-index:2147483647;' +
        'max-width:320px;padding:10px 14px;border-radius:8px;color:#fff;font-size:13px;' +
        'line-height:1.5;box-shadow:0 4px 16px rgba(0,0,0,.25);opacity:0;' +
        'transition:opacity .2s;pointer-events:auto;background:rgba(30,30,30,.92)';
      (document.body || document.documentElement).appendChild(t);
    }
    t.innerHTML = '';
    var span = document.createElement('span');
    span.textContent = text;
    t.appendChild(span);
    if (link) {
      var a = document.createElement('a');
      a.href = link;
      a.target = '_blank';
      a.rel = 'noopener';
      a.textContent = ' 查看帖子';
      a.style.cssText = 'color:#8fd3ff;text-decoration:underline;margin-left:4px';
      t.appendChild(a);
    }
    t.style.background = ok ? 'rgba(30,30,30,.92)' : 'rgba(200,40,40,.95)';
    t.style.opacity = '1';
    if (t.__hideTimer) { clearTimeout(t.__hideTimer); }
    t.__hideTimer = setTimeout(function(){ t.style.opacity = '0'; }, link ? 8000 : 4000);
  }

  function pickNote(){
    var state = window.__INITIAL_STATE__ || window.__INITIAL_SSR_STATE__;
    if (!state) { return null; }

    var noteId = noteIdFromPath();
    var note = null;
    var noteSection = state.note;
    if (noteSection) {
      if (noteSection.currentNoteId) { noteId = noteSection.currentNoteId; }
      var map = noteSection.noteDetailMap;
      if (map) {
        var entry = map[noteId];
        if (!entry) {
          var keys = Object.keys(map);
          if (keys.length > 0) { entry = map[keys[0]]; }
        }
        if (entry) { note = entry.note || entry; }
      }
    }
    if (!note && state.noteData && state.noteData.data) {
      note = state.noteData.data.noteData || state.noteData.data.note;
    }
    if (!note) { return null; }

    noteId = note.noteId || note.noteID || note.id || noteId;
    var user = note.user || note.author || {};
    var rawImages = note.imageList || note.images || [];
    var images = [];
    for (var i = 0; i < rawImages.length && images.length < MAX_IMAGES; i++) {
      var img = rawImages[i];
      if (!img) { continue; }
      if (typeof img === 'string') {
        images.push({ urlDefault: img, urlPre: '', url: '', width: 0, height: 0, infoList: [] });
        continue;
      }
      var infos = [];
      var rawInfos = img.infoList || [];
      for (var j = 0; j < rawInfos.length; j++) {
        var info = rawInfos[j] || {};
        infos.push({ url: info.url || '', imageScene: info.imageScene || '' });
      }
      images.push({
        urlDefault: img.urlDefault || '',
        urlPre: img.urlPre || '',
        url: img.url || '',
        width: img.width || 0,
        height: img.height || 0,
        infoList: infos
      });
    }

    var rawTags = note.tagList || note.tags || [];
    var tags = [];
    for (var k = 0; k < rawTags.length; k++) {
      var tag = rawTags[k];
      var name = typeof tag === 'string' ? tag : ((tag && (tag.name || tag.title)) || '');
      if (name) { tags.push({ name: name }); }
    }

    return {
      url: location.href,
      note: {
        noteId: noteId,
        title: note.title || '',
        desc: note.desc || note.content || '',
        type: note.type || '',
        time: note.time || 0,
        user: {
          nickname: user.nickname || user.nickName || user.name || '',
          userId: user.userId || ''
        },
        imageList: images,
        tagList: tags
      }
    };
  }

  function syncNote(){
    var btn = document.getElementById(BTN_ID);
    if (!btn || btn.disabled) { return; }
    var payload = pickNote();
    if (!payload || !payload.note.noteId) {
      toast('页面数据还没加载完,请稍等几秒再点', false);
      return;
    }
    btn.disabled = true;
    btn.textContent = '同步中…';
    fetch(admin + '/api/xhs/sync', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(function(res){
      return res.json().catch(function(){ return {}; }).then(function(json){
        if (!res.ok) { throw new Error((json && json.error) || ('HTTP ' + res.status)); }
        return json;
      });
    }).then(function(res){
      if (res && res.ok) {
        var text = res.duplicate ? '✓ 这篇笔记已经同步过' : ('✓ 已同步 ' + (res.images || 0) + ' 张图片到奶茶圈');
        toast(text, true, res.url || '');
        btn.textContent = res.duplicate ? '✓ 已同步过' : '✓ 已同步';
      } else {
        toast('✗ ' + ((res && res.error) || '同步失败'), false, '');
        btn.textContent = '⬆ 同步到奶茶圈';
      }
      setTimeout(function(){ btn.textContent = '⬆ 同步到奶茶圈'; btn.disabled = false; }, 5000);
    }).catch(function(err){
      toast('✗ ' + ((err && err.message) || '同步失败'), false, '');
      btn.textContent = '⬆ 同步到奶茶圈';
      btn.disabled = false;
    });
  }

  function addButton(){
    var existing = document.getElementById(BTN_ID);
    if (!isNotePage()) {
      if (existing) { existing.style.display = 'none'; }
      return;
    }
    if (!document.body) { return; }
    if (existing) { existing.style.display = 'block'; return; }
    var b = document.createElement('button');
    b.id = BTN_ID;
    b.type = 'button';
    b.textContent = '⬆ 同步到奶茶圈';
    b.title = '把这篇小红书笔记的正文和图片发布到奶茶圈';
    b.style.cssText = 'position:fixed;right:24px;bottom:120px;z-index:2147483647;' +
      'padding:10px 16px;border:0;border-radius:20px;background:#ff2442;color:#fff;' +
      'font-size:14px;font-weight:600;cursor:pointer;box-shadow:0 6px 18px rgba(0,0,0,.22);' +
      'font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Microsoft YaHei",sans-serif';
    b.addEventListener('click', syncNote);
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
