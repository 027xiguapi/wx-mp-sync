package doubao

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// maxReferences 限制附录里的参考来源数量,避免一次搜索返回上百条链接。
	maxReferences = 30
	// headingDemotePattern 匹配 Markdown 标题行(允许前面有空格)。
	headingDemotePattern = `^(\s*)(#{1,6})(\s)`
)

var headingRe = regexp.MustCompile(headingDemotePattern)

// Article 是一份解析后的豆包分享,供归档器直接使用。
type Article struct {
	ShareID    string
	Title      string
	Author     string
	BotName    string
	SourceURL  string
	CreatedAt  time.Time
	Messages   []ChatMessage
	Markdown   string
	Images     []Image
	References []Reference
	// ReferencesOmitted 是参考来源超出上限被省略的条数。
	ReferencesOmitted int
}

// ChatMessage 是对话里的一条消息。
type ChatMessage struct {
	Role   string
	Text   string
	Images []Image
}

// Image 是正文里的一张图片。URL 为最终要下载的地址。
type Image struct {
	URL     string
	Caption string
	Kind    string // search / generated / attachment
	Key     string // 去重键
}

// Reference 是回答里引用的网页来源。
type Reference struct {
	Title string
	URL   string
}

// textTag 是消息正文里的富文本标签(搜索图片、引用锚点等)。
type textTag struct {
	Type    int    `json:"type"`
	TagInfo string `json:"tag_info"`
}

type msgContent struct {
	Text             string            `json:"text"`
	TextTags         []textTag         `json:"text_tags"`
	SearchReferences []searchReference `json:"search_references"`
}

