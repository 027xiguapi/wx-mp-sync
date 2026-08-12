// Package bot 把公众号文章推送到外部 bot/内容服务(POST /api/bot/sync/articles)。
package bot

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"wx-mp-sync/pkg/scraper/wxmp"
)

// Config 描述 bot 同步的配置。
type Config struct {
	Enabled     bool   // 是否启用 bot 推送
	APIBase     string // 例如 http://localhost:7000
	BotID       string // botId
	Type        string // 默认 article
	UploadAPI   string // 图床上传接口路径(如 /api/images/upload);为空则正文直接用微信原图 URL
	UploadField string // multipart 文件字段名,默认 file
	Cookie      string // 可选:上传接口的鉴权 Cookie(如 NextAuth 会话 cookie)
	TimeoutSec  int
}

// SyncResult 描述一次 bot 推送的结果。
type SyncResult struct {
	OK      bool   `json:"ok"`
	Upload  bool   `json:"upload,omitempty"`  // 是否尝试了图片上传
	Message string `json:"error,omitempty"`   // 失败原因
}

// Syncer 负责把文章推送到 bot 服务。
type Syncer struct {
	cfg Config
	log *zerolog.Logger
	dl  *wxmp.OfficialAccountDownload
}

// New 创建 bot 同步器。cfg 未启用时返回 nil。
func New(cfg Config, logger *zerolog.Logger) *Syncer {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Type == "" {
		cfg.Type = "article"
	}
	if cfg.UploadField == "" {
		cfg.UploadField = "file"
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 30
	}
	return &Syncer{cfg: cfg, log: logger, dl: &wxmp.OfficialAccountDownload{}}
}

// Sync 生成文章 Markdown 并推送到 bot 接口。返回结果结构。
func (s *Syncer) Sync(ctx context.Context, article *wxmp.WechatOfficialArticle, rawURL string) SyncResult {
	if article == nil {
		return SyncResult{OK: false, Message: "article is nil"}
	}

	markdown, images, err := s.dl.MarkdownWithRemoteImages(article)
	if err != nil {
		return SyncResult{OK: false, Message: fmt.Sprintf("生成 Markdown 失败: %v", err)}
	}

	cover := firstImageOrCover(article, images)

	uploaded := false
	if s.cfg.UploadAPI != "" && len(images) > 0 {
		mapping, uploadErr := s.uploadImages(ctx, images)
		if uploadErr != nil {
			s.log.Warn().Err(uploadErr).Int("images", len(images)).Msg("bot 图片上传失败,回退用原图 URL")
		} else if len(mapping) > 0 {
			uploaded = true
			// 正文图片替换为自定义标签 <CustomImage src="thumbUrl" fullsrc="url" ... />
			markdown = replaceImagesWithCustomTags(markdown, images, mapping)
			if up, ok := mapping[cover]; ok && up.URL != "" {
				cover = up.URL
			}
		}
	}

	// 与后端 /api/bot/sync/articles 契约一致:title 和 url 必填
	articleURL := strings.TrimSpace(rawURL)
	if articleURL == "" && article.PageJSON != nil {
		articleURL = strings.TrimSpace(article.PageJSON.Link)
	}
	payload := map[string]any{
		"title":         strings.TrimSpace(article.Title),
		"url":           articleURL,
		"content":       markdown,
		"summary":       articleSummary(article),
		"coverImageUrl": cover,
		"author":        strings.TrimSpace(article.Creator),
		"sourceName":    strings.TrimSpace(article.AuthorNickname),
		"publishedAt":   publishTimeRFC3339(article),
	}

	body, _ := json.Marshal(payload)
	endpoint := strings.TrimRight(s.cfg.APIBase, "/") + "/api/bot/sync/articles"
	s.log.Info().Str("endpoint", endpoint).Str("title", article.Title).Bool("upload", uploaded).Msg("bot 推送文章")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return SyncResult{OK: false, Upload: uploaded, Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: time.Duration(s.cfg.TimeoutSec) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return SyncResult{OK: false, Upload: uploaded, Message: err.Error()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = resp.Status
		}
		s.log.Error().Str("endpoint", endpoint).Int("status", resp.StatusCode).Str("body", msg).Msg("bot 推送失败")
		return SyncResult{OK: false, Upload: uploaded, Message: fmt.Sprintf("bot 接口返回 %s: %s", resp.Status, msg)}
	}

	s.log.Info().Str("title", article.Title).Msg("bot 推送成功")
	return SyncResult{OK: true, Upload: uploaded}
}

