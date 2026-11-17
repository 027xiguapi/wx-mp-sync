// Package naicha 把本地归档的文章同步到奶茶圈论坛(POST /api/posts)。
//
// 论坛接口的正文上限是 3000 字、图片最多 6 张。归档的公众号文章往往更长,
// 因此这里会做一次「节选 + 保留原文链接」的整理:正文图片从 Markdown 里
// 摘出来,作为图集通过 multipart 上传;正文只保留文字和原文出处。
package naicha

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// 与论坛 app/api/posts/route.ts 的校验保持一致。
	maxTitleUnits   = 80
	minTitleUnits   = 4
	maxContentUnits = 3000
	minContentUnits = 8
	maxTags         = 6
	maxTagUnits     = 12
	maxAuthorUnits  = 20
	maxImages       = 6
	maxImageBytes   = 5 << 20
	maxTotalBytes   = 28 << 20 // 留出 multipart 开销,低于论坛 32MB 上限
	maxSourceURL    = 900      // 超长链接会挤掉正文,直接省略
)

var validCategories = map[string]bool{
	"group":      true,
	"rebate":     true,
	"promo":      true,
	"discussion": true,
	"poll":       true,
	"choose":     true,
	"lottery":    true,
	"notice":     true,
}

// imageExt 是论坛 post-images 允许的四种格式。
var imageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// Article 是从归档目录读出的文章。
type Article struct {
	Dir         string
	Title       string
	Author      string // 公众号名
	Creator     string // 原文作者
	URL         string // 原文链接
	PublishTime string
	Markdown    string
}

// Post 是准备提交给论坛的帖子。
type Post struct {
	Title     string
	Content   string
	Category  string
	Tags      []string
	Author    string
	Images    []PostImage
	Source    string
	Truncated bool
}

// PostImage 是一张要上传的图片;URL 非空时表示直接引用外链。
type PostImage struct {
	URL      string
	Data     []byte
	Filename string
}

