// Package xhsarchive 把小红书笔记归档到本地:Markdown 正文 + 原图 + metadata.json。
package xhsarchive

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"wx-mp-sync/pkg/scraper/xhs"
)

const (
	// accountDirName 是小红书归档在输出目录下的子目录名。
	accountDirName = "小红书"
	// maxImages 限制单篇笔记最多归档的图片数。
	maxImages = 100
	// imageWorkers 是图片下载并发数。
	imageWorkers = 4
)

// Fetcher 抓取一篇小红书笔记并下载图片;真实实现是 *xhs.Client。
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (*xhs.Note, error)
	DownloadImage(ctx context.Context, imageURL string) ([]byte, string, error)
}

// Archiver 归档小红书笔记。
type Archiver struct {
	OutputDir string
	Client    Fetcher
	Logger    *zerolog.Logger

	// download 允许测试替换图片下载逻辑。
	download func(ctx context.Context, imageURL string) ([]byte, string, error)
}

// NewArchiver 创建小红书归档器。
func NewArchiver(outputDir string, client Fetcher, logger *zerolog.Logger) *Archiver {
	return &Archiver{
		OutputDir: outputDir,
		Client:    client,
		Logger:    logger,
	}
}

// Result 是一次归档的结果。
type Result struct {
	Dir          string
	Title        string
	NoteID       string
	Images       int
	FailedImages int
	Duplicate    bool
}

