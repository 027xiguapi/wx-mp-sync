package proxy

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// TestBuildXHSInjection 验证小红书按钮脚本的关键内容。
func TestBuildXHSInjection(t *testing.T) {
	html := buildXHSInjection("http://127.0.0.1:9090", "")
	for _, want := range []string{
		"xhs-naicha-btn", "⬆ 同步到奶茶圈", "/api/xhs/sync",
		"http://127.0.0.1:9090", "__INITIAL_STATE__", "noteDetailMap", "setInterval",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("注入内容缺少 %q", want)
		}
	}
}

func TestBuildXHSInjectionCSPNonce(t *testing.T) {
	html := buildXHSInjection("http://127.0.0.1:9090", "abc123")
	if !strings.Contains(html, `nonce="abc123"`) {
		t.Errorf("缺少 CSP nonce: %s", html)
	}
}

func TestBuildXHSScriptSyntax(t *testing.T) {
	script := extractScript(t, buildXHSInjection("http://127.0.0.1:9090", ""))
	if _, err := goja.Compile("xhs-inject.js", script, false); err != nil {
		t.Fatalf("小红书注入脚本语法错误: %v", err)
	}
}
