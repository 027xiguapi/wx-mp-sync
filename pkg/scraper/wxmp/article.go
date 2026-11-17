package wxmp

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

var md_convert = htmltomarkdown.NewConverter("", true, nil)

// OfficialAccountDownload 提供公众号文章的抓取与归档能力。
type OfficialAccountDownload struct {
	article    *WechatOfficialArticle
	OnProgress func(downloaded int64) // 每下载一批字节后回调,用于进度展示
}

// reportProgress 触发下载进度回调。
func (c *OfficialAccountDownload) reportProgress(n int64) {
	if c.OnProgress != nil {
		c.OnProgress(n)
	}
}

// Scrape 请求文章页并返回原始 HTML。
func (c *OfficialAccountDownload) Scrape(url string) ([]byte, error) {
	if url == "" {
		return nil, fmt.Errorf("url is empty")
	}
	client := &http.Client{Timeout: 25 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	setWechatHeaders(req, "")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("bad status: %s body=%s", resp.Status, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if isVerificationPage(string(body)) {
		return nil, fmt.Errorf("wechat verification page returned for %s", url)
	}
	return body, nil
}

// FetchArticle 抓取并解析单篇文章。
func (c *OfficialAccountDownload) FetchArticle(url string) (*WechatOfficialArticle, error) {
	content, err := c.Scrape(url)
	if err != nil {
		return nil, err
	}
	contentStr := string(content)
	// 从页面脚本提取发布时间
	var publishTimeStr string
	re := regexp.MustCompile(`var\s+createTime\s*=\s*'([^']+)'`)
	matches := re.FindStringSubmatch(contentStr)
	if len(matches) > 1 {
		publishTimeStr = formatPublishTime(matches[1], 0)
	}
	data, err := parse_cgi_datanew(contentStr)
	if err != nil {
		article, fallbackErr := parseArticleFromDOM(contentStr)
		if fallbackErr != nil {
			return nil, err
		}
		if isEmptyArticle(article) {
			return nil, fmt.Errorf("未能解析文章内容(可能需要在微信中打开,或链接已失效): %s", url)
		}
		c.article = article
		return article, nil
	}
	if publishTimeStr == "" {
		publishTimeStr = formatPublishTime(data.CreateTime, int(data.OriCreateTime))
	}
	article := newWechatOfficialArticle(data, publishTimeStr)
	if isEmptyArticle(article) {
		return nil, fmt.Errorf("未能解析文章内容(可能需要在微信中打开,或链接已失效): %s", url)
	}
	article.PageJSON = data
	article.PageHTML = contentStr
	c.article = article
	return article, nil
}

// isEmptyArticle 判断解析结果是否为"空文章"(标题、正文、图片全部缺失)。
// 常见于微信的验证页、参数错误页或已删除文章。
func isEmptyArticle(article *WechatOfficialArticle) bool {
	if article == nil {
		return true
	}
	if strings.TrimSpace(article.Title) != "" || strings.TrimSpace(article.Content) != "" {
		return false
	}
	return len(article.Images) == 0 && len(article.PicturePageInfoList) == 0
}

// newWechatOfficialArticle 由 CgiDataNew 构建统一文章结构。
func newWechatOfficialArticle(data *CgiDataNew, publishTimeStr string) *WechatOfficialArticle {
	if data == nil {
		return &WechatOfficialArticle{}
	}
	article := &WechatOfficialArticle{
		Type:                data.PageType,
		Title:               data.Title,
		Content:             data.ContentNoEncode,
		PublishTimeStr:      publishTimeStr,
		ContentLength:       len(data.ContentNoEncode),
		Creator:             data.Author,
		AuthorNickname:      data.NickName,
		AuthorAvatar:        firstNonEmpty(data.RoundHeadImg, data.OriHeadImgURL, data.HdHeadImg),
		AuthorID:            data.UserName,
		Images:              make([]string, 0),
		Videos:              data.VideoPageInfos,
		PicturePageInfoList: data.PicturePageInfoList,
	}
	for _, item := range data.PicturePageInfoList {
		if item.CdnURL != "" {
			article.Images = append(article.Images, item.CdnURL)
		}
	}
	return article
}

// NewArticleFromCgiData 由 CgiDataNew 构建统一文章结构(供注入保存路径复用)。
func NewArticleFromCgiData(data *CgiDataNew, publishTimeStr string) *WechatOfficialArticle {
	article := newWechatOfficialArticle(data, publishTimeStr)
	if data != nil {
		article.PageJSON = data
	}
	return article
}

// FormatArticlePublishTime 从文章数据格式化发布时间字符串。
func FormatArticlePublishTime(data *CgiDataNew) string {
	if data == nil {
		return ""
	}
	return formatPublishTime(data.CreateTime, int(data.OriCreateTime))
}

// picturePageInfoLivePhotoURL 返回图集中 live photo 的第一个视频 URL,无则空。
func picturePageInfoLivePhotoURL(item PicturePageInfo) string {
	for _, format := range item.LivePhoto.FormatInfo {
		if videoURL := strings.TrimSpace(format.URL); videoURL != "" {
			return videoURL
		}
	}
	return ""
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// downloadImage 下载图片到 saveDir,返回文件名(URL md5 + 扩展名)。已存在则跳过。
func (c *OfficialAccountDownload) downloadImage(imgURL, saveDir string) (string, error) {
	imgURL = normalizeMediaURL(imgURL)
	hash := md5.Sum([]byte(imgURL))
	hashStr := hex.EncodeToString(hash[:])

	ext := ".jpg"
	switch {
	case strings.Contains(imgURL, "wx_fmt=png"):
		ext = ".png"
	case strings.Contains(imgURL, "wx_fmt=gif"):
		ext = ".gif"
	case strings.Contains(imgURL, "wx_fmt=jpeg"):
		ext = ".jpg"
	case strings.Contains(imgURL, "wx_fmt=webp"):
		ext = ".webp"
	default:
		if u, err := url.Parse(imgURL); err == nil {
			if pathExt := filepath.Ext(u.Path); pathExt != "" {
				ext = pathExt
			}
		}
	}

	filename := hashStr + ext
	filePath := filepath.Join(saveDir, filename)

	if _, err := os.Stat(filePath); err == nil {
		return filename, nil
	}

	client := &http.Client{Timeout: 25 * time.Second}
	req, err := http.NewRequest("GET", imgURL, nil)
	if err != nil {
		return "", err
	}
	setWechatHeaders(req, "https://mp.weixin.qq.com/")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status: %s", resp.Status)
	}

	file, err := os.Create(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	n, err := io.Copy(file, resp.Body)
	if err != nil {
		return "", err
	}
	c.reportProgress(n)
	return filename, nil
}

// DownloadFile 下载任意媒体 URL 到 dir,返回文件名(URL md5 + 扩展名)。已存在则跳过。
func (c *OfficialAccountDownload) DownloadFile(rawURL, dir string) (string, error) {
	return c.downloadImage(rawURL, dir)
}

// DownloadBytes 下载任意媒体 URL 返回字节与 Content-Type(用于图床转发)。
func (c *OfficialAccountDownload) DownloadBytes(rawURL string) ([]byte, string, error) {
	return downloadImageBytes(rawURL)
}

// rewriteContentMedia 把正文中的 mp-common-mpaudio 音频卡片与视频 iframe 替换为可直接播放的标签。
func rewriteContentMedia(doc *goquery.Document, article *WechatOfficialArticle) {
	doc.Find("mp-common-mpaudio").Each(func(i int, s *goquery.Selection) {
		voiceEncodeFileId := s.AttrOr("voice_encode_fileid", "")
		if voiceEncodeFileId != "" {
			audioURL := "https://res.wx.qq.com/voice/getvoice?mediaid=" + voiceEncodeFileId
			s.AppendHtml(fmt.Sprintf(`<audio src="%s" controls="controls"></audio>`, audioURL))
		}
	})

	doc.Find("iframe.video_iframe").Each(func(i int, s *goquery.Selection) {
		vid := firstNonEmpty(s.AttrOr("data-vid", ""), s.AttrOr("vid", ""), s.AttrOr("data-mpvid", ""))
		if vid == "" {
			return
		}
		for _, video := range article.Videos {
			if video.VideoID != vid {
				continue
			}
			if len(video.MpVideoTransInfo) == 0 {
				continue
			}
			videoURL := video.MpVideoTransInfo[0].URL
			posterAttr := ""
			if cover := s.AttrOr("data-cover", ""); cover != "" {
				if decodedCover, err := url.QueryUnescape(cover); err == nil {
					cover = decodedCover
				}
				posterAttr = fmt.Sprintf(` poster="%s"`, escapeHTML(cover))
			}
			videoHTML := fmt.Sprintf(`<video src="%s"%s controls="controls" style="width: 100%%; height: auto;"></video>`, videoURL, posterAttr)
			s.ReplaceWithHtml(videoHTML)
			break
		}
	})
}

// convertMarkdownString 把(已做换行占位处理的)HTML 转为 Markdown,恢复硬换行。
func convertMarkdownString(newHTML string) (string, error) {
	brPlaceholder := "WECHATBRHOLDER"
	newHTML = strings.ReplaceAll(newHTML, "WECHATNEWLINEHOLDER", brPlaceholder)
	newHTML = strings.ReplaceAll(newHTML, "<br/>", brPlaceholder)
	newHTML = strings.ReplaceAll(newHTML, "<br>", brPlaceholder)
	newHTML = strings.ReplaceAll(newHTML, "<br />", brPlaceholder)

	markdown, err := md_convert.ConvertString(newHTML)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(markdown, brPlaceholder, "  \n"), nil
}

// ArticleImage 描述文章正文里的一张图片及其 alt 文本。
type ArticleImage struct {
	URL string
	Alt string
}

// MarkdownWithRemoteImages 生成文章的 Markdown 文本,正文图片保留原 URL(data-src→src),
// 并返回正文用到的图片列表(含 alt)。用于推送/同步到外部服务,不下载图片到本地。
func (c *OfficialAccountDownload) MarkdownWithRemoteImages(article *WechatOfficialArticle) (string, []ArticleImage, error) {
	if article == nil {
		return "", nil, fmt.Errorf("article is nil")
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(article.Content))
	if err != nil {
		return "", nil, err
	}

	replaceNewlinesInTree(doc.Nodes, "WECHATNEWLINEHOLDER")
	rewriteContentMedia(doc, article)

	var images []ArticleImage
	seen := map[string]bool{}
	doc.Find("img").Each(func(i int, s *goquery.Selection) {
		imgURL := s.AttrOr("data-src", "")
		if imgURL == "" {
			imgURL = s.AttrOr("src", "")
		}
		imgURL = normalizeMediaURL(imgURL)
		if imgURL == "" {
			return
		}
		s.SetAttr("src", imgURL)
		s.RemoveAttr("data-src")
		if !seen[imgURL] {
			seen[imgURL] = true
			images = append(images, ArticleImage{URL: imgURL, Alt: strings.TrimSpace(s.AttrOr("alt", ""))})
		}
	})

	newHTML, err := doc.Html()
	if err != nil {
		return "", nil, err
	}
	markdown, err := convertMarkdownString(newHTML)
	if err != nil {
		return "", nil, err
	}

	// 附加图集/附属图片
	for _, imgURL := range article.Images {
		imgURL = normalizeMediaURL(imgURL)
		if imgURL == "" || seen[imgURL] {
			continue
		}
		seen[imgURL] = true
		images = append(images, ArticleImage{URL: imgURL})
		markdown += fmt.Sprintf("\n![image](%s)\n", imgURL)
	}

	return markdown, images, nil
}

// SaveArticle 把文章归档到 dirPath,生成 index.html、Markdown 与 images/ 本地图片。
// 返回生成的 HTML 文件名与 Markdown 文件名。
func (c *OfficialAccountDownload) SaveArticle(article *WechatOfficialArticle, dirPath string) (htmlName, mdName string, err error) {
	if article == nil {
		return "", "", fmt.Errorf("article is nil")
	}
	imagesDirPath := filepath.Join(dirPath, "images")
	if err := os.MkdirAll(imagesDirPath, 0755); err != nil {
		return "", "", err
	}

	// 标题净化成合法文件名
	title := strings.TrimSpace(article.Title)
	if title == "" {
		title = "untitled"
	}
	mdName = sanitizeFilename(title) + ".md"
	htmlName = "index.html"

	// 处理正文 HTML:换行占位、音频、视频、图片本地化
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(article.Content))
	if err != nil {
		return "", "", err
	}

	newlinePlaceholder := "WECHATNEWLINEHOLDER"
	replaceNewlinesInTree(doc.Nodes, newlinePlaceholder)
	rewriteContentMedia(doc, article)

	doc.Find("img").Each(func(i int, s *goquery.Selection) {
		imgURL := s.AttrOr("data-src", "")
		if imgURL == "" {
			imgURL = s.AttrOr("src", "")
		}
		imgURL = normalizeMediaURL(imgURL)
		if imgURL == "" {
			return
		}
		localFileName, err := c.downloadImage(imgURL, imagesDirPath)
		if err == nil {
			s.SetAttr("src", "images/"+localFileName)
			s.RemoveAttr("data-src")
		} else {
			fmt.Printf("Failed to download image %s: %v\n", imgURL, err)
		}
	})

	// 生成 HTML
	bodyHTML, err := doc.Find("body").Html()
	if err != nil {
		bodyHTML = article.Content
	}
	bodyHTML = strings.ReplaceAll(bodyHTML, newlinePlaceholder, "<br>")

	// 图片型文章(type=2):正文里的图片在 PicturePageInfoList 中,追加到 HTML 正文
	for _, pic := range article.PicturePageInfoList {
		if imgURL := normalizeMediaURL(pic.CdnURL); imgURL != "" {
			if filename, err := c.downloadImage(imgURL, imagesDirPath); err == nil {
				bodyHTML += `<img src="images/` + filename + `" alt="" style="max-width:100%;height:auto;display:block;margin:12px 0;">`
			} else {
				fmt.Printf("Failed to download picture image %s: %v\n", imgURL, err)
			}
		}
		if videoURL := picturePageInfoLivePhotoURL(pic); videoURL != "" {
			bodyHTML += `<video src="` + escapeHTML(videoURL) + `" controls="controls" playsinline style="width:100%;height:auto;display:block;margin:12px 0;"></video>`
		}
	}

	htmlOut, err := c.buildHTMLPage(article, bodyHTML, imagesDirPath)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dirPath, htmlName), []byte(htmlOut), 0644); err != nil {
		return "", "", err
	}

	// 生成 Markdown
	newHTML, err := doc.Html()
	if err != nil {
		return "", "", err
	}
	markdown, err := c.convertToMarkdown(article, newHTML, imagesDirPath)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dirPath, mdName), []byte(markdown), 0644); err != nil {
		return "", "", err
	}

	return htmlName, mdName, nil
}

