// Package chatgptarchive 把 ChatGPT 分享归档到本地:Markdown 正文 + 图片 + metadata.json。
package chatgpt

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	scraper "wx-mp-sync/pkg/scraper/chatgpt"
)

const (
	accountDirName = "ChatGPT"
	maxImages      = 200
	maxImageBytes  = 30 << 20
	imageWorkers   = 4
)

// Fetcher 抓取一份 ChatGPT 分享。
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (*scraper.Article, error)
}

// Archiver 归档 ChatGPT 分享。
type Archiver struct {
	OutputDir  string
	Client     Fetcher
	Logger     *zerolog.Logger
	HTTPClient *http.Client

	download func(ctx context.Context, url string) ([]byte, string, error)
}

// NewArchiver 创建归档器。httpClient 用于下载图片(与抓取共用代理)。
func NewArchiver(outputDir string, client Fetcher, httpClient *http.Client, logger *zerolog.Logger) *Archiver {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	a := &Archiver{
		OutputDir:  outputDir,
		Client:     client,
		Logger:     logger,
		HTTPClient: httpClient,
	}
	a.download = a.downloadImage
	return a
}

// Result 是一次归档的结果。
type Result struct {
	Dir       string
	Title     string
	ShareID   string
	Images    int
	Messages  int
	Duplicate bool
}

