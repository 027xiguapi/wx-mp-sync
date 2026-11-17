package chatgpt

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Article 是一份解析后的 ChatGPT 分享。
type Article struct {
	ShareID   string
	Title     string
	Author    string
	SourceURL string
	CreatedAt time.Time
	Messages  int
	Markdown  string
	Images    []Image
}

// Image 是正文里的一张图片。
type Image struct {
	URL     string
	Caption string
	Key     string
}

// chatMessage 是对话里的一条消息。
type chatMessage struct {
	Role   string
	Text   string
	Images []Image
}

// buildArticle 把分享页的 post 转成 Article。
func buildArticle(post map[string]any, shareID string) (*Article, error) {
	id := strings.TrimSpace(asString(post["id"]))
	if id == "" {
		id = shareID
	}
	if id == "" {
		return nil, fmt.Errorf("分享里没有 id")
	}

	article := &Article{
		ShareID:   id,
		Title:     strings.TrimSpace(asString(post["og_title"])),
		SourceURL: CanonicalURL(id),
	}
	if !isGenericTitle(article.Title) {
		// 使用分享自带的标题。
	} else {
		article.Title = ""
	}
	if ts, ok := asFloat(post["posted_at"]); ok && ts > 0 {
		article.CreatedAt = time.Unix(int64(ts), int64((ts-float64(int64(ts)))*1e9))
	}

	var messages []chatMessage
	if atts, ok := post["attachments"].([]any); ok {
		for _, att := range atts {
			m, ok := att.(map[string]any)
			if !ok {
				continue
			}
			messages = append(messages, messagesFromAttachment(m)...)
		}
	}

	images := map[string]bool{}
	refImages := func(list []Image) {
		for _, img := range list {
			if img.URL == "" || images[img.Key] {
				continue
			}
			images[img.Key] = true
			article.Images = append(article.Images, img)
		}
	}

	for i := range messages {
		refImages(messages[i].Images)
		if strings.TrimSpace(messages[i].Text) == "" && len(messages[i].Images) == 0 {
			continue
		}
		article.Messages++
	}

	if article.Title == "" {
		article.Title = titleFromMessages(messages, id)
	}

	article.Markdown = renderMarkdown(article, messages)
	return article, nil
}

// messagesFromAttachment 解析一个附件里的消息或图片。
func messagesFromAttachment(att map[string]any) []chatMessage {
	kind := strings.TrimSpace(asString(att["kind"]))
	if list, ok := att["messages"].([]any); ok {
		out := make([]chatMessage, 0, len(list))
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, parseMessage(m))
			}
		}
		return out
	}
	if kind == "media_generation" || kind == "image" {
		if imgs := imagesFromAttachment(att); len(imgs) > 0 {
			return []chatMessage{{Role: "assistant", Images: imgs}}
		}
	}
	for _, key := range []string{"text", "prompt", "code"} {
		if text := strings.TrimSpace(asString(att[key])); text != "" {
			return []chatMessage{{Role: "assistant", Text: text}}
		}
	}
	return nil
}

// parseMessage 解析一条消息的正文与图片。
func parseMessage(m map[string]any) chatMessage {
	role := "assistant"
	if author, ok := m["author"].(map[string]any); ok {
		if r := strings.TrimSpace(asString(author["role"])); r != "" {
			role = r
		}
	}

	var textParts []string
	if content, ok := m["content"].(map[string]any); ok {
		if parts, ok := content["parts"].([]any); ok {
			for _, part := range parts {
				switch p := part.(type) {
				case string:
					if strings.TrimSpace(p) != "" {
						textParts = append(textParts, p)
					}
				case map[string]any:
					if t := strings.TrimSpace(asString(p["text"])); t != "" {
						textParts = append(textParts, t)
					}
				}
			}
		}
	}
	text := strings.Join(textParts, "\n\n")

	var components []genuiComponent
	if metadata, ok := m["metadata"].(map[string]any); ok {
		components = parseGenUIComponents(metadata["genui_components"])
	}

	text, inlineImages := renderGenUI(text, components)
	images := inlineImages

	return chatMessage{Role: role, Text: strings.TrimSpace(text), Images: dedupeImages(images)}
}

// genuiComponent 是正文里的一个图片组件。
type genuiComponent struct {
	Start  int
	End    int
	Images []Image
}

