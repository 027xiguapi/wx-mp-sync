package wxmp

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/dop251/goja"
)

const wechatUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 MicroMessenger/8.0.50(0x1800322f) NetType/WIFI Language/zh_CN"

// setWechatHeaders 为请求注入微信浏览器的 UA 与通用请求头。
func setWechatHeaders(req *http.Request, referer string) {
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("User-Agent", wechatUserAgent)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

// NormalizeMediaURL 清洗媒体 URL 的导出包装。
func NormalizeMediaURL(raw string) string {
	return normalizeMediaURL(raw)
}

// normalizeMediaURL 清洗图片 URL:处理 HTML 实体、补全协议、强制 https。
func normalizeMediaURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	u = strings.ReplaceAll(u, "&amp;amp;", "&")
	u = strings.ReplaceAll(u, "&amp;", "&")
	u = html.UnescapeString(u)
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	if strings.HasPrefix(u, "http://mmbiz.qpic.cn/") {
		u = "https://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// formatPublishTime 把发布时间格式化为 "2006年01月02日 15:04"。
func formatPublishTime(createTime string, unixTime int) string {
	createTime = strings.TrimSpace(createTime)
	if createTime != "" {
		if t, err := time.Parse("2006-01-02 15:04", createTime); err == nil {
			return t.Format("2006年01月02日 15:04")
		}
		return createTime
	}
	if unixTime > 0 {
		return time.Unix(int64(unixTime), 0).Format("2006年01月02日 15:04")
	}
	return ""
}

// isVerificationPage 判断响应是否为微信的"环境异常"验证页。
func isVerificationPage(body string) bool {
	return strings.Contains(body, "环境异常") && strings.Contains(body, "完成验证后即可继续访问")
}

// escapeHTML 对字符串做 HTML 转义。
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}

// parse_cgi_datanew 通过 goja 执行文章页脚本,提取 window.cgiDataNew。
func parse_cgi_datanew(htmlContent string) (*CgiDataNew, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return nil, err
	}

	var scripts []string
	doc.Find("script").Each(func(i int, s *goquery.Selection) {
		text := s.Text()
		if strings.Contains(text, "window.cgiDataNew =") ||
			strings.Contains(text, "var videoPageInfos =") ||
			strings.Contains(text, "picture_page_info_list") {
			scripts = append(scripts, text)
		}
	})

	if len(scripts) == 0 {
		return nil, fmt.Errorf("cgiDataNew script not found")
	}

	vm := goja.New()

	// 模拟 window/document/jQuery 环境,避免页面脚本报错。
	vm.RunString("var window = {};")
	vm.RunString(`
		var document = {
			getElementById: function() { return {}; },
			getElementsByTagName: function() { return []; },
			createElement: function() { return {}; },
			head: {}
		};
		var console = { log: function() {}, warn: function() {}, error: function() {} };
		var mockDom = {
			html: function() { return mockDom; },
			text: function() { return mockDom; },
			val: function() { return mockDom; },
			attr: function() { return mockDom; },
			find: function() { return mockDom; },
			css: function() { return mockDom; },
			addClass: function() { return mockDom; },
			removeClass: function() { return mockDom; },
			show: function() { return mockDom; },
			hide: function() { return mockDom; },
			append: function() { return mockDom; }
		};
		var $ = function() { return mockDom; };
		var jQuery = $;
	`)

	// JsDecode 按恒等函数处理。
	vm.Set("JsDecode", func(call goja.FunctionCall) goja.Value {
		return call.Argument(0)
	})

	for _, script := range scripts {
		if _, err = vm.RunString(script); err != nil {
			// 单个脚本失败不致命,继续尝试下一个
			continue
		}
	}

	val := vm.Get("window").ToObject(vm).Get("cgiDataNew")
	if val == nil {
		return nil, fmt.Errorf("window.cgiDataNew is nil")
	}

	jsonStr, err := vm.RunString("JSON.stringify(window.cgiDataNew)")
	if err != nil {
		return nil, fmt.Errorf("failed to stringify cgiDataNew: %v", err)
	}

	data := &CgiDataNew{}
	if err := json.Unmarshal([]byte(jsonStr.String()), data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cgiDataNew: %v", err)
	}

	if len(data.PicturePageInfoList) == 0 {
		data.PicturePageInfoList = readPicturePageInfoList(vm, "window.picture_page_info_list")
	}
	if len(data.PicturePageInfoList) == 0 {
		data.PicturePageInfoList = readPicturePageInfoList(vm, "picture_page_info_list")
	}

	valVideo := vm.Get("videoPageInfos")
	if valVideo != nil && !goja.IsNull(valVideo) && !goja.IsUndefined(valVideo) {
		jsonVideo, err := vm.RunString("JSON.stringify(videoPageInfos)")
		if err == nil {
			var videoInfos []VideoPageInfoItem
			if err := json.Unmarshal([]byte(jsonVideo.String()), &videoInfos); err == nil {
				data.VideoPageInfos = videoInfos
			}
		}
	}

	return data, nil
}

