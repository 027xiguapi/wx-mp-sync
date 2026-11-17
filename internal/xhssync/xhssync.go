// Package xhssync 把小红书笔记整理成奶茶圈帖子并发布。
package xhssync

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"wx-mp-sync/internal/naicha"
	"wx-mp-sync/pkg/scraper/xhs"
)

const (
	// maxImages 限制一篇笔记最多同步的图片数量。
	maxImages = 18
	// maxTags 与新版论坛的 manualTags 上限保持一致。
	maxTags = 10
	// maxTagUnits 限制单个标签长度。
	maxTagUnits = 24
)

// Options 是整理帖子时的可选项。
type Options struct {
	Board string   // 发帖分类 boardSlug
	Tags  []string // 追加标签
}

// SyncResult 是一次小红书同步的结果。
type SyncResult struct {
	Note         *xhs.Note
	Publish      naicha.PublishResult
	Images       int
	FailedImages int
}

// BuildPost 把小红书笔记整理成论坛帖子:只保留笔记正文和图片,
// 不附带来源/原文链接;图片由 Publish 上传后追加到正文末尾。
func BuildPost(note *xhs.Note, opts Options) *naicha.PublishPost {
	if note == nil {
		return nil
	}

	// 正文/标题里的小红书标签标记只用于平台内检索,发到论坛时去掉,
	// 对应的标签已经通过 tagList 放进帖子的 manualTags。
	cleaned := *note
	cleaned.Title = stripXHSHashtags(note.Title)
	cleaned.Desc = stripXHSHashtags(note.Desc)
	note = &cleaned

	title := strings.TrimSpace(note.Title)
	if title == "" {
		title = firstLine(note.Desc)
	}
	if title == "" {
		title = "小红书笔记"
	}
	title = normalizeTitle(title)

	body := strings.TrimSpace(note.Desc)
	if body == "" {
		body = "（小红书笔记，详见配图）"
	}
	content := body
	if utf8.RuneCountInString(content) < 10 {
		content += "\n\n（全文完）"
	}

	return &naicha.PublishPost{
		Title:     title,
		Content:   content,
		Board:     strings.TrimSpace(opts.Board),
		Tags:      normalizeTags(opts.Tags, note.Tags),
		Source:    note.URL,
		DedupeKey: "xhs:" + note.ID,
	}
}

// Sync 下载笔记图片并发布到奶茶圈。note 可以来自 Fetch,也可以来自页面注入脚本。
func Sync(ctx context.Context, forum *naicha.Client, scraper *xhs.Client, note *xhs.Note, opts Options, force bool) SyncResult {
	result := SyncResult{Note: note}
	if note == nil {
		result.Publish = naicha.PublishResult{Message: "笔记内容为空"}
		return result
	}
	post := BuildPost(note, opts)
	if post == nil {
		result.Publish = naicha.PublishResult{Message: "整理笔记失败"}
		return result
	}

	// 先查去重状态,避免第二次同步还去下载已经过期的小红书图片。
	if !force && post.DedupeKey != "" {
		if rec, ok := forum.LookupKey(post.DedupeKey); ok {
			result.Publish = naicha.PublishResult{
				OK:        true,
				Duplicate: true,
				ID:        rec.PostID,
				URL:       rec.URL,
				Title:     rec.Title,
				Message:   "这篇内容已经同步过了",
			}
			return result
		}
	}

	if forum.BotReady() {
		// 机器人接口不能上传图片,直接引用小红书 CDN 的原图地址。
		post.Images = ImageLinks(note.Images, maxImages)
		result.Images = len(post.Images)
	} else {
		images, failed, err := DownloadImages(ctx, scraper, note, maxImages)
		if err != nil {
			result.Publish = naicha.PublishResult{Message: err.Error()}
			return result
		}
		post.Images = images
		result.Images = len(images)
		result.FailedImages = failed
	}

	result.Publish = forum.Publish(ctx, post, force)
	result.FailedImages += result.Publish.FailedImages
	if result.Publish.OK {
		result.Images = result.Publish.Images
	}
	return result
}

