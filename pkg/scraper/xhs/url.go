package xhs

import (
	"regexp"
	"strings"
)

var (
	// urlPattern 从整段分享文本里提取链接。
	// 只匹配 ASCII URL 字符,避免把后面的中文说明一起吞进链接里。
	urlPattern = regexp.MustCompile(`https?://[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=%]+`)
	// urlTrimCutset 去掉粘贴文本里常见的尾随标点。
	urlTrimCutset = ".,;:!?)]}>'\"，。；：！？）】》"
)

// ExtractURL 从一行文本里取出第一个小红书链接。
// 整行就是链接时直接使用;否则从文本里扫描 http(s) 链接并校验域名。
func ExtractURL(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if !strings.ContainsAny(line, " \t") {
		if candidate := strings.TrimRight(line, urlTrimCutset); IsXHSURL(candidate) {
			return candidate
		}
	}
	for _, candidate := range urlPattern.FindAllString(line, -1) {
		candidate = strings.TrimRight(candidate, urlTrimCutset)
		if IsXHSURL(candidate) {
			return candidate
		}
	}
	return ""
}