// parseGenUIComponents 解析 genui_components。
func parseGenUIComponents(raw any) []genuiComponent {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]genuiComponent, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		start, _ := asFloat(m["start_index"])
		end, _ := asFloat(m["end_index"])
		imgs := imagesFromSafeURLs(m["safe_urls"])
		if len(imgs) == 0 {
			continue
		}
		out = append(out, genuiComponent{Start: int(start), End: int(end), Images: imgs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// renderGenUI 把正文里的 <AsyncImageGroup .../> 标签替换成图片 Markdown。
func renderGenUI(text string, comps []genuiComponent) (string, []Image) {
	if len(comps) == 0 {
		return stripAsyncImageTags(text), nil
	}
	runes := []rune(text)
	var b strings.Builder
	var images []Image
	prev := 0
	for _, comp := range comps {
		start, end := comp.Start, comp.End
		if start < prev {
			start = prev
		}
		if end < start || start > len(runes) {
			continue
		}
		if end > len(runes) {
			end = len(runes)
		}
		b.WriteString(string(runes[prev:start]))
		b.WriteString("\n\n")
		for _, img := range comp.Images {
			b.WriteString("![" + mdAlt(img.Caption) + "](" + img.URL + ")\n\n")
			images = append(images, img)
		}
		prev = end
	}
	if prev < len(runes) {
		b.WriteString(string(runes[prev:]))
	}
	return stripAsyncImageTags(b.String()), images
}

// stripAsyncImageTags 清掉没有对应组件的图片标签,避免出现在 Markdown 里。
func stripAsyncImageTags(text string) string {
	return asyncImageGroupRe.ReplaceAllString(text, "")
}

// imagesFromSafeURLs 从组件的 safe_urls 里取图片。
// 同时有 fullsize / inline 时只保留 fullsize,避免同一张图下载两遍。
func imagesFromSafeURLs(raw any) []Image {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var full, inline []Image
	seen := map[string]bool{}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		u := strings.TrimSpace(asString(m["url"]))
		if !strings.Contains(u, "images.openai.com/") {
			continue
		}
		key := u
		if i := strings.IndexAny(key, "?#"); i >= 0 {
			key = key[:i]
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		img := Image{URL: u, Key: "img:" + key}
		if strings.Contains(u, "purpose=fullsize") {
			full = append(full, img)
		} else if strings.Contains(u, "purpose=inline") {
			inline = append(inline, img)
		} else {
			full = append(full, img)
		}
	}
	if len(full) > 0 {
		return full
	}
	return inline
}

// imagesFromAttachment 解析 media_generation 附件里的图片。
func imagesFromAttachment(att map[string]any) []Image {
	var out []Image
	encodings, ok := att["encodings"].(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"md", "unfurl", "thumbnail"} {
		enc, ok := encodings[key].(map[string]any)
		if !ok {
			continue
		}
		u := strings.TrimSpace(asString(enc["path"]))
		if !strings.HasPrefix(u, "http") || !isAllowedImageHost(u) {
			continue
		}
		out = append(out, Image{URL: u, Key: "img:" + u})
	}
	return out
}

// isAllowedImageHost 限制下载域名,避免把分享里的任意 URL 当成图片请求。
func isAllowedImageHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range []string{
		"images.openai.com", "files.oaiusercontent.com", "cdn.openai.com",
		"chatgpt.com", "chat.openai.com",
	} {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

// dedupeImages 按 Key 去重。
func dedupeImages(in []Image) []Image {
	out := make([]Image, 0, len(in))
	seen := map[string]bool{}
	for _, img := range in {
		if img.URL == "" || seen[img.Key] {
			continue
		}
		seen[img.Key] = true
		out = append(out, img)
	}
	return out
}

// titleFromMessages 在没有有效标题时,从正文第一行生成标题。
func titleFromMessages(messages []chatMessage, shareID string) string {
	for _, msg := range messages {
		for _, line := range strings.Split(msg.Text, "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimLeft(line, "#>-* ")
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if len([]rune(line)) > 60 {
				line = string([]rune(line)[:60]) + "…"
			}
			return line
		}
	}
	return "ChatGPT 分享 " + shareID
}

// isGenericTitle 判断分享标题是否是占位文案。
func isGenericTitle(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return true
	}
	for _, generic := range []string{
		"ChatGPT", "ChatGPT - Shared Content", "Shared Content",
		"看看这段聊天", "有人觉得你可能会想看看这段聊天。", "共享内容",
	} {
		if t == generic {
			return true
		}
	}
	return false
}

// renderMarkdown 生成最终的 Markdown。
func renderMarkdown(a *Article, messages []chatMessage) string {
	var b strings.Builder
	b.WriteString("# " + a.Title + "\n\n")

	meta := []string{"来源：ChatGPT 分享"}
	if a.CreatedAt.IsZero() == false {
		meta = append(meta, "分享时间："+a.CreatedAt.Local().Format("2006年01月02日 15:04"))
	}
	b.WriteString("> " + strings.Join(meta, " · ") + "\n")
	b.WriteString("> 链接：" + a.SourceURL + "\n")
	b.WriteString("\n---\n\n")

	for _, msg := range messages {
		if strings.TrimSpace(msg.Text) == "" && len(msg.Images) == 0 {
			continue
		}
		b.WriteString("## " + roleName(msg.Role) + "\n\n")
		if text := strings.TrimSpace(msg.Text); text != "" {
			b.WriteString(demoteHeadings(text))
			b.WriteString("\n\n")
		}
		for _, img := range msg.Images {
			alt := img.Caption
			if alt == "" {
				alt = "图片"
			}
			b.WriteString("![" + mdAlt(alt) + "](" + img.URL + ")\n\n")
		}
	}
	return strings.TrimSpace(b.String()) + "\n"
}

// roleName 把接口里的角色名转成中文标题。
func roleName(role string) string {
	switch strings.ToLower(role) {
	case "user":
		return "我"
	case "assistant":
		return "ChatGPT"
	case "tool":
		return "工具"
	case "system":
		return "系统"
	default:
		if role == "" {
			return "ChatGPT"
		}
		return role
	}
}

// demoteHeadings 把正文标题降一级,避免和消息标题冲突。
func demoteHeadings(text string) string {
	lines := strings.Split(text, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := headingRe.FindStringSubmatch(line)
		if len(m) != 4 {
			continue
		}
		level := len(m[2])
		if level < 6 {
			level++
		}
		lines[i] = m[1] + strings.Repeat("#", level) + m[3] + line[len(m[1])+len(m[2])+len(m[3]):]
	}
	return strings.Join(lines, "\n")
}

var headingRe = regexp.MustCompile(`^(\s*)(#{1,6})(\s)`)

// mdAlt 处理 Markdown 替代文字。
func mdAlt(s string) string {
	s = strings.ReplaceAll(s, "[", "［")
	s = strings.ReplaceAll(s, "]", "］")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// asString 把 JSON 解码出来的值转成字符串。
func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	case float64:
		return fmt.Sprintf("%v", x)
	case bool:
		return fmt.Sprintf("%v", x)
	}
	return ""
}

// asFloat 把 JSON 数字转成 float64。
func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}
