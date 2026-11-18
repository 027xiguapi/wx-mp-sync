//go:build windows

package main

import (
	"strings"
	"testing"
)

func TestParseLinks(t *testing.T) {
	const idA = "6af55bb7000000002b01a361"
	const idB = "6aeb09dd0000000003016c8f"
	input := strings.Join([]string{
		"# 注释行会被忽略",
		"https://www.xiaohongshu.com/discovery/item/" + idA + "?xsec_token=abc",
		"看看这个 https://www.xiaohongshu.com/discovery/item/" + idA + "?xsec_token=other，复制后打开",
		"https://www.xiaohongshu.com/explore/" + idB,
		"https://example.com/not-xhs",
		"",
	}, "\n")

	urls, invalid := parseLinks(input)
	if len(urls) != 2 {
		t.Fatalf("链接数 = %d, 期望 2 (%+v)", len(urls), urls)
	}
	if invalid != 1 {
		t.Errorf("invalid = %d, 期望 1", invalid)
	}
	if !strings.Contains(urls[0], "xsec_token=abc") || strings.Contains(urls[0], "复制后打开") {
		t.Errorf("第一个链接 = %q", urls[0])
	}
	if urls[1] != "https://www.xiaohongshu.com/explore/"+idB {
		t.Errorf("第二个链接 = %q", urls[1])
	}
}

func TestDefaultOutputDir(t *testing.T) {
	if got := defaultOutputDir(); got == "" {
		t.Fatal("默认保存目录为空")
	}
}

func TestParseTagList(t *testing.T) {
	tags := parseTagList(" 奶茶, 探店,奶茶；咖啡、新店\n")
	want := []string{"奶茶", "探店", "咖啡", "新店"}
	if len(tags) != len(want) {
		t.Fatalf("标签 = %v, 期望 %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Errorf("tags[%d] = %q, 期望 %q", i, tags[i], want[i])
		}
	}
	if got := parseTagList("  "); len(got) != 0 {
		t.Errorf("空白输入应返回空标签,实际 %v", got)
	}
}

func TestNormalizeAPIBase(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", defaultForumAPIBase},
		{"   ", defaultForumAPIBase},
		{"localhost:3000", "http://localhost:3000"},
		{"http://localhost:3000/", "http://localhost:3000"},
		{"https://bbs.example.com/naicha/", "https://bbs.example.com/naicha"},
		{"192.168.1.10:3000", "http://192.168.1.10:3000"},
	}
	for _, c := range cases {
		if got := normalizeAPIBase(c.in); got != c.want {
			t.Errorf("normalizeAPIBase(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestBuildForumSettings(t *testing.T) {
	cfg, err := buildForumSettings(" localhost:3000/ ", " quanxiaoer ", " secret ", "", "奶茶, 探店，新品")
	if err != nil {
		t.Fatalf("buildForumSettings: %v", err)
	}
	if cfg.APIBase != "http://localhost:3000" {
		t.Errorf("APIBase = %q", cfg.APIBase)
	}
	if cfg.Bot != "quanxiaoer" || cfg.BotSecret != "secret" {
		t.Errorf("bot/secret = %+v", cfg)
	}
	if cfg.Board != defaultForumBoard {
		t.Errorf("Board = %q", cfg.Board)
	}
	if len(cfg.Tags) != 3 || cfg.Tags[0] != "奶茶" || cfg.Tags[2] != "新品" {
		t.Errorf("Tags = %v", cfg.Tags)
	}

	if _, err := buildForumSettings("ftp://example.com", "bot", "secret", "board", ""); err == nil {
		t.Error("非 http(s) 地址应返回错误")
	}
}

func TestNormalizeForumSettings(t *testing.T) {
	cfg := normalizeForumSettings(forumSettings{})
	if cfg.APIBase != defaultForumAPIBase || cfg.Board != defaultForumBoard {
		t.Errorf("默认值 = %+v", cfg)
	}
}

func TestForumSummaryHidesSecret(t *testing.T) {
	app := &downloaderApp{forum: forumSettings{
		APIBase:   "http://localhost:3000",
		Bot:       "quanxiaoer",
		BotSecret: "super-secret",
		Board:     "general-discussion",
	}}
	summary := app.forumSummaryText()
	if strings.Contains(summary, "super-secret") {
		t.Fatalf("摘要不应包含密钥: %q", summary)
	}
	if !strings.Contains(summary, "quanxiaoer") || !strings.Contains(summary, "general-discussion") {
		t.Errorf("摘要缺少机器人/分类: %q", summary)
	}
}

func TestValidateAPIBase(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:3000",
		"https://bbs.example.com/naicha",
		"http://192.168.1.10:3000",
	} {
		if err := validateAPIBase(raw); err != nil {
			t.Errorf("validateAPIBase(%q) 应通过,实际 %v", raw, err)
		}
	}
	for _, raw := range []string{"ftp://example.com", "http://", ""} {
		if err := validateAPIBase(raw); err == nil {
			t.Errorf("validateAPIBase(%q) 应失败", raw)
		}
	}
}