// LoadArticle 读取归档目录里的 metadata.json 和 Markdown 正文。
func LoadArticle(dir string) (*Article, error) {
	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("读取文章目录失败: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("不是文章目录: %s", dir)
	}

	var meta struct {
		Title       string `json:"title"`
		Author      string `json:"author"`
		Creator     string `json:"creator"`
		URL         string `json:"url"`
		Link        string `json:"link"`
		SourceURL   string `json:"source_url"`
		Markdown    string `json:"markdown"`
		Content     string `json:"content"`
		PublishTime string `json:"publish_time"`
		PublishedAt string `json:"published_at"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, "metadata.json")); err == nil {
		_ = json.Unmarshal(data, &meta)
	}

	mdName := strings.TrimSpace(meta.Markdown)
	if mdName == "" {
		mdName = findMarkdown(dir)
	}
	if mdName == "" {
		return nil, fmt.Errorf("归档里没有 Markdown 正文")
	}

	mdPath := mdName
	if !filepath.IsAbs(mdPath) {
		mdPath = filepath.Join(dir, filepath.FromSlash(mdName))
	}
	mdPath = filepath.Clean(mdPath)
	if rel, err := filepath.Rel(dir, mdPath); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("Markdown 路径越界: %s", mdName)
	}
	f, err := os.Open(mdPath)
	if err != nil {
		return nil, fmt.Errorf("打开 Markdown 失败: %w", err)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 Markdown 失败: %w", err)
	}
	markdown := string(body)
	if strings.TrimSpace(markdown) == "" {
		markdown = strings.TrimSpace(meta.Content)
	}
	if strings.TrimSpace(markdown) == "" {
		return nil, fmt.Errorf("Markdown 正文为空")
	}

	title := strings.TrimSpace(meta.Title)
	if title == "" {
		title = filepath.Base(dir)
	}
	return &Article{
		Dir:         dir,
		Title:       title,
		Author:      strings.TrimSpace(meta.Author),
		Creator:     strings.TrimSpace(meta.Creator),
		URL:         firstNonEmpty(meta.URL, meta.Link, meta.SourceURL),
		PublishTime: firstNonEmpty(meta.PublishTime, meta.PublishedAt),
		Markdown:    markdown,
	}, nil
}

// findMarkdown 在目录根部找第一个 .md 文件。
func findMarkdown(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			return e.Name()
		}
	}
	return ""
}

// PreparePost 把文章整理成论坛接口可以接受的帖子。
func PreparePost(article *Article, cfg Config) (*Post, error) {
	if article == nil {
		return nil, fmt.Errorf("文章为空")
	}
	if strings.TrimSpace(article.Markdown) == "" {
		return nil, fmt.Errorf("Markdown 正文为空")
	}

	body, refs := stripImages(article.Markdown)
	body = cleanMarkdown(body)
	if body == "" {
		body = "（正文为空，请查看原文。）"
	}

	images := collectImages(article.Dir, refs)
	images = prependCover(images, article.Dir)
	if len(images) > maxImages {
		images = images[:maxImages]
	}

	title := normalizeTitle(article.Title)
	author := normalizeAuthor(article.Creator, article.Author, cfg.Author)
	publish := strings.TrimSpace(article.PublishTime)
	source := canonicalSourceURL(article.URL)
	footer := buildFooter(author, publish, source, title, false, 0)

	content, truncated := composeContent(body, footer, author, publish, source, title)
	if truncated {
		footer = buildFooter(author, publish, source, title, true, textLen(body))
		content, _ = composeContent(body, footer, author, publish, source, title)
	}
	if textLen(content) < minContentUnits {
		content += "\n\n（全文完）"
	}

	return &Post{
		Title:     title,
		Content:   content,
		Category:  normalizeCategory(cfg.Category),
		Tags:      normalizeTags(cfg.Tags),
		Author:    author,
		Images:    images,
		Source:    source,
		Truncated: truncated,
	}, nil
}

// composeContent 拼接正文与出处,超长时优先切正文。
func composeContent(body, footer, author, publish, source, title string) (string, bool) {
	if textLen(body)+textLen(footer) <= maxContentUnits {
		return body + footer, false
	}

	ellipsis := "\n\n…"
	allowed := maxContentUnits - textLen(footer) - textLen(ellipsis)
	if allowed < 80 {
		// 出处太长(通常是原文链接),退回只保留来源信息。
		footer = buildFooter(author, publish, "", title, true, textLen(body))
		allowed = maxContentUnits - textLen(footer) - textLen(ellipsis)
	}
	if allowed < 20 {
		allowed = 20
	}

	cut := cutMarkdown(body, allowed)
	if cut == "" {
		cut = truncateText(body, allowed)
	}
	return cut + ellipsis + footer, true
}

// buildFooter 生成正文末尾的出处块。truncated 为 true 时说明这是节选。
func buildFooter(author, publish, source, title string, truncated bool, originalUnits int) string {
	if author == "" && publish == "" && source == "" && !truncated {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n---\n\n")
	if truncated {
		if originalUnits > 0 {
			b.WriteString(fmt.Sprintf("> 本文为节选（原文约 %d 字），完整内容请见原文。\n>\n", originalUnits))
		} else {
			b.WriteString("> 本文为节选，完整内容请见原文。\n>\n")
		}
	}
	if author != "" || publish != "" {
		b.WriteString("> 来源：")
		if author != "" {
			b.WriteString("公众号「")
			b.WriteString(author)
			b.WriteString("」")
		}
		if publish != "" {
			if author != "" {
				b.WriteString(" · ")
			}
			b.WriteString(publish)
		}
		b.WriteString("\n")
	}
	if source != "" {
		b.WriteString("> 原文：[")
		b.WriteString(escapeMarkdownText(title))
		b.WriteString("](")
		b.WriteString(source)
		b.WriteString(")\n")
	}
	return b.String()
}

// normalizeTitle 清理标题并满足论坛的 4–80 字限制。
func normalizeTitle(raw string) string {
	title := strings.Join(strings.Fields(raw), " ")
	if title == "" {
		title = "公众号文章"
	}
	if textLen(title) > maxTitleUnits {
		title = truncateText(title, maxTitleUnits-1) + "…"
	}
	if textLen(title) < minTitleUnits {
		title += " 文章"
	}
	if textLen(title) < minTitleUnits {
		title = "公众号文章"
	}
	return title
}

// normalizeAuthor 选择发帖昵称:优先原文作者,其次公众号名,最后配置值。
func normalizeAuthor(creator, account, configured string) string {
	author := firstNonEmpty(creator, account, configured)
	author = strings.Join(strings.Fields(author), " ")
	if author == "" {
		author = "茶友"
	}
	author = strings.TrimSpace(truncateText(author, maxAuthorUnits))
	if author == "" {
		author = "茶友"
	}
	return author
}

// normalizeTags 去重、截断并限制标签数量。
func normalizeTags(raw []string) []string {
	if len(raw) == 0 {
		raw = []string{"公众号"}
	}
	out := make([]string, 0, maxTags)
	seen := make(map[string]bool, maxTags)
	for _, tag := range raw {
		tag = strings.TrimSpace(tag)
		tag = strings.TrimPrefix(tag, "#")
		// 论坛会用逗号/空白拆分标签,所以标签内部不能留空格或逗号。
		tag = strings.Join(strings.Fields(tag), "")
		tag = strings.NewReplacer(",", "", "，", "").Replace(tag)
		if tag == "" {
			continue
		}
		if textLen(tag) > maxTagUnits {
			tag = strings.TrimSpace(truncateText(tag, maxTagUnits-1) + "…")
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
		if len(out) == maxTags {
			break
		}
	}
	return out
}

// normalizeCategory 只允许论坛支持的非优惠券分类。
func normalizeCategory(raw string) string {
	category := strings.ToLower(strings.TrimSpace(raw))
	if validCategories[category] {
		return category
	}
	return "discussion"
}

// canonicalSourceURL 把微信文章的超长追踪链接压成可访问的短参数形式。
func canonicalSourceURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if strings.EqualFold(u.Hostname(), "mp.weixin.qq.com") {
		q := u.Query()
		biz, mid, idx, sn := q.Get("__biz"), q.Get("mid"), q.Get("idx"), q.Get("sn")
		if biz != "" && mid != "" && idx != "" && sn != "" {
			v := url.Values{}
			v.Set("__biz", biz)
			v.Set("mid", mid)
			v.Set("idx", idx)
			v.Set("sn", sn)
			return "https://mp.weixin.qq.com/s?" + v.Encode()
		}
	}
	if len(u.String()) > maxSourceURL {
		return ""
	}
	return u.String()
}

// ---------- 正文图片 ----------

var (
	markdownImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(\s*(?:<([^>]+)>|([^)\s]+))(?:\s+["'][^"']*["'])?\s*\)`)
	htmlImageRe     = regexp.MustCompile(`(?is)<img\b[^>]*\bsrc\s*=\s*["']([^"']+)["'][^>]*>`)
)

