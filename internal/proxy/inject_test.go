package proxy

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// extractScript 取出注入 HTML 中的内联脚本内容。
func extractScript(t *testing.T, injection string) string {
	t.Helper()
	idx := strings.Index(injection, "<script")
	if idx < 0 {
		t.Fatalf("注入 HTML 缺少 script 标签: %s", injection)
	}
	gt := strings.Index(injection[idx:], ">")
	if gt < 0 {
		t.Fatalf("script 标签不完整")
	}
	start := idx + gt + 1
	end := strings.LastIndex(injection, "</script>")
	if end < start {
		t.Fatalf("缺少 </script>")
	}
	return injection[start:end]
}

// TestBuildInjectionButtons 验证注入脚本会创建同步、下载和发帖按钮。
func TestBuildInjectionButtons(t *testing.T) {
	html := buildInjection("http://127.0.0.1:8080", "")
	for _, want := range []string{
		`'wx-mp-sync-btn'`, `'wx-mp-dl-btn'`, `'wx-mp-naicha-btn'`,
		"'⬇ 同步'", "'⬇ 下载'", "'⬆ 发帖'", "/api/save", "/api/save?naicha=1",
		"/api/status", "/api/download", "/api/diag",
		"MutationObserver", "documentElement",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("注入内容缺少 %q", want)
		}
	}
}

// TestBuildInjectionScriptSyntax 用 goja 编译脚本,确保语法正确。
func TestBuildInjectionScriptSyntax(t *testing.T) {
	script := extractScript(t, buildInjection("http://127.0.0.1:8080", ""))
	if _, err := goja.Compile("wx-mp-sync-inject.js", script, false); err != nil {
		t.Fatalf("注入脚本语法错误: %v", err)
	}
}

// TestBuildInjectionCSPNonce 验证 CSP nonce 会被带到 script 标签上。
func TestBuildInjectionCSPNonce(t *testing.T) {
	html := buildInjection("http://127.0.0.1:8080", "script-src 'nonce-abc123'")
	if !strings.Contains(html, `nonce="abc123"`) {
		t.Errorf("缺少 CSP nonce: %s", html)
	}
}