// UploadedImage 是一次上传返回的图片信息。
type UploadedImage struct {
	URL      string // 原图(图床 fullsrc)
	ThumbURL string // 缩略图(图床 src)
}

// uploadImages 逐个下载图片并上传到图床,返回 原URL→UploadedImage 映射。
func (s *Syncer) uploadImages(ctx context.Context, images []wxmp.ArticleImage) (map[string]UploadedImage, error) {
	mapping := make(map[string]UploadedImage)
	client := &http.Client{Timeout: time.Duration(s.cfg.TimeoutSec) * time.Second}

	for _, img := range images {
		if strings.TrimSpace(img.URL) == "" {
			continue
		}
		data, _, err := s.dl.DownloadBytes(img.URL)
		if err != nil {
			s.log.Warn().Str("url", img.URL).Err(err).Msg("下载图片失败,跳过上传")
			continue
		}
		up, err := s.uploadOne(ctx, client, img.URL, data)
		if err != nil {
			s.log.Warn().Str("url", img.URL).Err(err).Msg("上传图片失败,跳过")
			continue
		}
		mapping[img.URL] = up
	}
	return mapping, nil
}

// uploadOne 以 multipart 上传单张图片(file + md5),解析返回的图片 URL 与缩略图。
// 兼容插件图床接口 POST /api/bot/sync/images 与 POST /api/images/upload,
// 返回 {success, data:{url, thumbUrl,...}}。
func (s *Syncer) uploadOne(ctx context.Context, client *http.Client, originalURL string, data []byte) (UploadedImage, error) {
	md5hex := fmt.Sprintf("%x", md5.Sum(data))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(s.cfg.UploadField, "image.png")
	if err != nil {
		return UploadedImage{}, err
	}
	if _, err := fw.Write(data); err != nil {
		return UploadedImage{}, err
	}
	// md5 字段:部分图床接口用它做去重;不用的接口会忽略此字段
	if err := mw.WriteField("md5", md5hex); err != nil {
		return UploadedImage{}, err
	}
	if err := mw.Close(); err != nil {
		return UploadedImage{}, err
	}

	endpoint := strings.TrimRight(s.cfg.APIBase, "/") + "/" + strings.TrimLeft(s.cfg.UploadAPI, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return UploadedImage{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if s.cfg.Cookie != "" {
		req.Header.Set("Cookie", s.cfg.Cookie)
	}

	resp, err := client.Do(req)
	if err != nil {
		return UploadedImage{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return UploadedImage{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return UploadedImage{}, fmt.Errorf("上传接口返回 401 未授权(需要登录 Cookie,请在配置里填 bot_sync.cookie)")
	}
	if resp.StatusCode >= 300 {
		return UploadedImage{}, fmt.Errorf("上传接口返回 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	img, apiErr, ok := parseUploadResponse(body)
	if !ok {
		return UploadedImage{}, fmt.Errorf("上传接口返回错误: %s", firstNonEmpty(apiErr, strings.TrimSpace(string(body)), resp.Status))
	}
	// 用 UPLOAD_R2_DOMAIN 归一化图片域名(相对路径补全 / 默认 r2.dev 域名替换)
	img.URL = normalizeR2URL(img.URL)
	img.ThumbURL = normalizeR2URL(img.ThumbURL)
	return img, nil
}

// r2Domain 返回 UPLOAD_R2_DOMAIN 环境变量(去尾部斜杠),用于自定义图床域名。
func r2Domain() string {
	return strings.TrimRight(os.Getenv("UPLOAD_R2_DOMAIN"), "/")
}

// normalizeR2URL 根据 UPLOAD_R2_DOMAIN 归一化图床返回的图片 URL:
//   - 相对路径(/xxx) → 补全为 {domain}/xxx
//   - 绝对 URL 且主机是 Cloudflare R2 默认域(*.r2.dev) → 替换为自定义域
//   - 其余原样返回
func normalizeR2URL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	domain := r2Domain()
	if domain == "" {
		return raw
	}
	if strings.HasPrefix(raw, "/") {
		return domain + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return raw
	}
	if strings.HasSuffix(strings.ToLower(u.Hostname()), ".r2.dev") {
		if d, derr := url.Parse(domain); derr == nil && d.Hostname() != "" {
			if d.Scheme != "" {
				u.Scheme = d.Scheme
			} else {
				u.Scheme = "https"
			}
			u.Host = d.Host
			return u.String()
		}
	}
	return raw
}

// parseUploadResponse 解析图床上传响应,返回图片(url+thumbUrl)、错误信息、是否成功。
// 优先兼容返回结构 {success, data:{url, thumbUrl}, error?}。
func parseUploadResponse(body []byte) (img UploadedImage, errMsg string, ok bool) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return UploadedImage{}, "空响应", false
	}

	// 标准结构:{success, data:{url, thumbUrl,...}, error}
	var std struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Data    struct {
			URL      string `json:"url"`
			ThumbURL string `json:"thumbUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &std); err == nil && std.Data.URL != "" {
		if std.Success {
			return UploadedImage{URL: std.Data.URL, ThumbURL: std.Data.ThumbURL}, "", true
		}
		return UploadedImage{}, firstNonEmpty(std.Error, "接口返回 success=false"), false
	}

	// 兼容其它结构:递归找第一个 HTTP URL 字段作为原图
	if u := findURLInJSON(body); u != "" {
		return UploadedImage{URL: u}, "", true
	}
	// 纯文本即 URL
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return UploadedImage{URL: trimmed}, "", true
	}
	return UploadedImage{}, "", false
}

// findURLInJSON 递归从任意 JSON 中找第一个形如 HTTP URL 的字符串字段。
func findURLInJSON(body []byte) string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	var found string
	var walk func(any)
	walk = func(node any) {
		if found != "" {
			return
		}
		switch n := node.(type) {
		case map[string]any:
			for _, k := range []string{"url", "src", "imageUrl", "image_url", "path", "data"} {
				if s, ok := n[k].(string); ok && isHTTPURL(s) {
					found = s
					return
				}
			}
			for _, val := range n {
				walk(val)
			}
		case []any:
			for _, item := range n {
				walk(item)
			}
		case string:
			if isHTTPURL(n) {
				found = n
			}
		}
	}
	walk(v)
	return found
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// replaceImagesWithCustomTags 把 markdown 里的图片引用替换为自定义标签:
//
//	<CustomImage src="thumbUrl" fullsrc="url" alt="alt" />
func replaceImagesWithCustomTags(markdown string, images []wxmp.ArticleImage, mapping map[string]UploadedImage) string {
	for _, img := range images {
		up, ok := mapping[img.URL]
		if !ok || up.URL == "" {
			continue
		}
		alt := strings.TrimSpace(img.Alt)
		if alt == "" {
			alt = "image"
		}
		// 匹配 ![任意alt](原始URL)
		re := regexp.MustCompile(`!\[[^\]]*\]\(` + regexp.QuoteMeta(img.URL) + `\)`)
		tag := `<CustomImage src="` + escapeAttr(up.ThumbURL) + `" fullsrc="` + escapeAttr(up.URL) + `" alt="` + escapeAttr(alt) + `" />`
		markdown = re.ReplaceAllString(markdown, tag)
	}
	return markdown
}

// escapeAttr 对 HTML 属性值做转义。
func escapeAttr(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return replacer.Replace(s)
}

// articleSummary 返回文章摘要(PageJSON.Desc)。
func articleSummary(article *wxmp.WechatOfficialArticle) string {
	if article != nil && article.PageJSON != nil {
		return strings.TrimSpace(article.PageJSON.Desc)
	}
	return ""
}

// publishTimeRFC3339 返回文章的发布时间(RFC3339),供 publishedAt 字段使用。
func publishTimeRFC3339(article *wxmp.WechatOfficialArticle) string {
	if article == nil || article.PageJSON == nil {
		return ""
	}
	data := article.PageJSON
	if data.OriCreateTime > 0 {
		return time.Unix(int64(data.OriCreateTime), 0).Format(time.RFC3339)
	}
	if t, err := time.Parse("2006-01-02 15:04", strings.TrimSpace(data.CreateTime)); err == nil {
		return t.Format(time.RFC3339)
	}
	return ""
}

// firstImageOrCover 选择封面:优先正文第一张图,否则用文章封面。
func firstImageOrCover(article *wxmp.WechatOfficialArticle, images []wxmp.ArticleImage) string {
	if len(images) > 0 {
		return images[0].URL
	}
	if article != nil && article.PageJSON != nil {
		if u := wxmp.NormalizeMediaURL(article.PageJSON.CdnURL); u != "" {
			return u
		}
	}
	return ""
}