type imageRef struct {
	URL string // 外链
	Alt string
}

// stripImages 把 Markdown/HTML 里的图片引用从正文中摘掉,并返回图片列表。
func stripImages(markdown string) (string, []imageRef) {
	var refs []imageRef
	seen := map[string]bool{}

	add := func(raw, alt string) {
		raw = strings.TrimSpace(html.UnescapeString(raw))
		if raw == "" || seen[raw] {
			return
		}
		seen[raw] = true
		refs = append(refs, imageRef{URL: raw, Alt: strings.TrimSpace(alt)})
	}

	body := markdownImageRe.ReplaceAllStringFunc(markdown, func(m string) string {
		sub := markdownImageRe.FindStringSubmatch(m)
		if len(sub) < 4 {
			return ""
		}
		add(firstNonEmpty(sub[2], sub[3]), sub[1])
		return ""
	})
	body = htmlImageRe.ReplaceAllStringFunc(body, func(m string) string {
		sub := htmlImageRe.FindStringSubmatch(m)
		if len(sub) >= 2 {
			add(sub[1], "")
		}
		return ""
	})
	return body, refs
}

// collectImages 把图片引用加载成本地文件或外链,最多 6 张。
func collectImages(dir string, refs []imageRef) []PostImage {
	images := make([]PostImage, 0, maxImages)
	total := 0
	for _, ref := range refs {
		if len(images) >= maxImages {
			break
		}
		if isHTTPURL(ref.URL) {
			if len(ref.URL) > 500 {
				continue
			}
			images = append(images, PostImage{URL: ref.URL})
			continue
		}
		data, ext, ok := readLocalImage(dir, ref.URL)
		if !ok {
			continue
		}
		if len(data) > maxImageBytes || total+len(data) > maxTotalBytes {
			continue
		}
		total += len(data)
		images = append(images, PostImage{Data: data, Filename: fmt.Sprintf("image-%02d%s", len(images)+1, ext)})
	}
	return images
}

