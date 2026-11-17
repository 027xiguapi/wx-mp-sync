package xhs

import (
	"regexp"
	"strings"
)

var (
	// topicTagPattern 匹配小红书正文里的 #话题[话题]# 标记。
	// [^#] 保证只从最后一个 # 开始匹配,不会把前面的正文一起吃掉。
	topicTagPattern = regexp.MustCompile(`#\s*[^#\r\n]{1,80}?\[话题\]#`)
	// multiSpacePattern 匹配删除标签后留下的连续空格。
	multiSpacePattern = regexp.MustCompile(`[ \t]{2,}`)
	// cjkSpacePattern 匹配中文之间因删除标签留下的空格,例如 "2款 新品"。
	cjkSpacePattern = regexp.MustCompile(`([\p{Han}])[ \t]+([\p{Han}])`)
)

// StripTopicTags 去掉小红书正文里的 #话题[话题]# 标记,
// 并清理因删除标签产生的空格和多余空行。
// 只处理平台内检索语法,标题、正文和标签列表本身保持原样。
func StripTopicTags(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// 用空格替换标签,避免前后文字粘在一起;连续空行再合并。
	text = topicTagPattern.ReplaceAllString(text, " ")

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	text = strings.Join(lines, "\n")
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}

	text = multiSpacePattern.ReplaceAllString(text, " ")
	text = cjkSpacePattern.ReplaceAllString(text, "$1$2")
	return strings.TrimSpace(text)
}