// metadata 是归档目录里 metadata.json 的结构。
// 字段名与公众号文章/豆包/ChatGPT 归档保持一致,管理页可以直接复用。
type metadata struct {
	ExternalID   string   `json:"external_id"`
	Source       string   `json:"source"`
	NoteID       string   `json:"note_id"`
	Title        string   `json:"title"`
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id"`
	PublishTime  string   `json:"publish_time"`
	URL          string   `json:"url"`
	Markdown     string   `json:"markdown"`
	Images       int      `json:"images"`
	FailedImages int      `json:"failed_images,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	VideoURL     string   `json:"video_url,omitempty"`
	SyncedAt     string   `json:"synced_at"`
}

// Save 抓取并归档一篇小红书笔记。重复下载同一篇笔记时返回 Duplicate。
func (a *Archiver) Save(ctx context.Context, rawURL string) (*Result, error) {
	if a.Client == nil {
		return nil, fmt.Errorf("小红书抓取器未初始化")
	}
	note, err := a.Client.Fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	if note == nil {
		return nil, fmt.Errorf("小红书笔记内容为空")
	}
	if strings.TrimSpace(note.Title) == "" && strings.TrimSpace(note.Desc) == "" &&
		len(note.Images) == 0 && strings.TrimSpace(note.VideoURL) == "" {
		return nil, fmt.Errorf("小红书笔记没有正文或图片")
	}

	title := archiveTitle(note)
	month := noteMonth(note.PublishedAt, time.Now())
	base := filepath.Join(a.OutputDir, accountDirName, month)
	if err := os.MkdirAll(base, 0755); err != nil {
		return nil, fmt.Errorf("创建归档目录失败: %w", err)
	}

	expectedImages := len(note.Images)
	if expectedImages > maxImages {
		expectedImages = maxImages
	}
	externalID := "xhs:" + note.ID
	dir, duplicate, err := findOrCreateDir(base, title, externalID, expectedImages)
	if err != nil {
		return nil, err
	}
	result := &Result{
		Dir:       dir,
		Title:     title,
		NoteID:    note.ID,
		Images:    expectedImages,
		Duplicate: duplicate,
	}
	if duplicate {
		return result, nil
	}

	imagesDir := filepath.Join(dir, "images")
	if len(note.Images) > 0 {
		if err := os.MkdirAll(imagesDir, 0755); err != nil {
			return nil, fmt.Errorf("创建图片目录失败: %w", err)
		}
	}
	localPaths, failed := a.downloadImages(ctx, note, imagesDir)
	downloaded := 0
	for _, path := range localPaths {
		if path != "" {
			downloaded++
		}
	}

	mdName := title + ".md"
	markdown := buildMarkdown(note, localPaths, title)
	if err := os.WriteFile(filepath.Join(dir, mdName), []byte(markdown), 0644); err != nil {
		return nil, fmt.Errorf("写入 Markdown 失败: %w", err)
	}

	meta := metadata{
		ExternalID:   externalID,
		Source:       "xhs",
		NoteID:       note.ID,
		Title:        strings.TrimSpace(note.Title),
		Author:       strings.TrimSpace(note.Author),
		AuthorID:     strings.TrimSpace(note.AuthorID),
		PublishTime:  strings.TrimSpace(note.PublishedAt),
		URL:          noteURL(note, rawURL),
		Markdown:     mdName,
		Images:       downloaded,
		FailedImages: failed,
		Tags:         note.Tags,
		VideoURL:     strings.TrimSpace(note.VideoURL),
		SyncedAt:     time.Now().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("生成 metadata.json 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0644); err != nil {
		return nil, fmt.Errorf("写入 metadata.json 失败: %w", err)
	}

	result.Images = downloaded
	result.FailedImages = failed
	if a.Logger != nil {
		a.Logger.Info().
			Str("note_id", note.ID).
			Str("title", note.Title).
			Str("dir", dir).
			Int("images", downloaded).
			Int("failed_images", failed).
			Msg("小红书笔记归档成功")
	}
	return result, nil
}

// downloadImages 并发下载图片,返回与 note.Images 对齐的本地相对路径(失败的为空串)。
func (a *Archiver) downloadImages(ctx context.Context, note *xhs.Note, imagesDir string) ([]string, int) {
	source := note.Images
	if len(source) > maxImages {
		source = source[:maxImages]
	}
	local := make([]string, len(source))
	if len(source) == 0 {
		return local, 0
	}

	var failed int32
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < imageWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}
				imageURL := source[i].URL
				data, mime, err := a.downloadImage(ctx, imageURL)
				if err != nil || len(data) == 0 {
					atomic.AddInt32(&failed, 1)
					if a.Logger != nil {
						a.Logger.Warn().Str("url", imageURL).Err(err).Msg("小红书图片下载失败")
					}
					continue
				}
				name := fmt.Sprintf("%02d-%s.%s", i+1, shortHash(imageURL), imageExt(mime, imageURL))
				if err := os.WriteFile(filepath.Join(imagesDir, name), data, 0644); err != nil {
					atomic.AddInt32(&failed, 1)
					if a.Logger != nil {
						a.Logger.Warn().Str("file", name).Err(err).Msg("小红书图片写入失败")
					}
					continue
				}
				local[i] = "images/" + name
			}
		}()
	}

	for i := range source {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return local, int(atomic.LoadInt32(&failed))
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return local, int(atomic.LoadInt32(&failed))
}

// downloadImage 调用抓取器的图片下载,测试时可以替换。
func (a *Archiver) downloadImage(ctx context.Context, imageURL string) ([]byte, string, error) {
	if a.download != nil {
		return a.download(ctx, imageURL)
	}
	return a.Client.DownloadImage(ctx, imageURL)
}

// buildMarkdown 生成笔记 Markdown:标题 + 清洗后的正文 + 本地图片引用。
// 图片下载失败时保留远程地址,避免正文里丢图。
func buildMarkdown(note *xhs.Note, local []string, title string) string {
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(title)
	b.WriteString("\n")

	body := xhs.StripTopicTags(note.Desc)
	if body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}

	for i, image := range note.Images {
		if i >= len(local) {
			break
		}
		link := strings.TrimSpace(local[i])
		if link == "" {
			link = strings.TrimSpace(image.URL)
		}
		if link == "" {
			continue
		}
		b.WriteString("\n![](")
		b.WriteString(link)
		b.WriteString(")\n")
	}

	if videoURL := strings.TrimSpace(note.VideoURL); videoURL != "" {
		b.WriteString("\n[视频](")
		b.WriteString(videoURL)
		b.WriteString(")\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// findOrCreateDir 按 external_id 查找已有目录;找不到时用标题创建新目录,
// 同名冲突依次尝试 title-2、title-3。返回 duplicate 表示已有完整归档。
func findOrCreateDir(base, title, externalID string, expectedImages int) (string, bool, error) {
	if dir, complete, found := findExistingByID(base, externalID, expectedImages); found {
		return dir, complete, nil
	}

	for i := 1; i <= 100; i++ {
		name := title
		if i > 1 {
			name = fmt.Sprintf("%s-%d", title, i)
		}
		dir := filepath.Join(base, name)
		st, err := os.Stat(dir)
		if err == nil {
			if !st.IsDir() {
				continue
			}
			// 空目录可以复用(通常是上次中断留下的);有内容的同名目录继续找下一个名字。
			entries, readErr := os.ReadDir(dir)
			if readErr == nil && len(entries) == 0 {
				return dir, false, nil
			}
			continue
		}
		if !os.IsNotExist(err) {
			return "", false, fmt.Errorf("检查归档目录失败: %w", err)
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", false, fmt.Errorf("创建目录失败: %w", err)
		}
		return dir, false, nil
	}
	return "", false, fmt.Errorf("同名目录过多: %s", title)
}

// findExistingByID 在月份目录里按 external_id 查找已有归档。
func findExistingByID(base, externalID string, expectedImages int) (string, bool, bool) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", false, false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		meta, err := readMetadata(dir)
		if err != nil || meta.ExternalID != externalID {
			continue
		}
		mdOK := false
		if strings.TrimSpace(meta.Markdown) != "" {
			if _, err := os.Stat(filepath.Join(dir, meta.Markdown)); err == nil {
				mdOK = true
			}
		}
		return dir, mdOK && meta.Images >= expectedImages, true
	}
	return "", false, false
}

// readMetadata 读取归档目录里的 metadata.json。
func readMetadata(dir string) (*metadata, error) {
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return nil, err
	}
	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// archiveTitle 生成用于目录名和 Markdown 文件名的标题。
func archiveTitle(note *xhs.Note) string {
	title := strings.Join(strings.Fields(note.Title), " ")
	if title == "" {
		title = firstLine(note.Desc)
	}
	if title == "" {
		title = "小红书笔记"
	}
	title = sanitizeTitle(title)
	if title == "" {
		title = "小红书笔记"
	}
	return title
}

// firstLine 取正文第一行作为无标题笔记的兜底标题。
func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	line := strings.SplitN(text, "\n", 2)[0]
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > 80 {
		line = string([]rune(line)[:80])
	}
	return line
}

// noteMonth 由发布日期得到归档月份;解析失败时用当前月份。
func noteMonth(published string, now time.Time) string {
	published = strings.TrimSpace(published)
	if t, err := time.ParseInLocation("2006-01-02", published, time.Local); err == nil {
		return t.Format("2006-01")
	}
	return now.Format("2006-01")
}

// noteURL 优先使用抓取时的页面地址,其次用笔记标准地址。
func noteURL(note *xhs.Note, rawURL string) string {
	if u := strings.TrimSpace(note.URL); u != "" {
		return u
	}
	if u := strings.TrimSpace(rawURL); u != "" {
		return u
	}
	if note.ID != "" {
		return xhs.CanonicalURL(note.ID)
	}
	return ""
}

// sanitizeTitle 去掉 Windows 文件名非法字符并限制长度。
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

// imageExt 由 Content-Type 推断扩展名,识别不了时看 URL。
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

// shortHash 生成 URL 的短哈希,用于图片文件名。
func shortHash(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}
