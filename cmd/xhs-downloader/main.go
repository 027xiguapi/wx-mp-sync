//go:build windows

// Command xhs-downloader 是一个独立的小红书笔记下载器。
// 打开后直接在窗口里粘贴链接即可下载,不依赖公众号同步、代理或证书。
//
//go:generate go run github.com/akavel/rsrc@latest -manifest xhs-downloader.manifest -o rsrc.syso
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/rs/zerolog"

	xhsarchive "wx-mp-sync/internal/xhsarchive"
	"wx-mp-sync/pkg/scraper/xhs"
)

const (
	appTitle          = "小红书笔记下载器"
	delayBetweenNotes = 1200 * time.Millisecond
	perNoteTimeout    = 3 * time.Minute
)

// appConfig 是独立下载器自己的配置,与主程序的 config.yaml 无关。
type appConfig struct {
	OutputDir string `json:"output_dir"`
}

type downloaderApp struct {
	*walk.MainWindow

	input     *walk.TextEdit
	outputDir *walk.LineEdit
	logBox    *walk.TextEdit
	status    *walk.Label
	progress  *walk.ProgressBar
	startBtn  *walk.PushButton
	cancelBtn *walk.PushButton

	logger *zerolog.Logger

	mu        sync.Mutex
	cancel    context.CancelFunc
	running   bool
	logBuffer strings.Builder
}

