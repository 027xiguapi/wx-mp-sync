package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	xhsarchive "wx-mp-sync/internal/xhsarchive"
	"wx-mp-sync/pkg/scraper/xhs"
)

var (
	// xhsURLFlags 是 --url 指定的笔记链接。
	xhsURLFlags []string
	// xhsFileFlag 是每行一个链接的文件。
	xhsFileFlag string
	// xhsDelayMS 是两篇笔记之间的间隔毫秒数。
	xhsDelayMS int
)

var xhsCmd = &cobra.Command{
	Use:   "xhs",
	Short: "批量下载小红书笔记到本地",
	Long: "抓取小红书笔记并归档为 Markdown + 原图 + metadata.json。\n" +
		"链接可以是 xiaohongshu.com/explore/...、/discovery/item/... 或 xhslink.com 短链。",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runXHSDownload(cmd.Context())
	},
}

func init() {
	xhsCmd.Flags().StringSliceVar(&xhsURLFlags, "url", nil, "小红书笔记链接,可重复指定")
	xhsCmd.Flags().StringVar(&xhsFileFlag, "file", "", "链接文件,每行一个链接")
	xhsCmd.Flags().IntVar(&xhsDelayMS, "delay", 0, "两篇之间的间隔毫秒数(默认取 batch_delay_ms)")
	rootCmd.AddCommand(xhsCmd)
}

// runXHSDownload 顺序下载一批小红书笔记。
func runXHSDownload(ctx context.Context) error {
	urls, err := collectXHSURLs()
	if err != nil {
		return err
	}
	if len(urls) == 0 {
		return fmt.Errorf("请用 --url 或 --file 提供小红书笔记链接")
	}

	outputDir := Cfg.OutputDir
	if outputFlag != "" {
		outputDir = outputFlag
	}
	delay := xhsDelayMS
	if delay <= 0 {
		delay = Cfg.BatchDelayMS
	}
	if delay <= 0 {
		delay = 1200
	}

	logger := Cfg.App.Logger
	client := xhs.NewClient()
	client.Cookie = Cfg.Naicha.XHSCookie
	archiver := xhsarchive.NewArchiver(outputDir, client, logger)

	fmt.Printf("开始下载 %d 篇小红书笔记 → %s\n", len(urls), outputDir)
	ok, skipped, failed := 0, 0, 0
	for i, raw := range urls {
		if ctx.Err() != nil {
			fmt.Println("\n已取消")
			break
		}
		fmt.Printf("[%d/%d] %s\n", i+1, len(urls), raw)
		noteCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		result, err := archiver.Save(noteCtx, raw)
		cancel()
		switch {
		case err != nil:
			failed++
			fmt.Printf("  ✗ %v\n", err)
		case result.Duplicate:
			skipped++
			fmt.Printf("  • 已存在: %s\n", result.Dir)
		default:
			ok++
			fmt.Printf("  ✓ %s → %s(%d 张图片)\n", result.Title, result.Dir, result.Images)
		}
		if i < len(urls)-1 && ctx.Err() == nil {
			select {
			case <-time.After(time.Duration(delay) * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}

	fmt.Printf("\n完成: 总数=%d 成功=%d 已存在=%d 失败=%d\n输出目录: %s\n", len(urls), ok, skipped, failed, outputDir)
	return nil
}

// collectXHSURLs 合并 --url 与 --file 里的链接,忽略空行和 # 注释。
func collectXHSURLs() ([]string, error) {
	urls := make([]string, 0, len(xhsURLFlags))
	for _, raw := range xhsURLFlags {
		if line := strings.TrimSpace(raw); line != "" {
			urls = append(urls, line)
		}
	}
	if strings.TrimSpace(xhsFileFlag) == "" {
		return urls, nil
	}

	f, err := os.Open(xhsFileFlag)
	if err != nil {
		return nil, fmt.Errorf("打开链接文件失败: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取链接文件失败: %w", err)
	}
	return urls, nil
}