// readPicturePageInfoList 从 goja 运行时读取图集列表。
func readPicturePageInfoList(vm *goja.Runtime, expr string) []PicturePageInfo {
	val, err := vm.RunString(expr)
	if err != nil || val == nil || goja.IsNull(val) || goja.IsUndefined(val) {
		return nil
	}
	jsonList, err := vm.RunString("JSON.stringify(" + expr + ")")
	if err != nil {
		return nil
	}
	var list []PicturePageInfo
	if err := json.Unmarshal([]byte(jsonList.String()), &list); err != nil {
		return nil
	}
	return list
}

// downloadImageBytes 下载图片并返回字节与 Content-Type。
func downloadImageBytes(imgURL string) ([]byte, string, error) {
	imgURL = normalizeMediaURL(imgURL)
	client := &http.Client{}
	req, err := http.NewRequest("GET", imgURL, nil)
	if err != nil {
		return nil, "", err
	}

	setWechatHeaders(req, "https://mp.weixin.qq.com/")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("bad status: %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, "", err
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	return data, contentType, nil
}

// parseArticleFromDOM 在 cgiDataNew 解析失败时,从 DOM 直接提取文章信息兜底。
func parseArticleFromDOM(htmlContent string) (*WechatOfficialArticle, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return nil, err
	}

	article := &WechatOfficialArticle{}

	title := doc.Find("meta[property=\"og:title\"]").AttrOr("content", "")
	if title == "" {
		title = doc.Find("h1.rich_media_title").Text()
	}
	if title == "" {
		title = doc.Find("title").Text()
	}
	article.Title = strings.TrimSpace(title)

	contentHTML := ""
	doc.Find("#js_content, .rich_media_content").Each(func(i int, s *goquery.Selection) {
		if contentHTML == "" {
			contentHTML, _ = s.Html()
		}
	})
	article.Content = contentHTML

	nickname := doc.Find("meta[name=\"author\"]").AttrOr("content", "")
	if nickname == "" {
		nickname = doc.Find(".rich_media_meta_nickname").First().Text()
	}
	if nickname == "" {
		nickname = doc.Find(".profile_nickname").First().Text()
	}
	article.AuthorNickname = strings.TrimSpace(nickname)

	avatar := doc.Find(".rich_media_meta_avatar, .js_wx_tap_highlight img").First().AttrOr("src", "")
	article.AuthorAvatar = normalizeMediaURL(avatar)

	publishTimeStr := ""
	doc.Find("script").Each(func(i int, s *goquery.Selection) {
		if publishTimeStr != "" {
			return
		}
		text := s.Text()
		if strings.Contains(text, "createTime") {
			re := regexp.MustCompile(`var\s+createTime\s*=\s*'([^']+)'`)
			matches := re.FindStringSubmatch(text)
			if len(matches) > 1 {
				publishTimeStr = formatPublishTime(matches[1], 0)
			}
		}
	})
	article.PublishTimeStr = publishTimeStr

	doc.Find("#js_content img, .rich_media_content img").Each(func(i int, s *goquery.Selection) {
		imgURL := s.AttrOr("data-src", "")
		if imgURL == "" {
			imgURL = s.AttrOr("src", "")
		}
		imgURL = normalizeMediaURL(imgURL)
		if imgURL != "" {
			article.Images = append(article.Images, imgURL)
		}
	})

	return article, nil
}
