package proxy

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"text/template"
)

// chromePath 查找本机可用的 Chromium 内核浏览器,找不到时返回空串。
func chromePath() string {
	candidates := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("chrome"); err == nil {
		return p
	}
	return ""
}

// dumpDOM 用无头浏览器打开 file:// 页面并返回渲染后的 DOM。
// 通过 --virtual-time-budget 让页面里的 setTimeout 断言有机会跑完。
func dumpDOM(t *testing.T, browser, pagePath string, budgetMS int) string {
	t.Helper()
	abs, err := filepath.Abs(pagePath)
	if err != nil {
		t.Fatalf("取绝对路径失败: %v", err)
	}
	pageURL := "file:///" + strings.ReplaceAll(filepath.ToSlash(abs), " ", "%20")
	cmd := exec.Command(browser,
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		"--allow-file-access-from-files",
		"--hide-scrollbars",
		"--window-size=1000,900",
		"--virtual-time-budget="+itoa(budgetMS),
		"--dump-dom",
		pageURL,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("无头浏览器执行失败: %v\n%s", err, truncate(string(out), 2000))
	}
	return string(out)
}

// itoa 避免额外 import strconv 时的心智负担。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// truncate 截断过长的日志。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// pickerPage 生成一个模拟微信搜索结果页的测试页面。
const pickerPage = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>搜一搜</title></head>
<body>
<h1>搜索结果</h1>
<div id="list">
  <div class="card" id="card-1"><a href="https://mp.weixin.qq.com/s/aaa"><h3>文章一</h3><p>摘要一</p></a></div>
  <div class="card" id="card-dup">
    <a href="https://mp.weixin.qq.com/s/dup"><img src="data:," width="48" height="48" alt="封面"></a>
    <a href="https://mp.weixin.qq.com/s/dup"><h3>文章二</h3><p>摘要二</p></a>
  </div>
  <div class="card" id="card-3"><a href="https://mp.weixin.qq.com/s?__biz=MzA5&mid=1&idx=1"><h3>文章三</h3><p>摘要三</p></a></div>
</div>
<pre id="result">PENDING</pre>
<script>` + `{{.Script}}` + `</script>
<script>
var out = [];
function check(name, cond){ out.push((cond ? 'PASS ' : 'FAIL ') + name); }
function countText(){ var n = document.getElementById('wx-mp-pick-count'); return n ? n.textContent : ''; }
function cardOutline(id){ var c = document.getElementById(id); return c ? (c.style.outline || '') : ''; }
setTimeout(function(){
  try {
    check('自动进入多选模式', !!document.getElementById('wx-mp-pick-bar'));
    check('重复链接去重后共 3 篇', countText().indexOf('已选 0 / 共 3 篇') >= 0);
    var cover = document.querySelector('#card-dup a');
    cover.click();
    check('点击封面也能选中整张卡片', countText().indexOf('已选 1') >= 0);
    check('选中后卡片有高亮', cardOutline('card-dup').indexOf('7, 193, 96') >= 0 || cardOutline('card-dup').indexOf('07c160') >= 0);
    check('点击不会跳转', location.href.indexOf('inject-test') >= 0);
    document.querySelector('#card-1 a').click();
    check('不同卡片分别选中', countText().indexOf('已选 2') >= 0);
    document.querySelector('#card-1 a').click();
    check('再次点击取消选中', countText().indexOf('已选 1') >= 0);
    document.getElementById('wx-mp-pick-all').click();
    check('全选', countText().indexOf('已选 3 / 共 3') >= 0);
    document.getElementById('wx-mp-pick-none').click();
    check('清空', countText().indexOf('已选 0 / 共 3') >= 0);
    document.getElementById('wx-mp-pick-exit').click();
    check('退出移除多选工具条', !document.getElementById('wx-mp-pick-bar'));
    check('退出后出现「多选」入口', !!document.getElementById('wx-mp-pick-launch'));
  } catch (e) {
    out.push('ERROR ' + e);
  }
  document.getElementById('result').textContent = out.join('\n');
}, 800);
</script>
</body>
</html>`

// articlePage 生成一个模拟公众号文章页的测试页面。
const articlePage = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>文章标题</title></head>
<body>
<h1 id="activity-name">文章标题</h1>
<div id="js_content"><p>正文</p></div>
<pre id="result">PENDING</pre>
<script>
window.cgiDataNew = { title: '文章标题', nick_name: '测试号', content_noencode: '<p>正文</p>', create_time: '1760000000' };
</script>
<script>` + `{{.Script}}` + `</script>
<script>
var out = [];
function check(name, cond){ out.push((cond ? 'PASS ' : 'FAIL ') + name); }
setTimeout(function(){
  check('文章页出现下载按钮', !!document.getElementById('wx-mp-dl-btn'));
  check('文章页出现同步按钮', !!document.getElementById('wx-mp-sync-btn'));
  check('文章页不进入多选模式', !document.getElementById('wx-mp-pick-bar'));
  document.getElementById('result').textContent = out.join('\n');
}, 600);
</script>
</body>
</html>`

