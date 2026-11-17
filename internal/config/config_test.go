package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

func TestLoadNaichaConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
output_dir: ./tmp-articles
naicha_sync:
  enabled: true
  api_base: http://127.0.0.1:3002/
  token: test-token
  category: promo
  tags: [公众号, 奶茶]
  author: 茶友
  auto: true
  timeout_sec: 45
  board: coupon
  username: admin
  password: secret
  cookie: bbs_session=abc
  xhs_enabled: false
  xhs_tags: [小红书, 奶茶]
  xhs_cookie: a1=xyz
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	logger := zerolog.Nop()
	cfg := New("test", &logger)
	if err := cfg.LoadFromFile(path); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	n := cfg.Naicha
	if !n.Enabled || n.APIBase != "http://127.0.0.1:3002/" || n.Token != "test-token" {
		t.Fatalf("unexpected naicha config: %+v", n)
	}
	if n.Category != "promo" || !n.Auto || n.TimeoutSec != 45 || n.Author != "茶友" {
		t.Fatalf("unexpected naicha config: %+v", n)
	}
	if len(n.Tags) != 2 || n.Tags[0] != "公众号" || n.Tags[1] != "奶茶" {
		t.Fatalf("tags = %v", n.Tags)
	}
	if n.Board != "coupon" || n.Username != "admin" || n.Password != "secret" || n.Cookie != "bbs_session=abc" {
		t.Fatalf("unexpected xhs account config: %+v", n)
	}
	if n.XHSEnabled == nil || *n.XHSEnabled {
		t.Fatalf("xhs_enabled = %+v", n.XHSEnabled)
	}
	if len(n.XHSTags) != 2 || n.XHSTags[0] != "小红书" || n.XHSCookie != "a1=xyz" {
		t.Fatalf("unexpected xhs config: %+v", n)
	}
	if n.XHSActive() {
		t.Fatal("XHSActive should be false when xhs_enabled=false")
	}

	// 未显式配置 xhs_enabled 时默认启用。
	cfg.Naicha.XHSEnabled = nil
	if !cfg.Naicha.XHSActive() {
		t.Fatal("XHSActive should default to true")
	}
}

func TestNormalizeNaichaDefaults(t *testing.T) {
	logger := zerolog.Nop()
	cfg := New("test", &logger)
	cfg.Naicha.Enabled = true
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if cfg.Naicha.APIBase != "http://localhost:3002" {
		t.Fatalf("api_base = %q", cfg.Naicha.APIBase)
	}
	if cfg.Naicha.Category != "discussion" {
		t.Fatalf("category = %q", cfg.Naicha.Category)
	}
	if cfg.Naicha.TimeoutSec <= 0 {
		t.Fatalf("timeout = %d", cfg.Naicha.TimeoutSec)
	}
}