// prependCover 把归档目录根部的封面图放到图集第一张(不重复、仍受 6 张限制)。
func prependCover(images []PostImage, dir string) []PostImage {
	cover, ok := findCover(dir)
	if !ok {
		if len(images) > 0 {
			return images
		}
		// 没有正文图时,尝试用 images/ 目录里的第一张图。
		cover, ok = findFirstImage(filepath.Join(dir, "images"))
		if !ok {
			return images
		}
	}
	for _, img := range images {
		if img.URL == "" && img.Filename != "" && bytesEqual(img.Data, cover.Data) {
			return images
		}
	}
	if len(cover.Data) > maxImageBytes {
		return images
	}
	out := make([]PostImage, 0, maxImages)
	out = append(out, cover)
	out = append(out, images...)
	if len(out) > maxImages {
		out = out[:maxImages]
	}
	return out
}

// findCover 在归档根目录找一张图片作为封面。
func findCover(dir string) (PostImage, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return PostImage{}, false
	}
	names := make([]string, 0, 4)
	for _, e := range entries {
		if e.IsDir() || !isImageName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		data, ext, ok := readLocalImage(dir, name)
		if ok && len(data) <= maxImageBytes {
			return PostImage{Data: data, Filename: "cover" + ext}, true
		}
	}
	return PostImage{}, false
}

// findFirstImage 在给定目录里找第一张支持的图片。
func findFirstImage(dir string) (PostImage, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return PostImage{}, false
	}
	for _, e := range entries {
		if e.IsDir() || !isImageName(e.Name()) {
			continue
		}
		data, ext, ok := readLocalImage(dir, e.Name())
		if ok && len(data) <= maxImageBytes {
			return PostImage{Data: data, Filename: "cover" + ext}, true
		}
	}
	return PostImage{}, false
}

// readLocalImage 读取并校验一张归档内的图片,返回内容和扩展名。
func readLocalImage(dir, ref string) ([]byte, string, bool) {
	path, ok := resolveLocalPath(dir, ref)
	if !ok {
		return nil, "", false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, "", false
	}
	ctype := http.DetectContentType(data)
	ext, ok := imageExt[ctype]
	if !ok {
		// 少数 WebP 文件头可能识别不出来,退回扩展名判断,但必须仍在白名单内。
		ext = strings.ToLower(filepath.Ext(path))
		if !isImageExt(ext) {
			return nil, "", false
		}
	}
	return data, ext, true
}

// resolveLocalPath 把 Markdown 里的相对路径解析到归档目录内,拒绝越界。
func resolveLocalPath(dir, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(strings.ToLower(ref), "data:") {
		return "", false
	}
	if i := strings.IndexAny(ref, "?#"); i >= 0 {
		ref = ref[:i]
	}
	if unescaped, err := url.PathUnescape(ref); err == nil {
		ref = unescaped
	}
	ref = strings.ReplaceAll(ref, "\\", "/")
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, "/") {
		return "", false
	}
	path := filepath.Clean(filepath.Join(dir, filepath.FromSlash(ref)))
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return path, true
}

// cleanMarkdown 折叠多余空行并去掉首尾空白。
func cleanMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = regexp.MustCompile(`\n{3,}`).ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// cutMarkdown 尽量在段落/句子边界截断,并避免留下未闭合的链接。
func cutMarkdown(body string, max int) string {
	if max <= 0 {
		return ""
	}
	cut := truncateText(body, max)
	if cut == "" {
		return ""
	}
	for _, sep := range []string{"\n\n", "\n", "。", "！", "？", "；", "，", " "} {
		if i := strings.LastIndex(cut, sep); i >= len(cut)/2 {
			cut = cut[:i+len(sep)]
			break
		}
	}
	cut = strings.TrimRight(cut, " \t\n")
	// 如果截断点落在 Markdown 链接的 [..](..) 中间,退回到 "[" 之前。
	if strings.Count(cut, "[") > strings.Count(cut, "]") {
		if i := strings.LastIndex(cut, "["); i >= 0 {
			cut = strings.TrimRight(cut[:i], " \t\n")
		}
	}
	return cut
}

// textLen 返回 JavaScript 字符串长度(UTF-16 码元),和论坛的校验口径一致。
func textLen(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// truncateText 按 UTF-16 码元截断字符串。
func truncateText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if textLen(s) <= max {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for _, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if n+w > max {
			break
		}
		b.WriteRune(r)
		n += w
	}
	return b.String()
}

func escapeMarkdownText(s string) string {
	return strings.NewReplacer("[", `\[`, "]", `\]`).Replace(s)
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func isImageName(name string) bool {
	return isImageExt(strings.ToLower(filepath.Ext(name)))
}

func isImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	}
	return false
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