// replaceNewlinesInTree 递归把文本节点中的换行替换为占位符,保留 pre/code/script/style 原文。
func replaceNewlinesInTree(nodes []*html.Node, placeholder string) {
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.TextNode {
			if strings.Contains(n.Data, "\n") {
				n.Data = strings.ReplaceAll(n.Data, "\n", placeholder)
			}
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode {
				tag := strings.ToLower(child.Data)
				if tag == "pre" || tag == "code" || tag == "script" || tag == "style" {
					continue
				}
			}
			visit(child)
		}
	}
	for _, n := range nodes {
		visit(n)
	}
}

// buildHTMLPage 组装完整的归档 HTML 页面。
func (c *OfficialAccountDownload) buildHTMLPage(article *WechatOfficialArticle, bodyHTML, imagesDirPath string) (string, error) {
	var avatarTag string
	if article.AuthorAvatar != "" {
		if filename, err := c.downloadImage(article.AuthorAvatar, imagesDirPath); err == nil {
			avatarTag = `<img class="rich_media_meta_avatar" src="images/` + filename + `" alt="` + escapeHTML(article.AuthorNickname) + `">`
		}
	}

	creatorHTML := ""
	if article.Creator != "" {
		creatorHTML = `<span class="rich_media_meta rich_media_meta_text">` + escapeHTML(article.Creator) + `</span>`
	}

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>`)
	b.WriteString(escapeHTML(article.Title))
	b.WriteString(`</title>
<style>
    html { height: 100%; }
    body {
        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
        line-height: 1.6;
        max-width: 677px;
        margin: 0 auto;
        padding: 20px;
        color: #333;
        background: #fff;
    }
    h1 { font-size: 1.6em; margin-bottom: 0.5em; }
    .rich_media_meta_list { margin-bottom: 22px; line-height: 20px; font-size: 0; word-wrap: break-word; }
    .rich_media_meta { display: inline-block; vertical-align: middle; margin: 0 10px 10px 0; font-size: 15px; }
    .rich_media_meta_avatar { display: inline-block; width: 24px; height: 24px; border-radius: 50%; object-fit: cover; vertical-align: middle; margin: 0 8px 10px 0; }
    .rich_media_meta_nickname { color: #576b95; }
    .rich_media_meta_text { color: #666; }
    .rich_media_content img { max-width: 100%; height: auto; }
    .audio_card { display: flex; align-items: center; background: #f7f7f7; border-radius: 6px; padding: 12px; margin: 20px 0; }
    .audio_card_cover { width: 64px; height: 64px; border-radius: 4px; overflow: hidden; flex-shrink: 0; margin-right: 12px; }
    .audio_card_cover img { width: 100%; height: 100%; object-fit: cover; display: block; }
    .audio_card_content { flex-grow: 1; overflow: hidden; }
    .audio_card_title { font-size: 15px; font-weight: 500; color: #333; margin-bottom: 6px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
    .audio_card audio { height: 32px; width: 100%; }
    video { width: 100%; height: auto; }
</style>
</head>
<body>
<h1 class="rich_media_title"><span>`)
	b.WriteString(escapeHTML(article.Title))
	b.WriteString(`</span></h1>
<div class="rich_media_meta_list">`)
	b.WriteString(avatarTag)
	b.WriteString(creatorHTML)
	b.WriteString(`<span class="rich_media_meta rich_media_meta_nickname">` + escapeHTML(article.AuthorNickname) + `</span><span><em class="rich_media_meta rich_media_meta_text">` + escapeHTML(article.PublishTimeStr) + `</em></span></div>
<div class="rich_media_content js_underline_content">`)
	b.WriteString(bodyHTML)
	b.WriteString(`</div>
</body>
</html>`)
	return b.String(), nil
}

// convertToMarkdown 把已本地化图片的 HTML 转换为 Markdown,并附加图集图片。
func (c *OfficialAccountDownload) convertToMarkdown(article *WechatOfficialArticle, newHTML, imagesDirPath string) (string, error) {
	markdown, err := convertMarkdownString(newHTML)
	if err != nil {
		return "", err
	}

	// 附加图集/附属图片
	for _, imgURL := range article.Images {
		imgURL = normalizeMediaURL(imgURL)
		localFileName, err := c.downloadImage(imgURL, imagesDirPath)
		if err != nil {
			fmt.Printf("Failed to download attached image %s: %v\n", imgURL, err)
			continue
		}
		markdown += fmt.Sprintf("\n![image](images/%s)\n", localFileName)
	}

	return markdown, nil
}

// sanitizeFilename 把标题清洗为合法文件名。
func sanitizeFilename(s string) string {
	re := regexp.MustCompile(`[\\/:*?"<>|\r\n\t]`)
	s = re.ReplaceAllString(s, "_")
	s = strings.TrimSpace(s)
	if s == "" || s == "." || s == ".." {
		return "untitled"
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// ExtractArticleID 从文章 URL 中提取用于去重的短标识。
func ExtractArticleID(rawURL string) string {
	u := rawURL
	lower := strings.ToLower(u)
	if strings.HasPrefix(lower, "officialaccount://") {
		u = u[len("officialaccount://"):]
		if !strings.HasPrefix(u, "http") {
			u = "https://" + u
		}
	}

	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}

	if !strings.EqualFold(parsed.Hostname(), "mp.weixin.qq.com") {
		return ""
	}

	// 短链接 /s/xxxx
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasPrefix(path, "/s/") {
		shortID := strings.TrimPrefix(path, "/s/")
		if shortID != "" && !strings.Contains(shortID, "/") {
			return shortID
		}
	}

	// 完整链接 mid+idx
	q := parsed.Query()
	mid := q.Get("mid")
	if mid != "" {
		idx := q.Get("idx")
		if idx == "" {
			idx = "1"
		}
		return mid + "_" + idx
	}
	if q.Get("__biz") != "" {
		if sn := q.Get("sn"); sn != "" {
			return q.Get("__biz") + "_" + sn
		}
		sum := md5.Sum([]byte(parsed.EscapedPath() + "?" + parsed.RawQuery))
		return q.Get("__biz") + "_" + hex.EncodeToString(sum[:8])
	}

	return ""
}
