package proxy

import (
	"fmt"
	"html"
	"log"
	"regexp"
	"strings"

	"github.com/ltaoo/echo"
)

var (
	cspNonceReg  = regexp.MustCompile(`'nonce-([^']+)'`)
	htmlNonceReg = regexp.MustCompile(`(?i)nonce="([^"]+)"`)
	metaCSPTagRe = regexp.MustCompile(`(?is)<meta[^>]+http-equiv\s*=\s*["']?Content-Security-Policy["']?[^>]*>`)
	metaAttrRe   = regexp.MustCompile(`(?is)content\s*=\s*("([^"]*)"|'([^']*)')`)
)

// InjectOptions 配置注入插件的目标域名与管理服务地址。
type InjectOptions struct {
	// Match 是 echo 的域名匹配模式,如 "mp.weixin.qq.com"。
	Match string
	// AdminBase 是本地管理服务地址,如 "http://127.0.0.1:8080"。
	AdminBase string
}

// NewInjectPlugin 返回一个 echo 插件:拦截匹配域名的 text/html 响应,
// 在 </head> 前注入启动脚本;按钮由脚本创建并挂到 <html> 下,
// 页面框架重绘 body 后会自动补挂。
func NewInjectPlugin(opts InjectOptions) *echo.Plugin {
	return newInjectPlugin(opts, func(nonce string) string {
		return buildInjectionWithNonce(opts.AdminBase, nonce)
	})
}

// newInjectPlugin 是注入插件的通用实现:由 build 决定插入的 HTML 片段。
func newInjectPlugin(opts InjectOptions, build func(nonce string) string) *echo.Plugin {
	return newInjectPluginWithConnectSrc(opts, build, "")
}

// newInjectPluginWithConnectSrc 在注入的同时,把本地管理服务地址加进页面的
// CSP connect-src,供注入脚本 fetch 本地接口。Chrome 会严格按 connect-src 拦截。
func newInjectPluginWithConnectSrc(opts InjectOptions, build func(nonce string) string, connectSrc string) *echo.Plugin {
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

			csp := ctx.GetResponseHeader("Content-Security-Policy")
			nonce := extractCSPNonce(csp)
			if nonce == "" {
				// 有些页面只在 <meta http-equiv="Content-Security-Policy"> 里声明 CSP,
				// 这里直接复用页面已有脚本的 nonce,保证注入脚本能执行。
				if m := htmlNonceReg.FindStringSubmatch(body); len(m) > 1 {
					nonce = m[1]
				}
			}
			injection := build(nonce)

			// 允许注入的脚本访问本地管理服务。只修改已有的 connect-src,
			// 不新增指令,避免把页面其它网络请求一起限制掉。
			if connectSrc != "" {
				if updated, ok := addConnectSrc(csp, connectSrc); ok {
					ctx.SetResponseHeader("Content-Security-Policy", updated)
				}
				body = rewriteMetaCSP(body, connectSrc)
			}

			lower := strings.ToLower(body)
			headIdx := strings.Index(lower, "</head>")
			bodyIdx := strings.LastIndex(lower, "</body>")
			switch {
			case headIdx >= 0:
				body = body[:headIdx] + injection + body[headIdx:]
			case bodyIdx >= 0:
				body = body[:bodyIdx] + injection + body[bodyIdx:]
			default:
				log.Printf("[INJECT] 跳过(无 head/body): %s", requestURL(ctx))
				return
			}
			log.Printf("[INJECT] %s html=%d bytes csp=%t head=%t", requestURL(ctx), len(body), csp != "", headIdx >= 0)
			// 禁止缓存注入后的 HTML,避免微信复用旧页面导致改动不生效。
			ctx.SetResponseHeader("Cache-Control", "no-store, no-cache, must-revalidate")
			ctx.DelResponseHeader("ETag")
			ctx.DelResponseHeader("Last-Modified")
			ctx.SetResponseBody(body)
		},
	}
}

// requestURL 返回注入日志里使用的请求 URL。
func requestURL(ctx *echo.Context) string {
	if ctx.Req == nil || ctx.Req.URL == nil {
		return ""
	}
	return ctx.Req.URL.String()
}

// buildInjection 构造注入的启动脚本(测试用入口,nonce 从 CSP 字符串提取)。
func buildInjection(adminBase, csp string) string {
	return buildInjectionWithNonce(adminBase, extractCSPNonce(csp))
}

// extractCSPNonce 从 CSP 头中提取 nonce。
func extractCSPNonce(csp string) string {
	if m := cspNonceReg.FindStringSubmatch(csp); len(m) > 1 {
		return m[1]
	}
	return ""
}

// addConnectSrc 把本地地址追加到已有的 connect-src 指令。
// 没有 connect-src 时不新增(否则会把页面的其它请求全部限制掉)。
func addConnectSrc(csp, origin string) (string, bool) {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if csp == "" || origin == "" {
		return csp, false
	}
	directives := strings.Split(csp, ";")
	for i, directive := range directives {
		trimmed := strings.TrimSpace(directive)
		lower := strings.ToLower(trimmed)
		if lower == "connect-src" || strings.HasPrefix(lower, "connect-src ") {
			extra := " " + origin
			if strings.Contains(origin, "127.0.0.1") {
				extra += " http://127.0.0.1:*"
			}
			directives[i] = trimmed + extra
			return strings.Join(directives, ";"), true
		}
	}
	return csp, false
}