type metadata struct {
	ExternalID  string `json:"external_id"`
	Source      string `json:"source"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	PublishTime string `json:"publish_time"`
	URL         string `json:"url"`
	Markdown    string `json:"markdown"`
	Images      int    `json:"images"`
	Messages    int    `json:"messages"`
	SyncedAt    string `json:"synced_at"`
}

// Save 抓取并归档一个 ChatGPT 分享。
func (a *Archiver) Save(ctx context.Context, rawURL string) (*Result, error) {
	if a.Client == nil {
		return nil, fmt.Errorf("ChatGPT 抓取器未初始化")
	}
	article, err := a.Client.Fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	if article.Messages == 0 && len(article.Images) == 0 {
		return nil, fmt.Errorf("分享里没有可下载的正文或图片")
	}

	title := sanitizeTitle(article.Title)
	if title == "" {
		title = "ChatGPT 分享"
	}
	month := time.Now().Format("2006-01")
	if !article.CreatedAt.IsZero() {
		month = article.CreatedAt.Local().Format("2006-01")
	}
	base := filepath.Join(a.OutputDir, accountDirName, month)
	if err := os.MkdirAll(base, 0755); err != nil {
		return nil, fmt.Errorf("创建归档目录失败: %w", err)
	}

	dir, duplicate, err := findOrCreateDir(base, title, article.ShareID, len(article.Images))
	if err != nil {
		return nil, err
	}
	result := &Result{
		Dir:       dir,
		Title:     title,
		ShareID:   article.ShareID,
		Images:    len(article.Images),
		Messages:  article.Messages,
		Duplicate: duplicate,
	}
	if duplicate {
		return result, nil
	}

	relPaths := a.downloadImages(ctx, article, dir)
	markdown := article.Markdown
	for remote, local := range relPaths {
		markdown = strings.ReplaceAll(markdown, "]("+remote+")", "]("+local+")")
	}
	downloaded := len(relPaths)
	result.Images = downloaded

	mdName := title + ".md"
	if err := os.WriteFile(filepath.Join(dir, mdName), []byte(markdown), 0644); err != nil {
		return nil, fmt.Errorf("写入 Markdown 失败: %w", err)
	}

	meta := metadata{
		ExternalID:  "chatgpt:" + article.ShareID,
		Source:      "chatgpt",
		Title:       article.Title,
		Author:      article.Author,
		PublishTime: formatPublishTime(article.CreatedAt),
		URL:         article.SourceURL,
		Markdown:    mdName,
		Images:      downloaded,
		Messages:    article.Messages,
		SyncedAt:    time.Now().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("生成 metadata.json 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0644); err != nil {
		return nil, fmt.Errorf("写入 metadata.json 失败: %w", err)
	}

	if a.Logger != nil {
		a.Logger.Info().
			Str("title", article.Title).
			Str("dir", dir).
			Int("images", downloaded).
			Int("messages", article.Messages).
			Msg("ChatGPT 分享归档成功")
	}
	return result, nil
}

// findOrCreateDir 复用完整归档,补全上次未完成的归档。
func findOrCreateDir(base, title, shareID string, expectedImages int) (string, bool, error) {
	for i := 1; i <= 100; i++ {
		name := title
		if i > 1 {
			name = fmt.Sprintf("%s-%d", title, i)
		}
		dir := filepath.Join(base, name)
		meta, err := readMetadata(dir)
		if err != nil {
			if os.IsNotExist(err) {
				if err := os.MkdirAll(dir, 0755); err != nil {
					return "", false, fmt.Errorf("创建目录失败: %w", err)
				}
				return dir, false, nil
			}
			continue
		}
		if shareID != "" && meta.ExternalID == "chatgpt:"+shareID {
			mdOK := false
			if meta.Markdown != "" {
				if _, err := os.Stat(filepath.Join(dir, meta.Markdown)); err == nil {
					mdOK = true
				}
			}
			if mdOK && meta.Images >= expectedImages {
				return dir, true, nil
			}
			return dir, false, nil
		}
	}
	return "", false, fmt.Errorf("同名目录过多: %s", title)
}

func readMetadata(dir string) (*metadata, error) {
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		if os.IsNotExist(err) {
			if st, statErr := os.Stat(dir); statErr == nil && st.IsDir() {
				return nil, os.ErrNotExist
			}
		}
		return nil, err
	}
	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// downloadImages 下载图片并返回 远程 URL → 本地相对路径。
func (a *Archiver) downloadImages(ctx context.Context, article *scraper.Article, dir string) map[string]string {
	paths := map[string]string{}
	if len(article.Images) == 0 {
		return paths
	}
	imagesDir := filepath.Join(dir, "images")
	if err := os.MkdirAll(imagesDir, 0755); err != nil {
		if a.Logger != nil {
			a.Logger.Warn().Err(err).Msg("创建图片目录失败")
		}
		return paths
	}

	images := article.Images
	if len(images) > maxImages {
		images = images[:maxImages]
	}
	type job struct {
		index int
		image scraper.Image
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for w := 0; w < imageWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}
				data, contentType, err := a.download(ctx, j.image.URL)
				if err != nil {
					if a.Logger != nil {
						a.Logger.Warn().Str("url", j.image.URL).Err(err).Msg("ChatGPT 图片下载失败")
					}
					continue
				}
				ext := imageExt(contentType, j.image.URL)
				name := fmt.Sprintf("%02d-%s.%s", j.index+1, shortHash(j.image.URL), ext)
				if err := os.WriteFile(filepath.Join(imagesDir, name), data, 0644); err != nil {
					if a.Logger != nil {
						a.Logger.Warn().Str("file", name).Err(err).Msg("ChatGPT 图片写入失败")
					}
					continue
				}
				mu.Lock()
				paths[j.image.URL] = "images/" + name
				mu.Unlock()
			}
		}()
	}
	for i, img := range images {
		if strings.TrimSpace(img.URL) == "" {
			continue
		}
		jobs <- job{index: i, image: img}
	}
	close(jobs)
	wg.Wait()
	return paths
}

// downloadImage 下载一张图片,校验内容类型。
func (a *Archiver) downloadImage(ctx context.Context, imageURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	req.Header.Set("Referer", "https://chatgpt.com/")

	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("空响应")
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return nil, "", fmt.Errorf("不是图片: %s", contentType)
	}
	return data, contentType, nil
}

func imageExt(contentType, imageURL string) string {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch ct {
	case "image/jpeg", "image/jpg", "image/pjpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/avif":
		return "avif"
	case "image/bmp":
		return "bmp"
	case "image/svg+xml":
		return "svg"
	}
	lower := strings.ToLower(imageURL)
	if i := strings.IndexAny(lower, "?#"); i >= 0 {
		lower = lower[:i]
	}
	for _, ext := range []string{"jpg", "jpeg", "png", "webp", "gif", "avif", "bmp", "svg"} {
		if strings.HasSuffix(lower, "."+ext) {
			if ext == "jpeg" {
				return "jpg"
			}
			return ext
		}
	}
	return "jpg"
}

func shortHash(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func sanitizeTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|', '\r', '\n', '\t':
			return '_'
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ".")
	if utf8.RuneCountInString(s) > 80 {
		s = string([]rune(s)[:80])
	}
	return strings.TrimSpace(s)
}

func formatPublishTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006年01月02日 15:04")
}
