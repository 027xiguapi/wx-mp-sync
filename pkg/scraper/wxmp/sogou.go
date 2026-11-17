package wxmp

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// sogouUserAgent 是访问搜狗微信时使用的浏览器 UA(用微信 UA 会被跳转页拒绝的比例更高)。
const sogouUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36"

var (
	// 搜狗跳转页把真实链接拆成多段 JS 字符串拼接:url += 'https://mp.'; url += 'weixin.qq.c'; ...
	sogouURLPartRe = regexp.MustCompile(`url\s*\+=\s*'([^']*)'`)
	// 少数情况下直接 replace 到目标地址。
	sogouLocationRe = regexp.MustCompile(`window\.location\.replace\(\s*["']([^"']+)["']\s*\)`)
)

// IsSogouLink 判断链接是否为搜狗微信的 /link?url=... 跳转链接。
func IsSogouLink(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if !strings.EqualFold(u.Hostname(), "weixin.sogou.com") {
		return false
	}
	return strings.HasPrefix(strings.ToLower(u.Path), "/link")
}

// ParseSogouRedirect 从搜狗跳转页的 HTML 中还原真实文章链接。
// 搜狗会把链接拆成多段字符串再拼接,并插入 @ 来干扰解析。
func ParseSogouRedirect(page string) (string, error) {
	var b strings.Builder
	for _, m := range sogouURLPartRe.FindAllStringSubmatch(page, -1) {
		b.WriteString(m[1])
	}
	real := b.String()
	if strings.TrimSpace(real) == "" {
		if m := sogouLocationRe.FindStringSubmatch(page); len(m) > 1 {
			real = strings.ReplaceAll(m[1], `\x26`, "&")
		}
	}
	real = strings.ReplaceAll(real, "@", "")
	// 只还原 &amp; 之类的 URL 实体;不能用 html.UnescapeString,
	// 否则合法参数名如 "&times;tamp=" 会被当成 HTML 实体解成 "×tamp="。
	real = strings.ReplaceAll(real, "&amp;", "&")
	real = strings.ReplaceAll(real, "&#38;", "&")
	real = strings.ReplaceAll(real, "&#x26;", "&")
	real = strings.TrimSpace(real)
	if real == "" {
		return "", fmt.Errorf("未能解析搜狗跳转链接")
	}
	if !strings.HasPrefix(real, "http://") && !strings.HasPrefix(real, "https://") {
		return "", fmt.Errorf("搜狗跳转结果不是有效链接: %.120s", real)
	}
	return real, nil
}

// ResolveSogouLink 请求搜狗 /link 跳转页,返回真实的公众号文章链接。
// 搜狗对频繁请求会要求验证码,此时返回可读错误,调用方按单篇失败处理即可。
func ResolveSogouLink(raw string) (string, error) {
	if !IsSogouLink(raw) {
		return "", fmt.Errorf("不是搜狗跳转链接: %.120s", raw)
	}
	return resolveSogouLinkWithClient(&http.Client{Timeout: 20 * time.Second}, raw)
}

// resolveSogouLinkWithClient 是 ResolveSogouLink 的可注入客户端版本(便于测试)。
func resolveSogouLinkWithClient(client *http.Client, raw string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", sogouUserAgent)
	req.Header.Set("Referer", "https://weixin.sogou.com/")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求搜狗跳转页失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return "", fmt.Errorf("读取搜狗跳转页失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("搜狗跳转页返回 %s", resp.Status)
	}
	page := string(body)
	if strings.Contains(page, "antispider") || strings.Contains(page, "请输入验证码") {
		return "", fmt.Errorf("搜狗要求输入验证码,请稍后重试或直接在微信里打开该文章")
	}
	return ParseSogouRedirect(page)
}