type searchReference struct {
	TextCard *struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"text_card"`
}

// tagInfo 是 text_tag 里 tag_info 的结构,images 在嵌套的 value JSON 里。
type tagInfo struct {
	Pos   json.RawMessage `json:"pos"`
	Value string          `json:"value"`
}

type tagValue struct {
	Images []tagImage `json:"images"`
}

type tagImage struct {
	ID             json.RawMessage `json:"id"`
	URI            string          `json:"uri"`
	ThumbURL       string          `json:"thumb_url"`
	FullSizeURL    json.RawMessage `json:"full_size_url"`
	OriginThumbURL string          `json:"origin_thumb_url"`
	ImgCaption     string          `json:"img_caption"`
	ImageSource    string          `json:"image_source"`
}

// contentBlock 兼容老格式:content 是 block 数组而非 {"text": ...}。
type contentBlock struct {
	BlockType int               `json:"block_type"`
	Content   json.RawMessage   `json:"content"`
	MetaInfo  []json.RawMessage `json:"meta_info"`
}

// buildArticle 把接口数据转换成 Article(含 Markdown)。
func buildArticle(data *shareData) (*Article, error) {
	if data == nil {
		return nil, fmt.Errorf("分享数据为空")
	}
	info := data.ShareInfo
	all := data.MessageSnapshot.MessageList
	if len(all) == 0 {
		all = data.MessageSnapshot.ThreadMessageList
	}
	if info.ShareID == "" && len(all) == 0 {
		return nil, fmt.Errorf("分享不存在或已被删除")
	}

	article := &Article{
		ShareID: strings.TrimSpace(info.ShareID),
		Title:   strings.TrimSpace(info.ShareName),
		Author:  strings.TrimSpace(info.User.Nickname),
		BotName: strings.TrimSpace(info.Bot.Name),
	}
	if article.ShareID != "" {
		article.SourceURL = CanonicalURL(article.ShareID)
	}
	if article.Title == "" {
		article.Title = "豆包对话"
	}
	if article.BotName == "" {
		article.BotName = "豆包"
	}

	sort.SliceStable(all, func(i, j int) bool {
		return messageIndex(all[i]) < messageIndex(all[j])
	})

	article.CreatedAt = parseUnixMillis(string(info.ShareTime))
	if article.CreatedAt.IsZero() {
		for _, m := range all {
			if t := parseUnixSeconds(string(m.CreateTime)); !t.IsZero() {
				article.CreatedAt = t
				break
			}
		}
	}

	images := map[string]*Image{}
	addImage := func(img Image) *Image {
		if img.URL == "" {
			return nil
		}
		if img.Key == "" {
			img.Key = img.URL
		}
		if existing, ok := images[img.Key]; ok {
			return existing
		}
		stored := img
		images[img.Key] = &stored
		article.Images = append(article.Images, img)
		return &stored
	}

	refSeen := map[string]bool{}
	imageMode := detectImageMode(all)
	for _, m := range all {
		msg := parseChatMessage(m)
		msg.Images = nil
		for _, img := range parseMessageImages(m, imageMode) {
			if canonical := addImage(img); canonical != nil {
				msg.Images = append(msg.Images, *canonical)
			}
		}
		for _, ref := range parseReferences(m) {
			if ref.URL == "" || refSeen[ref.URL] {
				continue
			}
			refSeen[ref.URL] = true
			article.References = append(article.References, ref)
		}
		if strings.TrimSpace(msg.Text) == "" && len(msg.Images) == 0 {
			continue
		}
		article.Messages = append(article.Messages, msg)
	}

	if len(article.References) > maxReferences {
		article.ReferencesOmitted = len(article.References) - maxReferences
		article.References = article.References[:maxReferences]
	}

	article.Markdown = renderMarkdown(article)
	return article, nil
}

// messageIndex 把 index_in_conv 转成可排序的数字。
func messageIndex(m message) int {
	n, err := strconv.Atoi(strings.TrimSpace(string(m.IndexInConv)))
	if err != nil {
		return 1 << 30
	}
	return n
}

// parseChatMessage 提取一条消息的发送方与正文。
func parseChatMessage(m message) ChatMessage {
	role := "系统"
	switch m.UserType {
	case 1:
		role = "我"
	case 2:
		role = "豆包"
	}

	text, _, _ := parseMessageContent(m)
	if strings.TrimSpace(text) == "" {
		text, _, _ = m.extraContent()
	}
	return ChatMessage{Role: role, Text: strings.TrimSpace(text)}
}

// parseMessageContent 解析 content 字符串,返回正文、标签与搜索引用。
func parseMessageContent(m message) (string, []textTag, []searchReference) {
	text, tags, refs := parseContentString(m.Content)
	if text == "" && len(m.ContentBlock) > 0 {
		text = textFromBlocks(m.ContentBlock)
	}
	return text, tags, refs
}

// parseContentString 解析一段 content(可能是 {"text":...},也可能是 block 数组)。
func parseContentString(raw string) (string, []textTag, []searchReference) {
	if strings.TrimSpace(raw) == "" {
		return "", nil, nil
	}
	var content msgContent
	if err := json.Unmarshal([]byte(raw), &content); err == nil {
		text := content.Text
		if text == "" {
			text = textFromBlocks([]byte(raw))
		}
		return text, content.TextTags, content.SearchReferences
	}
	if text := textFromBlocks([]byte(raw)); text != "" {
		return text, nil, nil
	}
	return "", nil, nil
}

// extraContent 解析合并进来的接口版 content。
func (m message) extraContent() (string, []textTag, []searchReference) {
	return parseContentString(m.ExtraContent)
}

// textFromBlocks 从 block 数组里拼接 text_block 文本。
func textFromBlocks(raw []byte) string {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || raw[0] != '[' {
		return ""
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		var content struct {
			TextBlock *struct {
				Text string `json:"text"`
			} `json:"text_block"`
		}
		if err := json.Unmarshal(b.Content, &content); err != nil {
			continue
		}
		if content.TextBlock != nil && strings.TrimSpace(content.TextBlock.Text) != "" {
			parts = append(parts, strings.TrimSpace(content.TextBlock.Text))
		}
	}
	return strings.Join(parts, "\n\n")
}

// blocksFromMessage 返回消息的 block 列表,优先 content_block。
func blocksFromMessage(m message) []contentBlock {
	for _, raw := range [][]byte{m.ContentBlock, []byte(m.Content)} {
		trimmed := []byte(strings.TrimSpace(string(raw)))
		if len(trimmed) == 0 || trimmed[0] != '[' {
			continue
		}
		var blocks []contentBlock
		if err := json.Unmarshal(trimmed, &blocks); err == nil && len(blocks) > 0 {
			return blocks
		}
	}
	return nil
}

// parseReferences 提取一条消息里的网页参考来源(页面 SSR 与接口两边的都算)。
func parseReferences(m message) []Reference {
	_, _, refs := parseMessageContent(m)
	_, _, extra := m.extraContent()
	refs = append(refs, extra...)
	out := make([]Reference, 0, len(refs))
	for _, r := range refs {
		if r.TextCard == nil {
			continue
		}
		title := strings.TrimSpace(r.TextCard.Title)
		u := cleanURL(r.TextCard.URL)
		if u == "" {
			continue
		}
		if title == "" {
			title = u
		}
		out = append(out, Reference{Title: title, URL: u})
	}
	return out
}

// imageMode 决定一次分享里图片从哪种 block 结构提取。
type imageMode int

const (
	// imageModeRich 使用 rich_media_layout_block(页面上真正展示的图集)。
	imageModeRich imageMode = iota
	// imageModeCards 使用搜索工具返回的 image_card。
	imageModeCards
	// imageModeMeta 使用 text_block 的 meta_info 配图。
	imageModeMeta
)

// detectImageMode 判断整段对话该用哪种图片结构。
// 两种 block 同时存在时以 rich_media_layout_block 为准,避免同一批搜索图重复。
func detectImageMode(msgs []message) imageMode {
	hasCards := false
	for _, m := range msgs {
		for _, b := range blocksFromMessage(m) {
			switch b.BlockType {
			case 10084:
				return imageModeRich
			case 10025:
				if len(searchCardImages(b.Content)) > 0 {
					hasCards = true
				}
			}
		}
	}
	if hasCards {
		return imageModeCards
	}
	return imageModeMeta
}

// parseMessageImages 收集一条消息里的全部图片:搜索图片在前,AI 生图、上传图片在后。
func parseMessageImages(m message, mode imageMode) []Image {
	text, tags, _ := parseMessageContent(m)
	_, extraTags, _ := m.extraContent()
	tags = append(tags, extraTags...)
	var out []Image
	seen := map[string]bool{}
	seenURL := map[string]bool{}
	appendImg := func(img Image) {
		if img.URL == "" || seen[img.Key] || seenURL[img.URL] {
			return
		}
		seen[img.Key] = true
		seenURL[img.URL] = true
		out = append(out, img)
	}

	for _, tag := range tags {
		for _, img := range imagesFromTextTag(tag) {
			appendImg(img)
		}
	}
	for _, img := range blockImages(m, mode) {
		appendImg(img)
	}
	for _, img := range generatedImages(m.Content) {
		appendImg(img)
	}
	for _, img := range generatedImages(string(m.ContentBlock)) {
		appendImg(img)
	}
	// HTML 里的 content 本身就是 block 数组,图片已经由 blockImages 收集,
	// 再做一次附件扫描只会把同一批图片重复计算。
	for _, img := range attachedOnlyImages(m, text) {
		appendImg(img)
	}
	return out
}

// attachedOnlyImages 只在消息没有 block 时扫描附件图片。
func attachedOnlyImages(m message, text string) []Image {
	if len(blocksFromMessage(m)) > 0 {
		return nil
	}
	var out []Image
	for _, img := range attachedImages(m.Content) {
		// 正文里已经用 Markdown 引用过的图片不再追加,避免重复。
		if strings.Contains(text, img.URL) {
			continue
		}
		out = append(out, img)
	}
	return out
}

// imagesFromTextTag 解析 text_tag 里嵌套的图片列表。
func imagesFromTextTag(tag textTag) []Image {
	if strings.TrimSpace(tag.TagInfo) == "" {
		return nil
	}
	var info tagInfo
	if err := json.Unmarshal([]byte(tag.TagInfo), &info); err != nil || info.Value == "" {
		return nil
	}
	var value tagValue
	if err := json.Unmarshal([]byte(info.Value), &value); err != nil {
		return nil
	}
	out := make([]Image, 0, len(value.Images))
	for _, ti := range value.Images {
		u := ti.bestURL()
		if u == "" {
			continue
		}
		key := strings.TrimSpace(ti.URI)
		if key == "" {
			key = rawScalar(ti.ID)
		}
		// 同一张图在搜索卡片里有时是 labis/xxx,有时是 labis/image/xxx。
		key = strings.TrimPrefix(key, "labis/image/")
		key = strings.TrimPrefix(key, "labis/")
		if key == "" {
			key = u
		}
		out = append(out, Image{
			URL:     u,
			Caption: strings.TrimSpace(ti.ImgCaption),
			Kind:    "search",
			Key:     "img:" + key,
		})
	}
	return out
}

// rawScalar 把 JSON 标量(数字/字符串)转成去引号的字符串。
func rawScalar(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			return strings.TrimSpace(str)
		}
	}
	return strings.Trim(s, `"`)
}