// rewriteMetaCSP 处理 <meta http-equiv="Content-Security-Policy"> 形式的内联 CSP。
func rewriteMetaCSP(body, origin string) string {
	if body == "" || !strings.Contains(strings.ToLower(body), "content-security-policy") {
		return body
	}
	return metaCSPTagRe.ReplaceAllStringFunc(body, func(tag string) string {
		return metaAttrRe.ReplaceAllStringFunc(tag, func(attr string) string {
			m := metaAttrRe.FindStringSubmatch(attr)
			if len(m) < 2 {
				return attr
			}
			value := m[2]
			if value == "" {
				value = m[3]
			}
			updated, ok := addConnectSrc(html.UnescapeString(value), origin)
			if !ok {
				return attr
			}
			return `content="` + html.EscapeString(updated) + `"`
		})
	})
}

// buildInjectionWithNonce 构造注入的启动脚本。
//
// 脚本有两种工作模式:
//  1. 文章页:右下角「⬇ 下载」「⬇ 同步」按钮,归档/打包当前文章。
//  2. 列表/搜索页(例如微信「搜一搜」的文章结果):自动进入多选模式,
//     点击结果卡片即可选中,底部工具条汇总后一次性批量下载;
//     服务端按链接逐篇抓取,归档为 HTML + Markdown + 图片并打包成 ZIP。
//
// 整个脚本只创建固定定位的元素,并持续监测页面重绘后自动补挂。
// nonce 非空时加到 script 标签上,以通过页面 CSP。
func buildInjectionWithNonce(adminBase, nonce string) string {
	nonceAttr := ""
	if nonce != "" {
		nonceAttr = fmt.Sprintf(` nonce="%s"`, nonce)
	}

	script := `
(function(){
  if (window.__wxMpSyncBooted) { return; }
  window.__wxMpSyncBooted = true;
  var admin = '` + adminBase + `';
  var BAR_ID = 'wx-mp-sync-bar';
  var PICK_BAR_ID = 'wx-mp-pick-bar';
  var PICK_LAUNCH_ID = 'wx-mp-pick-launch';
  var FONT = '-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif';
  var MAX_PICK = 100;
  // 公众号文章链接:/s/xxx、/s?__biz=...、/mp/appmsg...
  var ARTICLE_HREF = /^https?:\/\/mp\.weixin\.qq\.com\/(s(\/|\?|$)|mp\/appmsg)/i;
  // 搜狗微信的跳转链接(结果页里的 <a> 指向 /link?url=...,抓取时由本地程序还原)。
  var SOGOU_HREF = /^https?:\/\/weixin\.sogou\.com\/link\?/i;
  // 列表里常见的发布日期格式:2026-10-20 / 2026/10/20 / 2026年10月20日
  var DATE_RE = /(20\d{2})[-\/年.](\d{1,2})[-\/月.](\d{1,2})/;

  // isPickURL 判断链接是否是可归档的公众号文章(含搜狗跳转链接)。
  function isPickURL(u){
    return !!u && (ARTICLE_HREF.test(u) || SOGOU_HREF.test(u));
  }

  function css(el, styles){
    for (var k in styles) { el.style[k] = styles[k]; }
  }

  function mk(tag, id, styles){
    var el = document.createElement(tag);
    if (id) { el.id = id; }
    if (styles) { css(el, styles); }
    return el;
  }

  function stop(ev){
    ev.preventDefault();
    ev.stopPropagation();
    if (ev.stopImmediatePropagation) { ev.stopImmediatePropagation(); }
  }

  // linksCount 统计页面上识别到的公众号文章链接数,用于诊断搜索页是否被识别。
  function linksCount(){
    var anchors = document.querySelectorAll('a[href]');
    var n = 0;
    for (var i = 0; i < anchors.length; i++) {
      var u = absURL(anchors[i].getAttribute('href'));
      if (isPickURL(u)) { n++; }
    }
    return n;
  }

  // currentBiz 返回当前页 URL 里的 __biz(公众号唯一标识)。
  function currentBiz(){
    var m = /[?&]__biz=([^&#]+)/.exec(location.href);
    return m ? decodeURIComponent(m[1]) : '';
  }

  // currentNick 返回文章页 cgiDataNew 里的公众号昵称。
  function currentNick(){
    try {
      var c = window.cgiDataNew;
      return (c && c.nick_name) || '';
    } catch (e) {
      return '';
    }
  }

  // sendDiag 把当前页面的注入状态上报到本地程序,便于排查"按钮没出现"。
  function sendDiag(stage, extra){
    try {
      var bar = document.getElementById(BAR_ID);
      var rect = null;
      var hit = null;
      var style = null;
      if (bar && bar.getBoundingClientRect) {
        var b = bar.getBoundingClientRect();
        rect = [Math.round(b.left), Math.round(b.top), Math.round(b.width), Math.round(b.height)];
        var cs = window.getComputedStyle(bar);
        style = [cs.position, cs.display, cs.visibility, cs.opacity, cs.zIndex].join('|');
        if (b.width > 0 && b.height > 0) {
          var e = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
          hit = e ? (e.id || e.nodeName) : null;
        }
      }
      var data = {
        stage: stage,
        href: String(location.href).slice(0, 300),
        ready: document.readyState,
        hasBody: !!document.body,
        hasBar: !!bar,
        rect: rect,
        style: style,
        hit: hit,
        parent: bar && bar.parentNode ? (bar.parentNode.nodeName || '') : '',
        bodyChildren: document.body ? document.body.children.length : -1,
        htmlChildren: document.documentElement ? document.documentElement.children.length : -1,
        cgi: !!window.cgiDataNew,
        biz: currentBiz(),
        nick: currentNick(),
        links: linksCount(),
        picks: pick && pick.on ? pick.entries.length : 0,
        selected: pick && pick.on ? pick.order.length : 0,
        viewport: [window.innerWidth, window.innerHeight],
        extra: extra || null
      };
      fetch(admin + '/api/diag', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(data)
      }).catch(function(){});
    } catch (e) {}
  }

  // ---------------- 通用请求/下载 ----------------

  function readJSONError(r){
    return r.json().then(function(res){
      throw new Error((res && res.error) || '请求失败');
    });
  }

  // triggerDownload 用 blob URL 触发浏览器下载(微信 PC 内置浏览器为 Chromium)。
  function triggerDownload(blob, name){
    // 测试钩子:自动化测试里只验证"拿到 ZIP",不真的落盘。
    if (window.__wxmpSkipDownload) { return; }
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = name;
    a.rel = 'noopener';
    a.style.display = 'none';
    document.body.appendChild(a);
    a.click();
    setTimeout(function(){
      if (a.parentNode) { a.parentNode.removeChild(a); }
      URL.revokeObjectURL(url);
    }, 10000);
  }

  // ---------------- 文章页:单篇同步 / 下载 ----------------

  function payload(){
    var p = {
      url: location.href,
      title: document.title,
      nickname: '',
      cgiDataNew: null
    };
    try {
      if (window.cgiDataNew) {
        p.cgiDataNew = window.cgiDataNew;
        p.title = window.cgiDataNew.title || document.title;
        p.nickname = window.cgiDataNew.nick_name || '';
      }
    } catch (e) {}
    return p;
  }

  function articleName(){
    var t = '';
    try {
      t = (window.cgiDataNew && window.cgiDataNew.title) || document.title || '';
    } catch (e) {}
    t = String(t).replace(/[\\/:*?"<>|\r\n\t]/g, '_').replace(/^\s+|\s+$/g, '').slice(0, 80);
    return t || 'wechat-article';
  }

  function makeButton(id, color, idleText){
    var el = mk('div', id, {
      display:'flex', alignItems:'center', gap:'6px',
      padding:'8px 14px', borderRadius:'20px', background:color,
      color:'#fff', fontSize:'14px', lineHeight:'1', cursor:'pointer',
      boxShadow:'0 2px 8px rgba(0,0,0,.25)', fontFamily:FONT,
      userSelect:'none', WebkitUserSelect:'none', whiteSpace:'nowrap'
    });
    el.textContent = idleText;
    var timer = null;
    function idle(){
      el.style.pointerEvents = '';
      el.style.background = color;
      el.textContent = idleText;
    }
    return {
      el: el,
      start: function(text){
        clearTimeout(timer);
        el.style.pointerEvents = 'none';
        el.style.background = '#8a8a8a';
        el.textContent = text;
      },
      done: function(text){
        clearTimeout(timer);
        el.style.pointerEvents = '';
        el.style.background = '#07c160';
        el.textContent = text;
        timer = setTimeout(idle, 3500);
      },
      fail: function(text){
        clearTimeout(timer);
        el.style.pointerEvents = '';
        el.style.background = '#e54d42';
        el.textContent = text;
        timer = setTimeout(idle, 4000);
      }
    };
  }

  function bindSync(btn){
    btn.el.addEventListener('click', function(){
      btn.start('同步中…');
      fetch(admin + '/api/save', {
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body: JSON.stringify(payload())
      }).then(function(r){ return r.json(); }).then(function(res){
        if (res && res.ok) {
          var base = res.duplicate ? '✓ 已存在' : '✓ 已保存';
          if (res.bot && res.bot.ok) { base += '·已推送'; }
          else if (res.bot && !res.bot.ok) { base += '·推送失败'; }
          if (res.naicha && res.naicha.ok) { base += '·已发帖'; }
          else if (res.naicha && !res.naicha.ok) { base += '·发帖失败'; }
          btn.done(base);
        } else {
          btn.fail('✗ ' + ((res && res.error) || '保存失败'));
        }
      }).catch(function(){
        btn.fail('✗ 本地程序未运行');
      });
    });
  }

  function bindNaicha(btn){
    btn.el.addEventListener('click', function(){
      btn.start('发帖中…');
      fetch(admin + '/api/save?naicha=1', {
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body: JSON.stringify(payload())
      }).then(function(r){ return r.json(); }).then(function(res){
        if (res && res.ok && res.naicha && res.naicha.ok) {
          btn.done(res.naicha.duplicate ? '✓ 已发过' : '✓ 已发帖');
          return;
        }
        var msg = (res && res.naicha && res.naicha.error) || (res && res.error) || '发帖失败';
        msg = String(msg);
        if (msg.length > 14) { msg = msg.slice(0, 14) + '…'; }
        btn.fail('✗ ' + msg);
      }).catch(function(){
        btn.fail('✗ 本地程序未运行');
      });
    });
  }

  function bindDownload(btn){
    btn.el.addEventListener('click', function(){
      btn.start('打包中…');
      fetch(admin + '/api/download', {
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body: JSON.stringify(payload())
      }).then(function(r){
        var ct = (r.headers.get('Content-Type') || '').toLowerCase();
        if (ct.indexOf('application/json') !== -1) {
          return readJSONError(r);
        }
        if (!r.ok) { throw new Error('HTTP ' + r.status); }
        return r.blob();
      }).then(function(blob){
        if (!blob || !blob.size) { throw new Error('压缩包为空'); }
        triggerDownload(blob, articleName() + '.zip');
        btn.done('✓ 已下载');
      }).catch(function(err){
        btn.fail('✗ ' + ((err && err.message) || '下载失败'));
      });
    });
  }

  function mountArticleBar(){
    if (!document.documentElement) { return; }
    if (document.getElementById(BAR_ID)) { return; }

    var bar = mk('div', BAR_ID, {
      position:'fixed', right:'14px', bottom:'120px', zIndex:'2147483647',
      display:'flex', flexDirection:'column', alignItems:'flex-end', gap:'8px',
      pointerEvents:'auto'
    });

    var dl = makeButton('wx-mp-dl-btn', '#1989fa', '⬇ 下载');
    var sync = makeButton('wx-mp-sync-btn', '#07c160', '⬇ 同步');
    bindDownload(dl);
    bindSync(sync);
    bar.appendChild(dl.el);
    bar.appendChild(sync.el);

    // 只有管理服务启用了奶茶圈同步时才显示「发帖」按钮。
    fetch(admin + '/api/status').then(function(r){ return r.json(); }).then(function(res){
      if (!res || !res.ok || !res.status || !res.status.naicha_api) { return; }
      if (document.getElementById(BAR_ID) !== bar) { return; }
      if (document.getElementById('wx-mp-naicha-btn')) { return; }
      var post = makeButton('wx-mp-naicha-btn', '#b26a3c', '⬆ 发帖');
      bindNaicha(post);
      bar.appendChild(post.el);
    }).catch(function(){});

    // 挂到 <html> 下而不是 <body>:新版公众号页面是单页应用,
    // 启动时会重绘 body,挂在 body 里的节点会被清掉。
    try {
      document.documentElement.appendChild(bar);
    } catch (e) {
      if (document.body) { document.body.appendChild(bar); }
    }
    sendDiag('mount');
  }

  // ---------------- 列表/搜索页:多选 + 批量下载 ----------------

  var pick = {
    on: false,
    busy: false,
    dismissed: false,
    entries: [],
    selected: {},
    order: [],
    jobId: '',
    timer: null,
    retries: 0
  };

  function cleanText(s){
    return String(s || '').replace(/\s+/g, ' ').replace(/^\s+|\s+$/g, '');
  }

  function absURL(href){
    if (!href) { return ''; }
    var h = String(href).replace(/^\s+|\s+$/g, '');
    if (!h || h.charAt(0) === '#') { return ''; }
    var low = h.toLowerCase();
    if (low.indexOf('javascript:') === 0 || low.indexOf('data:') === 0) { return ''; }
    try {
      return new URL(h, location.href).href;
    } catch (e) {
      return '';
    }
  }

  function visibleNode(node){
    if (!node || !node.getBoundingClientRect) { return false; }
    var r = node.getBoundingClientRect();
    return r.width > 4 && r.height > 4;
  }

  function countEntriesIn(node, entries){
    var n = 0;
    for (var i = 0; i < entries.length; i++) {
      if (node.contains(entries[i].anchor)) { n++; }
    }
    return n;
  }

  // itemFor 从链接向上找"只包含这一个结果"的最大容器,作为结果卡片。
  function itemFor(anchor, entries){
    var cur = anchor;
    var last = anchor;
    for (var depth = 0; depth < 8; depth++) {
      var p = cur.parentNode;
      if (!p || p.nodeType !== 1) { break; }
      if (p === document.body || p === document.documentElement) { break; }
      if (countEntriesIn(p, entries) > 1) { break; }
      last = p;
      cur = p;
    }
    return last;
  }

  // scanEntries 扫描页面上所有公众号文章链接(含搜狗跳转链接),返回去重后的结果列表。
  // 同一篇文章常同时有"封面链接"和"标题链接"两个 <a>,这里优先保留 <a>,
  // 并保留文字最多的那个,这样结果卡片会被识别成一整块,点卡片任意位置都能选中。
  // 另一些列表(如公众号历史消息)把链接放在 data-link/data-url 上,
  // 没有真实 href,这里也一并识别。
  function scanEntries(){
    var entries = [];
    var index = {};
    var i, j, u;

    function add(node, url, isAnchor){
      if (!url || !isPickURL(url) || !visibleNode(node)) { return; }
      if (index.hasOwnProperty(url)) {
        var exist = entries[index[url]];
        if (isAnchor && !exist.isAnchor) {
          exist.anchor = node;
          exist.isAnchor = true;
        } else if (isAnchor === exist.isAnchor &&
                   cleanText(node.textContent).length > cleanText(exist.anchor.textContent).length) {
          exist.anchor = node;
        }
        return;
      }
      // 结果很多(无限滚动)时限制扫描量:选中上限只有 100 篇。
      if (entries.length >= 300) { return; }
      index[url] = entries.length;
      entries.push({ url: url, anchor: node, item: node, title: '', date: '', isAnchor: !!isAnchor });
    }

    var anchors = document.querySelectorAll('a[href]');
    for (i = 0; i < anchors.length; i++) {
      add(anchors[i], absURL(anchors[i].getAttribute('href')), true);
    }
    var datas = document.querySelectorAll('[data-link],[data-url],[data-href]');
    for (i = 0; i < datas.length; i++) {
      var attrs = ['data-link', 'data-url', 'data-href'];
      for (j = 0; j < attrs.length; j++) {
        u = absURL(datas[i].getAttribute(attrs[j]));
        if (u) { add(datas[i], u, false); }
      }
    }
    for (i = 0; i < entries.length; i++) {
      entries[i].item = itemFor(entries[i].anchor, entries);
    }
    for (i = 0; i < entries.length; i++) {
      var t = cleanText(entries[i].anchor.getAttribute('title'));
      if (!t) {
        var head = entries[i].anchor.querySelector && entries[i].anchor.querySelector('h1,h2,h3,h4');
        if (head) { t = cleanText(head.textContent); }
      }
      if (!t) { t = cleanText(entries[i].anchor.textContent); }
      if (!t) { t = cleanText(entries[i].item.textContent); }
      entries[i].title = t.slice(0, 120);
      entries[i].date = findDate(entries[i].item) || findDate(entries[i].anchor);
      entries[i].isAnchor = undefined;
    }
    return entries;
  }

  // findDate 从结果卡片文字里找发布日期(有则用于"选最新 N 篇"和排序提示)。
  function findDate(node){
    if (!node) { return ''; }
    var m = DATE_RE.exec(cleanText(node.textContent).slice(0, 800));
    if (!m) { return ''; }
    var mo = m[2].length < 2 ? '0' + m[2] : m[2];
    var day = m[3].length < 2 ? '0' + m[3] : m[3];
    return m[1] + '-' + mo + '-' + day;
  }

  function isArticlePage(){
    try {
      var c = window.cgiDataNew;
      if (c && (c.title || c.content_noencode || c.nick_name)) { return true; }
    } catch (e) {}
    if (document.getElementById('js_content')) { return true; }
    var p = String(location.pathname || '');
    return p === '/s' || p.indexOf('/s/') === 0 || p.indexOf('/mp/appmsg') === 0;
  }

  function paint(entry, on){
    var node = entry.item || entry.anchor;
    if (!node || !node.style) { return; }
    if (on) {
      if (node.__wxmpOrig) { return; }
      node.__wxmpOrig = {
        outline: node.style.outline,
        offset: node.style.outlineOffset,
        radius: node.style.borderRadius,
        bg: node.style.backgroundColor
      };
      node.style.outline = '2px solid #07c160';
      node.style.outlineOffset = '-2px';
      node.style.borderRadius = '10px';
      node.style.backgroundColor = 'rgba(7,193,96,0.10)';
    } else if (node.__wxmpOrig) {
      node.style.outline = node.__wxmpOrig.outline || '';
      node.style.outlineOffset = node.__wxmpOrig.offset || '';
      node.style.borderRadius = node.__wxmpOrig.radius || '';
      node.style.backgroundColor = node.__wxmpOrig.bg || '';
      node.__wxmpOrig = null;
    }
  }

  function inOrder(url){
    for (var i = 0; i < pick.order.length; i++) {
      if (pick.order[i] === url) { return i; }
    }
    return -1;
  }

  // selectEntry 选中一个结果(已选中或超过上限时忽略)。
  function selectEntry(entry){
    if (!entry || pick.selected[entry.url] || pick.order.length >= MAX_PICK) { return; }
    pick.selected[entry.url] = { url: entry.url, title: entry.title };
    pick.order.push(entry.url);
    paint(entry, true);
  }

  // clearSelection 取消全部选择。
  function clearSelection(){
    for (var i = 0; i < pick.entries.length; i++) { paint(pick.entries[i], false); }
    pick.selected = {};
    pick.order = [];
  }

  function toggle(entry){
    if (!entry || pick.busy) { return; }
    var u = entry.url;
    if (pick.selected[u]) {
      delete pick.selected[u];
      var idx = inOrder(u);
      if (idx >= 0) { pick.order.splice(idx, 1); }
      paint(entry, false);
    } else {
      if (pick.order.length >= MAX_PICK) {
        setPickStatus('最多选择 ' + MAX_PICK + ' 篇');
        return;
      }
      selectEntry(entry);
    }
    updatePickBar();
  }

  // selectNewest 按卡片上显示的发布日期,选中最新 n 篇(搜狗结果里很有用)。
  function selectNewest(n){
    if (pick.busy) { return; }
    var list = [];
    var i;
    for (i = 0; i < pick.entries.length; i++) {
      if (pick.entries[i].date) { list.push(pick.entries[i]); }
    }
    if (list.length === 0) {
      setPickStatus('当前页面没有识别到发布日期,请手动勾选');
      return;
    }
    list.sort(function(a, b){
      if (a.date === b.date) { return 0; }
      return a.date < b.date ? 1 : -1;
    });
    clearSelection();
    for (i = 0; i < list.length && i < n; i++) { selectEntry(list[i]); }
    updatePickBar();
    setPickStatus('已按发布时间选中最新 ' + Math.min(n, list.length) + ' 篇(最新 ' + list[0].date + ')');
  }

  // dateRange 返回页面上识别到的日期范围文案。
  function dateRange(){
    var min = '';
    var max = '';
    for (var i = 0; i < pick.entries.length; i++) {
      var d = pick.entries[i].date;
      if (!d) { continue; }
      if (!min || d < min) { min = d; }
      if (!max || d > max) { max = d; }
    }
    return min ? ('最新 ' + max + ' / 最旧 ' + min) : '';
  }

  // hasSogouEntry 判断结果里是否有搜狗跳转链接。
  function hasSogouEntry(){
    for (var i = 0; i < pick.entries.length; i++) {
      if (SOGOU_HREF.test(pick.entries[i].url)) { return true; }
    }
    return false;
  }

  // entryFromNode 由点击目标反查是哪一个结果卡片(先看链接,再看卡片容器)。
  function entryFromNode(target){
    if (!target || target.nodeType !== 1) { return null; }
    var node, depth, i, e;
    for (node = target, depth = 0; node && depth < 12; node = node.parentNode, depth++) {
      for (i = 0; i < pick.entries.length; i++) {
        e = pick.entries[i];
        if (e.anchor === node || (e.anchor.contains && e.anchor.contains(node))) { return e; }
      }
    }
    for (node = target, depth = 0; node && depth < 12; node = node.parentNode, depth++) {
      for (i = 0; i < pick.entries.length; i++) {
        e = pick.entries[i];
        if (e.item === node || (e.item.contains && e.item.contains(node))) { return e; }
      }
    }
    return null;
  }

  function onPickClick(ev){
    if (!pick.on) { return; }
    var entry = entryFromNode(ev.target);
    if (!entry) { return; }
    stop(ev);
    if (ev.type === 'click') { toggle(entry); }
  }

  function setPickStatus(s){
    var status = document.getElementById('wx-mp-pick-status');
    if (status) { status.textContent = s || ''; }
  }

  function smallBtn(id, label, primary, onClick){
    var b = mk('span', id, {
      padding:'6px 10px', borderRadius:'6px', cursor:'pointer',
      background: primary ? '#07c160' : '#3a3a3a', color:'#fff',
      fontSize:'12px', lineHeight:'1.2', whiteSpace:'nowrap',
      userSelect:'none', WebkitUserSelect:'none'
    });
    b.textContent = label;
    b.addEventListener('click', function(ev){ stop(ev); onClick(); });
    return b;
  }

  function mountPickBar(){
    if (document.getElementById(PICK_BAR_ID)) { return; }
    var bar = mk('div', PICK_BAR_ID, {
      position:'fixed', left:'50%', bottom:'18px', transform:'translateX(-50%)',
      zIndex:'2147483647', display:'flex', flexDirection:'column', gap:'6px',
      background:'rgba(31,31,31,.96)', color:'#fff', padding:'10px 12px',
      borderRadius:'10px', boxShadow:'0 6px 20px rgba(0,0,0,.3)',
      fontFamily:FONT, fontSize:'13px', maxWidth:'92vw'
    });
    // 工具条上的事件不能被页面抢走(部分页面在 document 上有代理监听)。
    // 注意用冒泡阶段:捕获阶段 stopPropagation 会把自己按钮的点击也拦掉。
    bar.addEventListener('mousedown', function(ev){ ev.stopPropagation(); });
    bar.addEventListener('click', function(ev){ ev.stopPropagation(); });

    var row = mk('div', '', { display:'flex', alignItems:'center', gap:'8px', flexWrap:'wrap' });
    row.appendChild(mk('span', 'wx-mp-pick-count', { fontWeight:'600', whiteSpace:'nowrap' }));

    var all = smallBtn('wx-mp-pick-all', '全选', false, function(){
      if (pick.busy) { return; }
      for (var i = 0; i < pick.entries.length; i++) { selectEntry(pick.entries[i]); }
      updatePickBar();
      setPickStatus('已选中 ' + pick.order.length + ' 篇');
    });
    var none = smallBtn('wx-mp-pick-none', '清空', false, function(){
      if (pick.busy) { return; }
      clearSelection();
      updatePickBar();
      setPickStatus('已清空选择');
    });
    var newest = smallBtn('wx-mp-pick-newest', '选最新10篇', false, function(){
      selectNewest(10);
    });
    newest.style.display = 'none';
    var dl = smallBtn('wx-mp-pick-dl', '⬇ 批量下载', true, startBatch);
    var quit = smallBtn('wx-mp-pick-exit', '退出多选', false, exitPick);

    row.appendChild(all);
    row.appendChild(none);
    row.appendChild(newest);
    row.appendChild(dl);
    row.appendChild(quit);
    bar.appendChild(row);
    bar.appendChild(mk('div', 'wx-mp-pick-status', {
      fontSize:'12px', color:'#b9b9b9', maxWidth:'560px',
      whiteSpace:'nowrap', overflow:'hidden', textOverflow:'ellipsis'
    }));

    try {
      document.documentElement.appendChild(bar);
    } catch (e) {
      if (document.body) { document.body.appendChild(bar); }
    }
  }

  function updatePickBar(){
    var count = document.getElementById('wx-mp-pick-count');
    if (!count) { return; }
    count.textContent = '已选 ' + pick.order.length + ' / 共 ' + pick.entries.length + ' 篇';
    var dl = document.getElementById('wx-mp-pick-dl');
    if (dl) {
      var enabled = pick.order.length > 0 && !pick.busy;
      dl.style.opacity = enabled ? '1' : '.45';
      dl.style.pointerEvents = enabled ? 'auto' : 'none';
    }
    var newest = document.getElementById('wx-mp-pick-newest');
    if (newest) {
      var hasDate = false;
      for (var k = 0; k < pick.entries.length; k++) {
        if (pick.entries[k].date) { hasDate = true; break; }
      }
      newest.style.display = hasDate ? '' : 'none';
      newest.style.opacity = pick.busy ? '.45' : '1';
      newest.style.pointerEvents = pick.busy ? 'none' : 'auto';
    }
    var ids = ['wx-mp-pick-all', 'wx-mp-pick-none', 'wx-mp-pick-exit'];
    for (var i = 0; i < ids.length; i++) {
      var b = document.getElementById(ids[i]);
      if (b) {
        b.style.opacity = pick.busy ? '.45' : '1';
        b.style.pointerEvents = pick.busy ? 'none' : 'auto';
      }
    }
  }

  function mountLauncher(){
    if (document.getElementById(PICK_LAUNCH_ID)) { return; }
    var launcher = mk('div', PICK_LAUNCH_ID, {
      position:'fixed', right:'14px', bottom:'120px', zIndex:'2147483646',
      padding:'8px 14px', borderRadius:'20px', background:'#07c160', color:'#fff',
      fontSize:'14px', lineHeight:'1', cursor:'pointer', boxShadow:'0 2px 8px rgba(0,0,0,.25)',
      fontFamily:FONT, userSelect:'none', WebkitUserSelect:'none', whiteSpace:'nowrap'
    });
    launcher.textContent = '☑ 多选';
    launcher.addEventListener('click', function(ev){
      stop(ev);
      enterPick();
    });
    try {
      document.documentElement.appendChild(launcher);
    } catch (e) {
      if (document.body) { document.body.appendChild(launcher); }
    }
  }

  function hideLauncher(){
    var el = document.getElementById(PICK_LAUNCH_ID);
    if (el && el.parentNode) { el.parentNode.removeChild(el); }
  }

  function enterPick(entries){
    pick.on = true;
    pick.dismissed = false;
    pick.entries = entries || scanEntries();
    for (var i = 0; i < pick.entries.length; i++) {
      if (pick.selected[pick.entries[i].url]) { paint(pick.entries[i], true); }
    }
    hideLauncher();
    mountPickBar();
    updatePickBar();
    var hint = '点击文章卡片选中/取消;选好后点「批量下载」,会逐篇抓取并打包成 ZIP。';
    var range = dateRange();
    if (range) { hint += ' 结果时间:' + range + '。'; }
    if (hasSogouEntry()) { hint += ' 搜狗结果可能不是最新,抓取时会自动还原真实文章链接。'; }
    setPickStatus(hint);
    sendDiag('pick-enter', { entries: pick.entries.length });
  }

  function exitPick(){
    if (pick.busy) {
      setPickStatus('正在下载,完成后才能退出');
      return;
    }
    pick.on = false;
    pick.dismissed = true;
    if (pick.timer) { clearTimeout(pick.timer); pick.timer = null; }
    for (var i = 0; i < pick.entries.length; i++) { paint(pick.entries[i], false); }
    pick.selected = {};
    pick.order = [];
    pick.jobId = '';
    var bar = document.getElementById(PICK_BAR_ID);
    if (bar && bar.parentNode) { bar.parentNode.removeChild(bar); }
    try { sessionStorage.setItem('wx-mp-pick-off', location.pathname); } catch (e) {}
    refresh();
  }

  function firstError(job){
    if (!job || !job.items) { return ''; }
    for (var i = 0; i < job.items.length; i++) {
      if (job.items[i].state === 'failed' && job.items[i].error) { return job.items[i].error; }
    }
    return '';
  }

  function pollJob(){
    if (!pick.on || !pick.jobId) { return; }
    var id = pick.jobId;
    fetch(admin + '/api/batch/status?id=' + encodeURIComponent(id))
      .then(function(r){ return r.json(); })
      .then(function(res){
        if (!res || !res.ok || !res.job) { throw new Error((res && res.error) || '任务丢失'); }
        pick.retries = 0;
        var job = res.job;
        var doneOK = job.ok + job.skipped;
        setPickStatus('下载中 ' + job.done + '/' + job.total + ' · 成功 ' + doneOK +
          (job.failed ? ' · 失败 ' + job.failed : ''));
        if (!job.finished) {
          pick.timer = setTimeout(pollJob, 800);
          return;
        }
        pick.busy = false;
        updatePickBar();
        if (doneOK > 0) {
          downloadBatchZip(job, doneOK);
        } else {
          setPickStatus('✗ 下载失败:' + (firstError(job) || '没有成功归档的文章'));
        }
      })
      .catch(function(err){
        pick.retries = (pick.retries || 0) + 1;
        if (pick.retries > 5) {
          pick.busy = false;
          updatePickBar();
          setPickStatus('✗ 查询进度失败:' + ((err && err.message) || '未知错误'));
          return;
        }
        pick.timer = setTimeout(pollJob, 1200);
        setPickStatus('查询进度失败,正在重试…');
      });
  }

  function downloadBatchZip(job, doneOK){
    setPickStatus('✓ 成功 ' + job.ok + ' 篇' + (job.skipped ? ' · 已存在 ' + job.skipped + ' 篇' : '') +
      (job.failed ? ' · 失败 ' + job.failed + ' 篇' : '') + ',正在打包…');
    fetch(admin + '/api/batch/zip?id=' + encodeURIComponent(job.id))
      .then(function(r){
        var ct = (r.headers.get('Content-Type') || '').toLowerCase();
        if (ct.indexOf('application/json') !== -1) { return readJSONError(r); }
        if (!r.ok) { throw new Error('HTTP ' + r.status); }
        var cd = r.headers.get('Content-Disposition') || '';
        var m = /filename\*=UTF-8''([^;]+)/i.exec(cd);
        var name = 'wx-mp-batch.zip';
        if (m) { try { name = decodeURIComponent(m[1]); } catch (e) {} }
        return r.blob().then(function(blob){ return { blob: blob, name: name }; });
      })
      .then(function(out){
        if (!out || !out.blob || !out.blob.size) { throw new Error('压缩包为空'); }
        triggerDownload(out.blob, out.name);
        var msg = '✓ 已下载 ' + doneOK + ' 篇' + (job.failed ? ',失败 ' + job.failed + ' 篇' : '');
        var errText = job.failed ? firstError(job) : '';
        if (errText) { msg += '(' + String(errText).slice(0, 80) + ')'; }
        setPickStatus(msg);
        updatePickBar();
      })
      .catch(function(err){
        setPickStatus('✗ 打包失败:' + ((err && err.message) || '未知错误'));
        updatePickBar();
      });
  }

  // resolveSogouLink 在页面内(同源、带搜狗会话 Cookie)请求 /link 跳转页,
  // 还原真实文章链接。搜狗会校验发起搜索时的会话,所以必须在页面里解析,
  // 不能交给本地程序(那会拿到验证码页)。
  function resolveSogouLink(url){
    // 测试钩子:允许注入一个替代实现,便于自动化测试批量流程。
    if (typeof window.__wxmpResolveSogou === 'function') {
      return window.__wxmpResolveSogou(url);
    }
    return fetch(url, { credentials: 'include' }).then(function(r){
      if (!r.ok) { throw new Error('HTTP ' + r.status); }
      return r.text();
    }).then(function(html){
      var parts = [];
      var re = /url\s*\+=\s*'([^']*)'/g;
      var m;
      while ((m = re.exec(html))) { parts.push(m[1]); }
      var real = parts.join('').replace(/@/g, '');
      real = real.replace(/&amp;/g, '&');
      if (!real) {
        var m2 = /window\.location\.replace\(\s*["']([^"']+)["']\s*\)/.exec(html);
        if (m2) { real = m2[1].replace(/\\x26/g, '&'); }
      }
      if (!real || real.indexOf('http') !== 0) { throw new Error('未能解析搜狗跳转链接'); }
      return real;
    });
  }

  // resolveSogouItems 逐条还原选中的搜狗链接(限速,避免触发搜狗验证码)。
  function resolveSogouItems(items){
    var out = [];
    var failed = 0;
    var i = 0;
    function next(){
      if (i >= items.length) { return Promise.resolve({ items: out, failed: failed }); }
      var it = items[i];
      i++;
      if (!SOGOU_HREF.test(it.url)) {
        out.push(it);
        return next();
      }
      setPickStatus('还原搜狗链接 ' + i + '/' + items.length + '…');
      return resolveSogouLink(it.url).then(function(real){
        out.push({ url: real, title: it.title });
      }).catch(function(){
        failed++;
      }).then(function(){
        return new Promise(function(resolve){ setTimeout(resolve, 250); }).then(next);
      });
    }
    return next();
  }

  function startBatch(){
    if (pick.busy || pick.order.length === 0) { return; }
    var items = [];
    for (var i = 0; i < pick.order.length; i++) {
      var u = pick.order[i];
      var s = pick.selected[u];
      items.push({ url: u, title: (s && s.title) || '' });
    }
    pick.busy = true;
    pick.jobId = '';
    pick.retries = 0;
    updatePickBar();
    setPickStatus('准备下载 ' + items.length + ' 篇…');
    resolveSogouItems(items).then(function(res){
      if (res.items.length === 0) { throw new Error('选中的搜狗链接都没有还原成功'); }
      if (res.failed > 0) {
        setPickStatus('已还原 ' + res.items.length + ' 篇(失败 ' + res.failed + ' 篇),创建任务中…');
      } else {
        setPickStatus('创建任务中…');
      }
      return fetch(admin + '/api/batch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ urls: res.items, download: true })
      }).then(function(r){ return r.json(); }).then(function(job){
        if (!job || !job.ok) { throw new Error((job && job.error) || '创建任务失败'); }
        pick.jobId = job.job_id;
        setPickStatus('开始下载 ' + job.total + ' 篇…');
        pollJob();
      });
    }).catch(function(err){
      pick.busy = false;
      updatePickBar();
      var msg = (err && err.message) || '';
      if (!msg || msg === 'Failed to fetch' || msg.indexOf('NetworkError') >= 0 || msg.indexOf('fetch') >= 0) {
        msg = '本地程序未运行,请先启动 wx-mp-sync.exe';
      }
      setPickStatus('✗ ' + msg);
    });
  }

  // ---------------- 刷新与挂载 ----------------

  var refreshTimer = null;
  var lastHref = location.href;

  function refreshPicks(){
    pick.entries = scanEntries();
    for (var i = 0; i < pick.entries.length; i++) {
      if (pick.selected[pick.entries[i].url]) { paint(pick.entries[i], true); }
    }
    updatePickBar();
  }

  function refresh(){
    if (location.href !== lastHref) {
      lastHref = location.href;
      pick.on = false;
      pick.dismissed = false;
      pick.busy = false;
      pick.selected = {};
      pick.order = [];
      pick.jobId = '';
      if (pick.timer) { clearTimeout(pick.timer); pick.timer = null; }
      var stale = document.getElementById(PICK_BAR_ID);
      if (stale && stale.parentNode) { stale.parentNode.removeChild(stale); }
      try {
        pick.dismissed = sessionStorage.getItem('wx-mp-pick-off') === location.pathname;
      } catch (e) {}
    }
    if (pick.on) {
      refreshPicks();
      return;
    }
    if (isArticlePage()) {
      hideLauncher();
      mountArticleBar();
      return;
    }
    var entries = scanEntries();
    if (entries.length >= 2) {
      hideArticleBar();
      if (!pick.dismissed) {
        enterPick(entries);
        return;
      }
      mountLauncher();
      return;
    }
    hideLauncher();
    hideArticleBar();
  }

  function hideArticleBar(){
    var el = document.getElementById(BAR_ID);
    if (el && el.parentNode) { el.parentNode.removeChild(el); }
  }

  function scheduleRefresh(delay){
    if (refreshTimer) { return; }
    refreshTimer = setTimeout(function(){
      refreshTimer = null;
      try { refresh(); } catch (e) {}
    }, delay || 400);
  }

  // watch 在页面重绘/滚动加载后重新挂载按钮与结果选择。
  function watch(){
    try {
      new MutationObserver(function(){
        if (pick.on) { scheduleRefresh(250); }
        else { scheduleRefresh(500); }
      }).observe(document.documentElement, { childList: true, subtree: true });
    } catch (e) {}
    setInterval(function(){
      if (pick.on) {
        if (!document.getElementById(PICK_BAR_ID)) { mountPickBar(); }
      } else if (isArticlePage()) {
        if (!document.getElementById(BAR_ID)) { mountArticleBar(); }
      }
      try { refresh(); } catch (e) {}
    }, 2000);
  }

  // 多选模式下拦截结果卡片的点击(含 mousedown,防止页面自己打开文章)。
  document.addEventListener('mousedown', onPickClick, true);
  document.addEventListener('mouseup', function(ev){
    if (!pick.on) { return; }
    if (entryFromNode(ev.target)) { stop(ev); }
  }, true);
  document.addEventListener('click', onPickClick, true);

  function boot(){
    refresh();
    watch();
    setTimeout(function(){ sendDiag('t1'); }, 1000);
    setTimeout(function(){ sendDiag('t3'); }, 3000);
    setTimeout(function(){ sendDiag('t8'); }, 8000);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
`

	return `<script` + nonceAttr + `>` + script + `</script>`
}