// DownloadImages 并发下载笔记图片,返回按原顺序排列的图片数据。
func DownloadImages(ctx context.Context, scraper *xhs.Client, note *xhs.Note, limit int) ([]naicha.PostImage, int, error) {
	if note == nil || len(note.Images) == 0 {
		return nil, 0, nil
	}
	if limit <= 0 || limit > maxImages {
		limit = maxImages
	}
	source := note.Images
	if len(source) > limit {
		source = source[:limit]
	}

	results := make([]*naicha.PostImage, len(source))
	var failed int32
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i, image := range source {
		wg.Add(1)
		go func(index int, imageURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			data, mime, err := scraper.DownloadImage(ctx, imageURL)
			if err != nil || len(data) == 0 {
				atomic.AddInt32(&failed, 1)
				return
			}
			results[index] = &naicha.PostImage{
				Filename: fmt.Sprintf("xhs-%02d%s", index+1, extensionForMIME(mime)),
				Data:     data,
			}
		}(i, image.URL)
	}
	wg.Wait()

	images := make([]naicha.PostImage, 0, len(source))
	for _, image := range results {
		if image != nil {
			images = append(images, *image)
		}
	}
	if len(images) == 0 {
		return nil, int(atomic.LoadInt32(&failed)), fmt.Errorf("小红书图片全部下载失败,请稍后重试")
	}
	return images, int(atomic.LoadInt32(&failed)), nil
}

// ImageLinks 取前 limit 张图片的原图地址,供机器人接口以 Markdown 引用。
func ImageLinks(images []xhs.Image, limit int) []naicha.PostImage {
	if len(images) == 0 {
		return nil
	}
	if limit <= 0 || limit > maxImages {
		limit = maxImages
	}
	if len(images) > limit {
		images = images[:limit]
	}
	out := make([]naicha.PostImage, 0, len(images))
	for i, image := range images {
		u := strings.TrimSpace(image.URL)
		if u == "" {
			continue
		}
		out = append(out, naicha.PostImage{
			Filename: fmt.Sprintf("xhs-%02d.jpg", i+1),
			URL:      u,
		})
	}
	return out
}

// stripXHSHashtags 去掉小红书正文里的 #话题[话题]# 标记。
// 具体规则放在 xhs.StripTopicTags,本地归档和奶茶圈共用同一份实现。
func stripXHSHashtags(text string) string {
	return xhs.StripTopicTags(text)
}

// normalizeTitle 清理标题并满足论坛的标题长度要求。
func normalizeTitle(raw string) string {
	title := strings.Join(strings.Fields(raw), " ")
	if title == "" {
		title = "小红书笔记"
	}
	if utf8.RuneCountInString(title) > 80 {
		title = string([]rune(title)[:79]) + "…"
	}
	if utf8.RuneCountInString(title) < 5 {
		title += " 笔记"
	}
	return title
}

// normalizeTags 合并配置标签和笔记标签,去重并限制数量。
func normalizeTags(extra, noteTags []string) []string {
	tags := make([]string, 0, maxTags)
	seen := make(map[string]bool, maxTags)
	add := func(raw string) {
		tag := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "#"))
		tag = strings.Join(strings.Fields(tag), " ")
		if tag == "" {
			return
		}
		if utf8.RuneCountInString(tag) > maxTagUnits {
			tag = string([]rune(tag)[:maxTagUnits-1]) + "…"
		}
		key := strings.ToLower(tag)
		if seen[key] {
			return
		}
		seen[key] = true
		tags = append(tags, tag)
	}

	add("小红书")
	for _, tag := range extra {
		add(tag)
	}
	for _, tag := range noteTags {
		add(tag)
	}
	if len(tags) > maxTags {
		tags = tags[:maxTags]
	}
	return tags
}

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

func extensionForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/avif":
		return ".avif"
	default:
		return ".jpg"
	}
}