// blockImages 按整段对话确定的结构收集图片。
func blockImages(m message, mode imageMode) []Image {
	blocks := blocksFromMessage(m)
	if len(blocks) == 0 {
		return nil
	}

	switch mode {
	case imageModeRich:
		var out []Image
		for _, b := range blocks {
			if b.BlockType == 10084 {
				out = append(out, richMediaImages(b.Content)...)
			}
		}
		return out
	case imageModeCards:
		var out []Image
		for _, b := range blocks {
			if b.BlockType == 10025 {
				out = append(out, searchCardImages(b.Content)...)
			}
		}
		return out
	default:
		var out []Image
		for _, b := range blocks {
			if b.BlockType != 10000 {
				continue
			}
			for _, item := range b.MetaInfo {
				out = append(out, tagInfoImages(item)...)
			}
		}
		return out
	}
}

// richMediaImages 解析 rich_media_layout_block.media 里的图片。
func richMediaImages(raw json.RawMessage) []Image {
	var v struct {
		RichMediaLayoutBlock struct {
			Media []json.RawMessage `json:"media"`
		} `json:"rich_media_layout_block"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	out := make([]Image, 0, len(v.RichMediaLayoutBlock.Media))
	for _, item := range v.RichMediaLayoutBlock.Media {
		var m map[string]any
		if err := json.Unmarshal(item, &m); err != nil {
			continue
		}
		if inner, ok := m["image"].(map[string]any); ok {
			if img, ok := imageFromMap(inner); ok {
				out = append(out, img)
			}
			continue
		}
		if img, ok := imageFromMap(m); ok {
			out = append(out, img)
		}
	}
	return out
}

// searchCardImages 解析 search_query_result_block.results[].image_card 里的图片。
func searchCardImages(raw json.RawMessage) []Image {
	var v struct {
		SearchQueryResultBlock struct {
			Results []struct {
				ImageCard map[string]any `json:"image_card"`
			} `json:"results"`
		} `json:"search_query_result_block"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	var out []Image
	for _, result := range v.SearchQueryResultBlock.Results {
		if result.ImageCard == nil {
			continue
		}
		if img, ok := imageFromMap(result.ImageCard); ok {
			out = append(out, img)
		}
	}
	return out
}

// tagInfoImages 解析 meta_info 里 tag_info 的 media / images 图片。
func tagInfoImages(raw json.RawMessage) []Image {
	var meta struct {
		TagInfo string `json:"tag_info"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil || strings.TrimSpace(meta.TagInfo) == "" {
		return nil
	}
	var v struct {
		Media []struct {
			Image map[string]any `json:"image"`
		} `json:"media"`
		Images []map[string]any `json:"images"`
	}
	if err := json.Unmarshal([]byte(meta.TagInfo), &v); err != nil {
		return nil
	}
	var out []Image
	for _, item := range v.Media {
		if img, ok := imageFromMap(item.Image); ok {
			out = append(out, img)
		}
	}
	for _, item := range v.Images {
		if img, ok := imageFromMap(item); ok {
			out = append(out, img)
		}
	}
	return out
}

// imageFromMap 把一个图片对象转成 Image,优先原图。
func imageFromMap(m map[string]any) (Image, bool) {
	candidates := urlsFromAny(m["full_size_url"])
	for _, key := range []string{"thumb_url", "origin_thumb_url", "uri", "url", "image_url"} {
		if s, ok := m[key].(string); ok {
			candidates = append(candidates, s)
		}
	}
	for _, raw := range candidates {
		u := cleanURL(raw)
		if u == "" {
			continue
		}
		return Image{
			URL:     u,
			Caption: imageCaption(m),
			Kind:    "search",
			Key:     "img:" + normalizeImageKey(m, u),
		}, true
	}
	return Image{}, false
}

// imageCaption 读取图片说明,没有时返回空串。
func imageCaption(m map[string]any) string {
	for _, key := range []string{"img_caption", "caption", "alt"} {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// normalizeImageKey 生成去重键,兼容 labis/xxx 与 labis/image/xxx 两种写法。
func normalizeImageKey(m map[string]any, fallbackURL string) string {
	for _, key := range []string{"item_id", "uri", "id"} {
		switch v := m[key].(type) {
		case string:
			s := strings.TrimSpace(v)
			if s == "" {
				continue
			}
			s = strings.TrimPrefix(s, "labis/image/")
			s = strings.TrimPrefix(s, "labis/")
			return s
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return fallbackURL
}

// urlsFromAny 把字符串 / 字符串数组转成 []string。
func urlsFromAny(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// bestURL 选择图片的最佳下载地址:优先原图,其次缩略图。
func (ti tagImage) bestURL() string {
	for _, u := range ti.fullSizeURLs() {
		if cleaned := cleanURL(u); cleaned != "" {
			return cleaned
		}
	}
	for _, u := range []string{ti.ThumbURL, ti.OriginThumbURL} {
		if cleaned := cleanURL(u); cleaned != "" {
			return cleaned
		}
	}
	return ""
}

// fullSizeURLs 兼容 full_size_url 为字符串数组或单个字符串两种写法。
func (ti tagImage) fullSizeURLs() []string {
	raw := strings.TrimSpace(string(ti.FullSizeURL))
	if raw == "" || raw == "null" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err == nil {
		return list
	}
	var one string
	if err := json.Unmarshal([]byte(raw), &one); err == nil {
		return []string{one}
	}
	return nil
}

// generatedImages 从消息 JSON 里提取 AI 生图 URL(rc_gen_image)。
func generatedImages(raw string) []Image {
	if raw == "" || !strings.Contains(raw, "rc_gen_image") {
		return nil
	}
	text := strings.ReplaceAll(raw, `\u002F`, "/")
	text = strings.ReplaceAll(text, `\u0026`, "&")
	text = strings.ReplaceAll(text, `\/`, "/")

	var out []Image
	seen := map[string]bool{}
	for _, u := range urlPattern.FindAllString(text, -1) {
		if !strings.Contains(u, "rc_gen_image") {
			continue
		}
		u = rawGeneratedURL(cleanURL(u))
		m := generatedImagePattern.FindStringSubmatch(u)
		if len(m) != 2 || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, Image{URL: u, Kind: "generated", Key: "gen:" + m[1]})
	}
	return out
}

// attachedImages 从消息 content JSON 里收集上传图片等非文本附件。
// 搜索引用(search_references)里的缩略图不算正文图片,这里跳过。
func attachedImages(raw string) []Image {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var root any
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return nil
	}
	var out []Image
	seen := map[string]bool{}
	add := func(rawURL string) {
		u := cleanURL(rawURL)
		// AI 生图由 generatedImages 统一处理(会换成无水印原图),这里跳过。
		if u == "" || strings.Contains(u, "rc_gen_image") || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, Image{URL: u, Kind: "attachment", Key: "att:" + u})
	}

	var walk func(v any, key string)
	walk = func(v any, key string) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				lk := strings.ToLower(k)
				if lk == "search_references" || lk == "multi_reference" {
					continue
				}
				if lk == "url" || lk == "origin_url" || lk == "download_url" || lk == "image_url" {
					if s, ok := child.(string); ok && looksLikeImageURL(s) {
						add(s)
					}
				}
				walk(child, lk)
			}
		case []any:
			for _, child := range x {
				walk(child, key)
			}
		case string:
			if (strings.Contains(key, "image") || strings.Contains(key, "url")) && looksLikeImageURL(x) {
				add(x)
			}
		}
	}
	walk(root, "")
	return out
}

// looksLikeImageURL 判断一个 URL 是否指向图片(豆包图床或常见图片扩展名)。
func looksLikeImageURL(raw string) bool {
	u := strings.ToLower(cleanURL(raw))
	if u == "" {
		return false
	}
	if strings.Contains(u, "byteimg.com") || strings.Contains(u, "doubaocdn.com") {
		return true
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".bmp"} {
		if strings.HasSuffix(u, ext) {
			return true
		}
	}
	return false
}

// rawGeneratedURL 把带水印/缩放模板的生图地址换成无水印原图。
func rawGeneratedURL(u string) string {
	if strings.Contains(u, "image_raw") {
		return u
	}
	return generatedTplvPattern.ReplaceAllString(u, "~tplv-a9rns2rl98-image_raw_b.png")
}

// cleanURL 清洗 URL:去 HTML 实体、补全协议、去掉首尾标点。
func cleanURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	u = strings.ReplaceAll(u, `\u002F`, "/")
	u = strings.ReplaceAll(u, `\u0026`, "&")
	u = strings.ReplaceAll(u, `\/`, "/")
	u = strings.ReplaceAll(u, "&amp;", "&")
	u = html.UnescapeString(u)
	u = strings.TrimRight(u, `.,;，。；）)`)
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return ""
	}
	return u
}

// renderMarkdown 生成最终的 Markdown。
func renderMarkdown(a *Article) string {
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(a.Title)
	b.WriteString("\n\n")

	meta := []string{"来源：豆包分享"}
	if a.Author != "" {
		meta = append(meta, "分享者："+a.Author)
	}
	if !a.CreatedAt.IsZero() {
		meta = append(meta, "分享时间："+a.CreatedAt.Local().Format("2006年01月02日 15:04"))
	}
	b.WriteString("> " + strings.Join(meta, " · ") + "\n")
	if a.SourceURL != "" {
		b.WriteString("> 链接：" + a.SourceURL + "\n")
	}
	b.WriteString("\n---\n\n")

	for i, msg := range a.Messages {
		role := msg.Role
		if role == "豆包" && a.BotName != "" {
			role = a.BotName
		}
		b.WriteString("## " + role + "\n\n")
		if text := strings.TrimSpace(msg.Text); text != "" {
			b.WriteString(demoteHeadings(text))
			b.WriteString("\n\n")
		}
		for _, img := range msg.Images {
			alt := strings.TrimSpace(img.Caption)
			if alt == "" {
				alt = fmt.Sprintf("图片%d", i+1)
			}
			b.WriteString("![" + mdAlt(alt) + "](" + img.URL + ")\n\n")
		}
	}

	if len(a.References) > 0 {
		b.WriteString("## 参考来源\n\n")
		for i, ref := range a.References {
			b.WriteString(fmt.Sprintf("%d. [%s](%s)\n", i+1, mdAlt(ref.Title), ref.URL))
		}
		if a.ReferencesOmitted > 0 {
			b.WriteString(fmt.Sprintf("\n> 其余 %d 条参考来源已省略。\n", a.ReferencesOmitted))
		}
		b.WriteString("\n")
	}

	return strings.TrimSpace(b.String()) + "\n"
}

// demoteHeadings 把消息正文里的标题降一级,避免和消息的 ## 标题冲突。
// 代码块里的 # 行保持原样。
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

// mdAlt 处理 Markdown 图片/链接的替代文字,避免破坏语法。
func mdAlt(s string) string {
	s = strings.ReplaceAll(s, "[", "［")
	s = strings.ReplaceAll(s, "]", "］")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
