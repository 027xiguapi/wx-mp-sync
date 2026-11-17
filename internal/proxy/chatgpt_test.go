package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dop251/goja"
	"github.com/ltaoo/echo"
)

// TestBuildChatGPTInjection 验证 ChatGPT 下载按钮脚本的关键内容。
func TestBuildChatGPTInjection(t *testing.T) {
	html := buildChatGPTInjection("http://127.0.0.1:9090", "")
	for _, want := range []string{
		"chatgpt-dl-btn", "⬇ 下载本页", "/api/chatgpt/one",
		"http://127.0.0.1:9090", "chatgpt.com", "setInterval",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("注入内容缺少 %q", want)
		}
	}
}

// TestBuildChatGPTInjectionCSPNonce 验证 CSP nonce 会带到 script 标签上。
func TestBuildChatGPTInjectionCSPNonce(t *testing.T) {
	html := buildChatGPTInjection("http://127.0.0.1:9090", "abc123")
	if !strings.Contains(html, `nonce="abc123"`) {
		t.Errorf("缺少 CSP nonce: %s", html)
	}
}

// TestBuildChatGPTScriptSyntax 用 goja 编译脚本,确保语法正确。
func TestBuildChatGPTScriptSyntax(t *testing.T) {
	script := extractScript(t, buildChatGPTInjection("http://127.0.0.1:9090", ""))
	if _, err := goja.Compile("chatgpt-inject.js", script, false); err != nil {
		t.Fatalf("ChatGPT 注入脚本语法错误: %v", err)
	}
}

// TestChatGPTPluginRewritesCSP 模拟一次 HTML 响应,验证插件会修改 CSP 头并注入脚本。
func TestChatGPTPluginRewritesCSP(t *testing.T) {
	plugin := NewChatGPTInjectPlugin(InjectOptions{Match: "chatgpt.com", AdminBase: "http://127.0.0.1:9090"})
	csp := "base-uri 'none'; connect-src 'self' https://chatgpt.com; img-src https:"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Content-Security-Policy": {csp},
		},
		Body: io.NopCloser(strings.NewReader("<html><head></head><body></body></html>")),
	}
	ctx := &echo.Context{Req: httptest.NewRequest(http.MethodGet, "https://chatgpt.com/s/t_abc12345", nil), Res: resp}
	plugin.OnResponse(ctx)

	gotCSP := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(gotCSP, "http://127.0.0.1:9090") {
		t.Errorf("CSP 未追加本地地址: %s", gotCSP)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "chatgpt-dl-btn") {
		t.Errorf("响应里没有注入脚本")
	}
}

// TestRewriteMetaCSP 验证 meta 形式的 CSP 也会被修改。
func TestRewriteMetaCSP(t *testing.T) {
	body := `<html><head><meta http-equiv="Content-Security-Policy" content="base-uri 'none'; connect-src 'self' https://chatgpt.com; img-src https:"><title>x</title></head></html>`
	got := rewriteMetaCSP(body, "http://127.0.0.1:9090")
	if !strings.Contains(got, "127.0.0.1:9090") || !strings.Contains(got, "connect-src") {
		t.Errorf("meta CSP 未追加本地地址:\n%s", got)
	}
	if !strings.Contains(got, "img-src https:") {
		t.Errorf("其它指令被破坏:\n%s", got)
	}
	if strings.Contains(got, `<meta http-equiv="Content-Security-Policy"`) == false {
		t.Errorf("meta 标签被破坏:\n%s", got)
	}
}

// TestAddConnectSrc 验证本地地址会追加到 CSP 的 connect-src。
func TestAddConnectSrc(t *testing.T) {
	csp := "base-uri 'none'; connect-src 'self' https://chatgpt.com; img-src https:"
	got, ok := addConnectSrc(csp, "http://127.0.0.1:9090")
	if !ok {
		t.Fatal("没有识别到 connect-src")
	}
	for _, want := range []string{
		"connect-src 'self' https://chatgpt.com http://127.0.0.1:9090 http://127.0.0.1:*",
		"base-uri 'none'",
		"img-src https:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("结果缺少 %q:\n%s", want, got)
		}
	}
	if _, ok := addConnectSrc("base-uri 'none'", "http://127.0.0.1:9090"); ok {
		t.Error("没有 connect-src 时不应新增")
	}
}
