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