// sogouPage 模拟搜狗微信搜索结果页:链接是 /link 跳转,卡片上带发布日期。
const sogouPage = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>机场通道 – 搜狗微信搜索</title></head>
<body>
<h1>搜狗微信搜索</h1>
<div id="list">
  <div class="card" id="s1"><a href="https://weixin.sogou.com/link?url=aaa"><h3>2016 年的文章</h3></a><span>2016-11-06</span></div>
  <div class="card" id="s2"><a href="https://weixin.sogou.com/link?url=bbb"><h3>2023 年的文章</h3></a><span>2023-09-21</span></div>
  <div class="card" id="s3"><a href="https://weixin.sogou.com/link?url=ccc"><h3>最新的文章</h3></a><span>2026-10-20</span></div>
</div>
<pre id="result">PENDING</pre>
<script>` + `{{.Script}}` + `</script>
<script>
var out = [];
function check(name, cond){ out.push((cond ? 'PASS ' : 'FAIL ') + name); }
function countText(){ var n = document.getElementById('wx-mp-pick-count'); return n ? n.textContent : ''; }
function statusText(){ var n = document.getElementById('wx-mp-pick-status'); return n ? n.textContent : ''; }
function outline(id){ var c = document.getElementById(id); return c ? (c.style.outline || '') : ''; }
setTimeout(function(){
  try {
    check('搜狗页自动进入多选', !!document.getElementById('wx-mp-pick-bar'));
    check('识别到 3 条搜狗结果', countText().indexOf('已选 0 / 共 3 篇') >= 0);
    check('提示结果时间', statusText().indexOf('最新 2026-10-20') >= 0);
    check('提示搜狗结果可能较旧', statusText().indexOf('搜狗') >= 0);
    var newest = document.getElementById('wx-mp-pick-newest');
    check('显示「选最新10篇」', !!newest && newest.style.display !== 'none');
    newest.click();
    check('选最新选中全部 3 篇', countText().indexOf('已选 3 / 共 3') >= 0);
    check('最新一篇被高亮', (outline('s3').indexOf('7, 193, 96') >= 0) || (outline('s3').indexOf('07c160') >= 0));
    document.querySelector('#s3 a').click();
    check('点击已选卡片可取消', countText().indexOf('已选 2') >= 0);
    check('点击搜狗结果不跳转', location.href.indexOf('inject-test') >= 0);
  } catch (e) {
    out.push('ERROR ' + e);
  }
  document.getElementById('result').textContent = out.join('\n');
}, 800);
</script>
</body>
</html>`

// dataLinkPage 模拟公众号历史消息列表:文章链接在 data-link 上,由 JS 打开。
const dataLinkPage = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>公众号历史消息</title></head>
<body>
<div id="list">
  <div class="item" id="d1" data-link="https://mp.weixin.qq.com/s?__biz=MzA5&mid=11&idx=1"><h3>历史文章一</h3><span>2026-10-18</span></div>
  <div class="item" id="d2" data-link="https://mp.weixin.qq.com/s?__biz=MzA5&mid=12&idx=1"><h3>历史文章二</h3><span>2026-10-19</span></div>
  <div class="item" id="d3" data-link="https://mp.weixin.qq.com/s?__biz=MzA5&mid=13&idx=1"><h3>历史文章三</h3><span>2026-10-20</span></div>
</div>
<pre id="result">PENDING</pre>
<script>` + `{{.Script}}` + `</script>
<script>
var out = [];
function check(name, cond){ out.push((cond ? 'PASS ' : 'FAIL ') + name); }
function countText(){ var n = document.getElementById('wx-mp-pick-count'); return n ? n.textContent : ''; }
setTimeout(function(){
  try {
    check('历史消息页自动进入多选', !!document.getElementById('wx-mp-pick-bar'));
    check('识别到 3 条 data-link 文章', countText().indexOf('已选 0 / 共 3 篇') >= 0);
    document.getElementById('d2').click();
    check('点击卡片可选中', countText().indexOf('已选 1') >= 0);
    check('页面没有被导航走', location.href.indexOf('inject-test') >= 0);
    document.getElementById('wx-mp-pick-all').click();
    check('全选 3 篇', countText().indexOf('已选 3 / 共 3') >= 0);
  } catch (e) {
    out.push('ERROR ' + e);
  }
  document.getElementById('result').textContent = out.join('\n');
}, 800);
</script>
</body>
</html>`

