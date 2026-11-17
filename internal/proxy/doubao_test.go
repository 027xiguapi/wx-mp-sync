package proxy

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// TestBuildDoubaoInjection 验证豆包按钮脚本的关键内容。
func TestBuildDoubaoInjection(t *testing.T) {
	html := buildDoubaoInjection("http://127.0.0.1:9090", "")
	for _, want := range []string{
		"doubao-dl-btn", "⬇ 下载本页", "/api/doubao/one",
		"http://127.0.0.1:9090", "thread", "setInterval",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("注入内容缺少 %q", want)
		}
	}
}

// TestBuildDoubaoInjectionCSPNonce 验证 CSP nonce 会带到 script 标签上。
func TestBuildDoubaoInjectionCSPNonce(t *testing.T) {
	html := buildDoubaoInjection("http://127.0.0.1:9090", "abc123")
	if !strings.Contains(html, `nonce="abc123"`) {
		t.Errorf("缺少 CSP nonce: %s", html)
	}
}

// TestBuildDoubaoScriptSyntax 用 goja 编译脚本,确保语法正确。
func TestBuildDoubaoScriptSyntax(t *testing.T) {
	script := extractScript(t, buildDoubaoInjection("http://127.0.0.1:9090", ""))
	if _, err := goja.Compile("doubao-inject.js", script, false); err != nil {
		t.Fatalf("豆包注入脚本语法错误: %v", err)
	}
}