func main() {
	debugLog("main start")
	logger, closeLog := newLogger()
	defer closeLog()

	app := &downloaderApp{logger: logger}
	cfg := loadConfig()
	if strings.TrimSpace(cfg.OutputDir) == "" {
		cfg.OutputDir = defaultOutputDir()
	}
	debugLog("config loaded: %s", cfg.OutputDir)

	if err := (MainWindow{
		AssignTo: &app.MainWindow,
		Title:    appTitle,
		MinSize:  Size{Width: 720, Height: 520},
		Size:     Size{Width: 820, Height: 620},
		Layout: VBox{
			Margins: Margins{Left: 14, Top: 14, Right: 14, Bottom: 14},
			Spacing: 8,
		},
		Children: []Widget{
			Label{
				Text: appTitle,
				Font: Font{Family: "Microsoft YaHei", PointSize: 16, Bold: true},
			},
			Label{Text: "粘贴小红书笔记链接(每行一个,也支持直接粘贴整段分享文本):"},
			TextEdit{
				AssignTo:      &app.input,
				VScroll:       true,
				MinSize:       Size{Height: 110},
				StretchFactor: 1,
			},
			Composite{
				Layout: HBox{Spacing: 8},
				Children: []Widget{
					Label{Text: "保存到:"},
					LineEdit{
						AssignTo:      &app.outputDir,
						Text:          cfg.OutputDir,
						StretchFactor: 1,
					},
					PushButton{Text: "打开目录", OnClicked: app.openOutputDir},
				},
			},
			Composite{
				Layout: HBox{Spacing: 8},
				Children: []Widget{
					PushButton{AssignTo: &app.startBtn, Text: "开始下载", OnClicked: app.startDownload},
					PushButton{AssignTo: &app.cancelBtn, Text: "取消", Enabled: false, OnClicked: app.cancelDownload},
					PushButton{Text: "清空记录", OnClicked: app.clearLog},
					HSpacer{},
					Label{AssignTo: &app.status, Text: "就绪"},
				},
			},
			ProgressBar{AssignTo: &app.progress, MaxValue: 100},
			TextEdit{
				AssignTo:      &app.logBox,
				ReadOnly:      true,
				VScroll:       true,
				StretchFactor: 3,
			},
		},
	}).Create(); err != nil {
		debugLog("create failed: %v", err)
		walk.MsgBox(nil, appTitle, "启动失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}

	debugLog("window created")
	_ = app.input.SetFocus()
	app.Run()
	debugLog("run returned")
}

// debugLog 把启动/退出过程写到临时文件,便于排查 GUI 子系统的启动问题。
func debugLog(format string, args ...any) {
	dir := filepath.Join(os.TempDir(), "xhs-downloader")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "startup.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// startDownload 解析输入框里的链接并启动后台下载。
func (a *downloaderApp) startDownload() {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	urls, invalid := parseLinks(a.input.Text())
	if len(urls) == 0 {
		walk.MsgBox(a.MainWindow, appTitle,
			"没有识别到小红书笔记链接。\n\n支持 xiaohongshu.com/explore/...、/discovery/item/... 和 xhslink.com 短链。",
			walk.MsgBoxIconWarning)
		return
	}

	outDir := strings.TrimSpace(a.outputDir.Text())
	if outDir == "" {
		outDir = defaultOutputDir()
		_ = a.outputDir.SetText(outDir)
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		walk.MsgBox(a.MainWindow, appTitle, "创建保存目录失败:\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	saveConfig(appConfig{OutputDir: outDir})

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.running = true
	a.cancel = cancel
	a.mu.Unlock()

	a.startBtn.SetEnabled(false)
	a.cancelBtn.SetEnabled(true)
	a.progress.SetRange(0, len(urls))
	a.progress.SetValue(0)
	_ = a.status.SetText(fmt.Sprintf("开始下载 %d 篇…", len(urls)))
	a.clearLog()
	a.appendLog(fmt.Sprintf("开始下载 %d 篇 → %s\r\n", len(urls), outDir))
	if invalid > 0 {
		a.appendLog(fmt.Sprintf("已忽略 %d 行无法识别的链接\r\n", invalid))
	}

	client := xhs.NewClient()
	archiver := xhsarchive.NewArchiver(outDir, client, a.logger)

	go func() {
		defer func() {
			cancel()
			a.Synchronize(func() {
				a.mu.Lock()
				a.running = false
				a.cancel = nil
				a.mu.Unlock()
				a.startBtn.SetEnabled(true)
				a.cancelBtn.SetEnabled(false)
			})
		}()

		ok, skipped, failed := 0, 0, 0
		for i, rawURL := range urls {
			if ctx.Err() != nil {
				break
			}
			a.appendLogSynced(fmt.Sprintf("[%d/%d] %s\r\n", i+1, len(urls), rawURL))

			noteCtx, noteCancel := context.WithTimeout(ctx, perNoteTimeout)
			result, err := archiver.Save(noteCtx, rawURL)
			noteCancel()

			switch {
			case err != nil:
				failed++
				a.appendLogSynced(fmt.Sprintf("  ✗ %v\r\n", err))
			case result.Duplicate:
				skipped++
				a.appendLogSynced(fmt.Sprintf("  • 已存在: %s\r\n", result.Dir))
			default:
				ok++
				a.appendLogSynced(fmt.Sprintf("  ✓ %s → %s(%d 张图片)\r\n", result.Title, result.Dir, result.Images))
			}

			done := i + 1
			a.Synchronize(func() { a.progress.SetValue(done) })

			if i < len(urls)-1 && ctx.Err() == nil {
				select {
				case <-time.After(delayBetweenNotes):
				case <-ctx.Done():
				}
			}
		}

		summary := fmt.Sprintf("完成: 总数=%d 成功=%d 已存在=%d 失败=%d\r\n", len(urls), ok, skipped, failed)
		a.appendLogSynced("\r\n" + summary)
		a.Synchronize(func() {
			_ = a.status.SetText(fmt.Sprintf("完成:成功 %d,已存在 %d,失败 %d", ok, skipped, failed))
		})
	}()
}

// cancelDownload 请求取消当前任务。
func (a *downloaderApp) cancelDownload() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	a.cancelBtn.SetEnabled(false)
	_ = a.status.SetText("正在取消…")
	a.appendLog("正在取消…\r\n")
}

// openOutputDir 在资源管理器中打开保存目录。
func (a *downloaderApp) openOutputDir() {
	dir := strings.TrimSpace(a.outputDir.Text())
	if dir == "" {
		dir = defaultOutputDir()
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		walk.MsgBox(a.MainWindow, appTitle, "创建保存目录失败:\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	if err := exec.Command("explorer.exe", dir).Start(); err != nil {
		walk.MsgBox(a.MainWindow, appTitle, "打开目录失败:\n"+err.Error(), walk.MsgBoxIconError)
	}
}

// clearLog 清空日志区和缓冲区。
func (a *downloaderApp) clearLog() {
	a.logBuffer.Reset()
	if a.logBox != nil {
		_ = a.logBox.SetText("")
	}
}

// appendLog 只能在 UI 线程调用。
// 日志框是只读的,EM_REPLACESEL 对只读控件无效,所以维护缓冲区后用 SetText 刷新。
func (a *downloaderApp) appendLog(text string) {
	if a.logBox == nil {
		return
	}
	a.logBuffer.WriteString(text)
	_ = a.logBox.SetText(a.logBuffer.String())
	length := a.logBox.TextLength()
	a.logBox.SetTextSelection(length, length)
	a.logBox.ScrollToCaret()
}

// appendLogSynced 从后台 goroutine 更新日志。
func (a *downloaderApp) appendLogSynced(text string) {
	a.Synchronize(func() { a.appendLog(text) })
}

// parseLinks 从输入文本里提取小红书链接,按笔记 ID 去重,并统计无法识别的行数。
func parseLinks(text string) (urls []string, invalid int) {
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rawURL := xhs.ExtractURL(line)
		if rawURL == "" {
			invalid++
			continue
		}
		key := rawURL
		if id, err := xhs.ParseNoteID(rawURL); err == nil {
			key = id
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		urls = append(urls, rawURL)
	}
	return urls, invalid
}

// defaultOutputDir 默认保存到用户的「下载」目录下。
func defaultOutputDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".", "小红书")
	}
	downloads := filepath.Join(home, "Downloads")
	if st, err := os.Stat(downloads); err == nil && st.IsDir() {
		return filepath.Join(downloads, "小红书")
	}
	return filepath.Join(home, "小红书")
}

// configPath 返回独立下载器的配置文件路径。
func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "xhs-downloader", "config.json")
}

func loadConfig() appConfig {
	var cfg appConfig
	path := configPath()
	if path == "" {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func saveConfig(cfg appConfig) {
	path := configPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0644)
}

// newLogger 把归档器日志写到临时目录,便于排查问题。
func newLogger() (*zerolog.Logger, func()) {
	dir := filepath.Join(os.TempDir(), "xhs-downloader")
	if err := os.MkdirAll(dir, 0755); err != nil {
		l := zerolog.Nop()
		return &l, func() {}
	}
	f, err := os.OpenFile(filepath.Join(dir, "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		l := zerolog.Nop()
		return &l, func() {}
	}
	l := zerolog.New(f).With().Timestamp().Logger()
	return &l, func() { _ = f.Close() }
}