// TestInjectedPickerInBrowser 在真实浏览器里验证搜索结果多选交互。
func TestInjectedPickerInBrowser(t *testing.T) {
	browser := chromePath()
	if browser == "" {
		t.Skip("本机没有 Chrome/Edge,跳过浏览器测试")
	}
	injection := buildInjection("http://127.0.0.1:9", "")
	script := extractScript(t, injection)

	dir := t.TempDir()
	writePage := func(name, tpl string) string {
		return writePageFile(t, dir, name, tpl, script)
	}

	t.Run("搜索页多选", func(t *testing.T) {
		page := writePage("inject-test-picker", pickerPage)
		dom := dumpDOM(t, browser, page, 3000)
		result := resultBlock(dom)
		if strings.Contains(result, "PENDING") {
			t.Fatalf("页面断言没有执行,result=%q", result)
		}
		if strings.Contains(result, "FAIL ") || strings.Contains(result, "ERROR ") {
			t.Errorf("断言失败:\n%s", result)
		}
		if !strings.Contains(result, "PASS 自动进入多选模式") {
			t.Errorf("未验证到多选模式,result=%q", result)
		}
	})

	t.Run("文章页按钮", func(t *testing.T) {
		page := writePage("inject-test-article", articlePage)
		dom := dumpDOM(t, browser, page, 3000)
		result := resultBlock(dom)
		if strings.Contains(result, "FAIL ") || strings.Contains(result, "ERROR ") {
			t.Errorf("断言失败:\n%s", result)
		}
		if !strings.Contains(result, "PASS 文章页出现下载按钮") {
			t.Errorf("未验证到文章页按钮,result=%q", result)
		}
	})

	t.Run("搜狗结果页", func(t *testing.T) {
		page := writePage("inject-test-sogou", sogouPage)
		dom := dumpDOM(t, browser, page, 3000)
		result := resultBlock(dom)
		if strings.Contains(result, "PENDING") {
			t.Fatalf("页面断言没有执行,result=%q", result)
		}
		if strings.Contains(result, "FAIL ") || strings.Contains(result, "ERROR ") {
			t.Errorf("断言失败:\n%s", result)
		}
		if !strings.Contains(result, "PASS 搜狗页自动进入多选") {
			t.Errorf("未验证到搜狗页多选,result=%q", result)
		}
	})

	t.Run("历史消息列表", func(t *testing.T) {
		page := writePage("inject-test-datalink", dataLinkPage)
		dom := dumpDOM(t, browser, page, 3000)
		result := resultBlock(dom)
		if strings.Contains(result, "PENDING") {
			t.Fatalf("页面断言没有执行,result=%q", result)
		}
		if strings.Contains(result, "FAIL ") || strings.Contains(result, "ERROR ") {
			t.Errorf("断言失败:\n%s", result)
		}
		if !strings.Contains(result, "PASS 历史消息页自动进入多选") {
			t.Errorf("未验证到 data-link 列表,result=%q", result)
		}
	})
}

// writePageFile 渲染一个带注入脚本的测试页面,返回文件路径。
func writePageFile(t *testing.T, dir, name, tpl, script string) string {
	t.Helper()
	tmpl, err := template.New(name).Parse(tpl)
	if err != nil {
		t.Fatalf("解析模板失败: %v", err)
	}
	path := filepath.Join(dir, name+".html")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建页面失败: %v", err)
	}
	defer f.Close()
	if err := tmpl.Execute(f, map[string]string{"Script": script}); err != nil {
		t.Fatalf("渲染页面失败: %v", err)
	}
	// 设置 WXMP_PAGE_OUT 可把渲染后的页面留一份,便于手动打开/截图调试。
	if out := strings.TrimSpace(os.Getenv("WXMP_PAGE_OUT")); out != "" {
		if err := os.MkdirAll(out, 0o755); err == nil {
			data, _ := os.ReadFile(path)
			_ = os.WriteFile(filepath.Join(out, name+".html"), data, 0o644)
		}
	}
	return path
}

// resultBlock 取出页面里 <pre id="result"> 的内容,便于失败时定位。
func resultBlock(dom string) string {
	marker := `<pre id="result">`
	idx := strings.Index(dom, marker)
	if idx < 0 {
		return truncate(dom, 1500)
	}
	rest := dom[idx+len(marker):]
	end := strings.Index(rest, "</pre>")
	if end < 0 {
		return truncate(rest, 1500)
	}
	return rest[:end]
}

// batchFlowPage 模拟搜狗结果页,并用测试桩替代真实搜狗解析(真实解析需要搜狗会话),
// 用于验证"选最新 → 还原链接 → 创建任务 → 轮询进度 → 下载 ZIP"整条链路。
const batchFlowPage = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>机场通道 – 搜狗微信搜索</title></head>
<body>
<div id="list">
  <div class="card" id="b1"><a href="https://weixin.sogou.com/link?url=old"><h3>老文章</h3></a><span>2016-11-06</span></div>
  <div class="card" id="b2"><a href="https://weixin.sogou.com/link?url=new"><h3>新文章</h3></a><span>2026-10-20</span></div>
</div>
<pre id="result">PENDING</pre>
<script>
window.__wxmpResolveSogou = function(url){
  window.__resolved = (window.__resolved || 0) + 1;
  return Promise.resolve('https://mp.weixin.qq.com/s?__biz=RESOLVED&sn=' + encodeURIComponent(url).length);
};
// 无头浏览器里真的触发下载会让 --dump-dom 一直不退出,这里只验证链路。
window.__wxmpSkipDownload = true;
</script>
<script>{{.Script}}</script>
<script>
var out = [];
function check(name, cond){ out.push((cond ? 'PASS ' : 'FAIL ') + name); }
function countText(){ var n = document.getElementById('wx-mp-pick-count'); return n ? n.textContent : ''; }
function statusText(){ var n = document.getElementById('wx-mp-pick-status'); return n ? n.textContent : ''; }
setTimeout(function(){
  try {
    document.getElementById('wx-mp-pick-newest').click();
    check('已选最新 2 篇', countText().indexOf('已选 2') >= 0);
    document.getElementById('wx-mp-pick-dl').click();
  } catch (e) {
    out.push('ERROR ' + e);
  }
  setTimeout(function(){
    check('两条链接都已还原', window.__resolved === 2);
    check('任务完成并下载 ZIP', statusText().indexOf('已下载') >= 0);
    document.getElementById('result').textContent = out.join('\n');
  }, 3500);
}, 600);
</script>
</body>
</html>`

// TestInjectedBatchFlowInBrowser 用本地假管理服务验证批量下载整条链路。
func TestInjectedBatchFlowInBrowser(t *testing.T) {
	browser := chromePath()
	if browser == "" {
		t.Skip("本机没有 Chrome/Edge,跳过浏览器测试")
	}

	var mu sync.Mutex
	var posted []string
	zipHits := 0

	mux := http.NewServeMux()
	allowCORS := func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/batch", func(w http.ResponseWriter, r *http.Request) {
		if !allowCORS(w, r) {
			return
		}
		var req struct {
			URLs []struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"urls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, u := range req.URLs {
			posted = append(posted, u.URL)
		}
		n := len(req.URLs)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "job_id": "job-1", "total": n})
	})
	mux.HandleFunc("/api/batch/status", func(w http.ResponseWriter, r *http.Request) {
		if !allowCORS(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "job": map[string]any{
			"id": "job-1", "total": 2, "done": 2, "ok": 2, "skipped": 0, "failed": 0,
			"finished": true, "items": []any{},
		}})
	})
	mux.HandleFunc("/api/batch/zip", func(w http.ResponseWriter, r *http.Request) {
		if !allowCORS(w, r) {
			return
		}
		mu.Lock()
		zipHits++
		mu.Unlock()
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		f, _ := zw.Create("a.md")
		_, _ = f.Write([]byte("# hi"))
		_ = zw.Close()
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="wx-mp-batch.zip"`)
		_, _ = w.Write(buf.Bytes())
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	script := extractScript(t, buildInjection(srv.URL, ""))
	page := writePageFile(t, t.TempDir(), "inject-test-batch", batchFlowPage, script)
	dom := dumpDOM(t, browser, page, 12000)
	result := resultBlock(dom)
	if strings.Contains(result, "PENDING") {
		t.Fatalf("页面断言没有执行,result=%q", result)
	}
	if strings.Contains(result, "FAIL ") || strings.Contains(result, "ERROR ") {
		t.Errorf("断言失败:\n%s", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 2 {
		t.Fatalf("提交了 %d 篇, 期望 2: %v", len(posted), posted)
	}
	for _, u := range posted {
		if strings.Contains(u, "weixin.sogou.com") {
			t.Errorf("提交了未还原的搜狗链接: %s", u)
		}
		if !strings.Contains(u, "mp.weixin.qq.com/s?__biz=RESOLVED") {
			t.Errorf("提交链接不是还原后的文章链接: %s", u)
		}
	}
	if zipHits == 0 {
		t.Errorf("没有请求打包接口")
	}
}
