//go:build windows

// Command xhs-downloader 是一个独立的小红书笔记下载器。
// 打开后直接在窗口里粘贴链接即可下载,也可以把笔记批量同步到奶茶圈论坛。
// 不依赖公众号同步、代理或证书。
//
//go:generate go run github.com/akavel/rsrc@latest -manifest xhs-downloader.manifest -o rsrc.syso
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/rs/zerolog"

	"wx-mp-sync/internal/naicha"
	xhsarchive "wx-mp-sync/internal/xhsarchive"
	"wx-mp-sync/internal/xhssync"
	"wx-mp-sync/pkg/scraper/xhs"
)

const (
	appTitle          = "小红书笔记下载器"
	delayBetweenNotes = 1200 * time.Millisecond
	perNoteTimeout    = 3 * time.Minute
	// perNoteSyncTimeout 比下载宽松一些:同步要先传图再发帖,论坛还有上传限流。
	perNoteSyncTimeout = 5 * time.Minute

	defaultForumAPIBase = "http://localhost:3000"
	defaultForumBoard   = "new-releases"
)

// appConfig 是独立下载器自己的配置,与主程序的 config.yaml 无关。
type appConfig struct {
	OutputDir string        `json:"output_dir"`
	Forum     forumSettings `json:"forum"`
}

// forumSettings 是「同步到奶茶圈论坛」的连接设置。
// 机器人密钥以明文保存在用户配置目录里,与主程序 config.yaml 的做法一致。
type forumSettings struct {
	APIBase   string   `json:"api_base"`
	Bot       string   `json:"bot"`
	BotSecret string   `json:"bot_secret"`
	Board     string   `json:"board"`
	Tags      []string `json:"tags,omitempty"`
}

type downloaderApp struct {
	*walk.MainWindow

	input        *walk.TextEdit
	outputDir    *walk.LineEdit
	logBox       *walk.TextEdit
	status       *walk.Label
	progress     *walk.ProgressBar
	startBtn     *walk.PushButton
	syncBtn      *walk.PushButton
	cancelBtn    *walk.PushButton
	forumSummary *walk.Label

	// forum 是「发帖设置」弹框保存的论坛连接参数。
	forum forumSettings

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
	app.forum = normalizeForumSettings(cfg.Forum)
	debugLog("config loaded: %s", cfg.OutputDir)

	if err := (MainWindow{
		AssignTo: &app.MainWindow,
		Title:    appTitle,
		MinSize:  Size{Width: 760, Height: 520},
		Size:     Size{Width: 860, Height: 620},
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
					PushButton{Text: "发帖设置…", OnClicked: func() { app.openForumSettings() }},
					PushButton{AssignTo: &app.syncBtn, Text: "同步到论坛", OnClicked: app.startSync},
					PushButton{AssignTo: &app.cancelBtn, Text: "取消", Enabled: false, OnClicked: app.cancelDownload},
					PushButton{Text: "清空记录", OnClicked: app.clearLog},
					HSpacer{},
					Label{AssignTo: &app.status, Text: "就绪"},
				},
			},
			Label{
				AssignTo:  &app.forumSummary,
				Text:      app.forumSummaryText(),
				TextColor: walk.RGB(120, 120, 120),
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
	app.updateForumSummary()
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
	a.saveConfig()

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.running = true
	a.cancel = cancel
	a.mu.Unlock()

	a.startBtn.SetEnabled(false)
	a.syncBtn.SetEnabled(false)
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
				a.syncBtn.SetEnabled(true)
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

// startSync 解析输入框里的链接,把笔记批量发布到奶茶圈论坛。
// 与「开始下载」的区别:不写本地归档,而是抓取正文、上传图片并创建帖子。
func (a *downloaderApp) startSync() {
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

	cfg := normalizeForumSettings(a.forum)
	if cfg.Bot == "" || cfg.BotSecret == "" {
		// 首次同步或设置不完整时直接弹出发帖设置,保存后继续。
		if !a.openForumSettings() {
			return
		}
		cfg = normalizeForumSettings(a.forum)
	}
	if err := validateAPIBase(cfg.APIBase); err != nil {
		walk.MsgBox(a.MainWindow, appTitle,
			"同步目标地址无效:\n"+err.Error()+"\n\n请点「发帖设置」修改。",
			walk.MsgBoxIconWarning)
		return
	}
	if cfg.Bot == "" || cfg.BotSecret == "" {
		walk.MsgBox(a.MainWindow, appTitle,
			"机器人账号和 BOT_API_SECRET 不能为空,请点「发帖设置」填写。",
			walk.MsgBoxIconWarning)
		return
	}
	a.forum = cfg
	a.saveConfig()

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.running = true
	a.cancel = cancel
	a.mu.Unlock()

	a.startBtn.SetEnabled(false)
	a.syncBtn.SetEnabled(false)
	a.cancelBtn.SetEnabled(true)
	a.progress.SetRange(0, len(urls))
	a.progress.SetValue(0)
	_ = a.status.SetText(fmt.Sprintf("开始同步 %d 篇…", len(urls)))
	a.clearLog()
	a.appendLog(fmt.Sprintf("开始同步 %d 篇 → %s 分类:%s\r\n", len(urls), cfg.APIBase, cfg.Board))
	if invalid > 0 {
		a.appendLog(fmt.Sprintf("已忽略 %d 行无法识别的链接\r\n", invalid))
	}

	scraper := xhs.NewClient()
	forum := naicha.New(naicha.Config{
		APIBase:    cfg.APIBase,
		Bot:        cfg.Bot,
		BotSecret:  cfg.BotSecret,
		Board:      cfg.Board,
		Tags:       cfg.Tags,
		TimeoutSec: 120,
		StatePath:  naichaStatePath(),
	}, a.logger)
	opts := xhssync.Options{Board: cfg.Board, Tags: cfg.Tags}

	go func() {
		defer func() {
			cancel()
			a.Synchronize(func() {
				a.mu.Lock()
				a.running = false
				a.cancel = nil
				a.mu.Unlock()
				a.startBtn.SetEnabled(true)
				a.syncBtn.SetEnabled(true)
				a.cancelBtn.SetEnabled(false)
			})
		}()

		ok, skipped, failed := 0, 0, 0
		for i, rawURL := range urls {
			if ctx.Err() != nil {
				break
			}
			a.appendLogSynced(fmt.Sprintf("[%d/%d] %s\r\n", i+1, len(urls), rawURL))

			noteCtx, noteCancel := context.WithTimeout(ctx, perNoteSyncTimeout)
			note, err := scraper.Fetch(noteCtx, rawURL)
			var result xhssync.SyncResult
			if err == nil {
				result = xhssync.Sync(noteCtx, forum, scraper, note, opts, false)
			}
			noteCancel()

			switch {
			case err != nil:
				failed++
				a.appendLogSynced(fmt.Sprintf("  ✗ 抓取失败: %v\r\n", err))
			case !result.Publish.OK:
				failed++
				a.appendLogSynced(fmt.Sprintf("  ✗ 发布失败: %s\r\n", result.Publish.Message))
			case result.Publish.Duplicate:
				skipped++
				a.appendLogSynced(fmt.Sprintf("  • 已同步过: %s\r\n", result.Publish.URL))
			default:
				ok++
				a.appendLogSynced(fmt.Sprintf("  ✓ %s → %s(%d 张图片)\r\n", result.Publish.Title, result.Publish.URL, result.Publish.Images))
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

		summary := fmt.Sprintf("同步完成: 总数=%d 成功=%d 已同步=%d 失败=%d\r\n", len(urls), ok, skipped, failed)
		a.appendLogSynced("\r\n" + summary)
		a.Synchronize(func() {
			_ = a.status.SetText(fmt.Sprintf("同步完成:成功 %d,已同步 %d,失败 %d", ok, skipped, failed))
		})
	}()
}

// normalizeForumSettings 补齐论坛设置的默认值并去掉首尾空白。
func normalizeForumSettings(cfg forumSettings) forumSettings {
	cfg.APIBase = normalizeAPIBase(cfg.APIBase)
	cfg.Bot = strings.TrimSpace(cfg.Bot)
	cfg.BotSecret = strings.TrimSpace(cfg.BotSecret)
	cfg.Board = strings.TrimSpace(cfg.Board)
	if cfg.Board == "" {
		cfg.Board = defaultForumBoard
	}
	cfg.Tags = parseTagList(strings.Join(cfg.Tags, ","))
	return cfg
}

// buildForumSettings 校验弹框里填写的参数,并生成可保存的论坛设置。
func buildForumSettings(apiBase, bot, secret, board, tags string) (forumSettings, error) {
	cfg := normalizeForumSettings(forumSettings{
		APIBase:   apiBase,
		Bot:       bot,
		BotSecret: secret,
		Board:     board,
		Tags:      parseTagList(tags),
	})
	if err := validateAPIBase(cfg.APIBase); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// openForumSettings 弹出发帖设置对话框;保存成功返回 true。
// 弹框里的参数对应论坛机器人接口:
// POST /api/bot/posts + X-Bot-Secret,请求体 bot/boardSlug/title/content/manualTags。
func (a *downloaderApp) openForumSettings() bool {
	current := normalizeForumSettings(a.forum)
	var (
		dlg       *walk.Dialog
		apiBase   *walk.LineEdit
		bot       *walk.LineEdit
		secret    *walk.LineEdit
		board     *walk.LineEdit
		tags      *walk.LineEdit
		saveBtn   *walk.PushButton
		cancelBtn *walk.PushButton
	)
	saved := false

	save := func() {
		next, err := buildForumSettings(apiBase.Text(), bot.Text(), secret.Text(), board.Text(), tags.Text())
		if err != nil {
			walk.MsgBox(dlg, appTitle, "发帖设置无效:\n"+err.Error(), walk.MsgBoxIconWarning)
			return
		}
		a.forum = next
		a.saveConfig()
		a.updateForumSummary()
		saved = true
		dlg.Accept()
	}
	cancel := func() { dlg.Cancel() }

	if err := (Dialog{
		AssignTo:      &dlg,
		Title:         "发帖设置",
		MinSize:       Size{Width: 600, Height: 340},
		Size:          Size{Width: 660, Height: 360},
		DefaultButton: &saveBtn,
		CancelButton:  &cancelBtn,
		Layout: VBox{
			Margins: Margins{Left: 14, Top: 14, Right: 14, Bottom: 14},
			Spacing: 8,
		},
		Children: []Widget{
			Label{Text: "同步目标地址(例如 http://localhost:3000,可省略 http://):"},
			LineEdit{AssignTo: &apiBase, Text: current.APIBase, StretchFactor: 1},
			Label{Text: "机器人账号 bot(论坛后台创建的用户名/昵称/ID):"},
			LineEdit{AssignTo: &bot, Text: current.Bot, StretchFactor: 1},
			Label{Text: "BOT_API_SECRET(与论坛服务 .env 里的值一致):"},
			LineEdit{AssignTo: &secret, Text: current.BotSecret, PasswordMode: true, StretchFactor: 1},
			Composite{
				Layout: HBox{Spacing: 8},
				Children: []Widget{
					Label{Text: "分类 boardSlug:"},
					LineEdit{AssignTo: &board, Text: current.Board, StretchFactor: 1},
				},
			},
			Label{Text: "标签 manualTags(逗号分隔,可选;会与笔记标签合并):"},
			LineEdit{AssignTo: &tags, Text: strings.Join(current.Tags, ", "), StretchFactor: 1},
			Label{
				Text:      "接口:POST /api/bot/posts(X-Bot-Secret);标题/正文自动生成。",
				TextColor: walk.RGB(120, 120, 120),
			},
			Composite{
				Layout: HBox{Spacing: 8},
				Children: []Widget{
					HSpacer{},
					PushButton{AssignTo: &saveBtn, Text: "保存", OnClicked: save},
					PushButton{AssignTo: &cancelBtn, Text: "取消", OnClicked: cancel},
				},
			},
		},
	}).Create(a.MainWindow); err != nil {
		walk.MsgBox(a.MainWindow, appTitle, "打开发帖设置失败:\n"+err.Error(), walk.MsgBoxIconError)
		return false
	}

	dlg.Run()
	return saved
}

// forumSummaryText 返回主窗口里显示的发帖设置摘要(不包含密钥)。
func (a *downloaderApp) forumSummaryText() string {
	cfg := normalizeForumSettings(a.forum)
	if cfg.Bot == "" || cfg.BotSecret == "" {
		return "发帖设置未完成:点「发帖设置」填写同步目标、机器人账号和 BOT_API_SECRET。"
	}
	summary := fmt.Sprintf("发帖目标: %s · 机器人: %s · 分类: %s", cfg.APIBase, cfg.Bot, cfg.Board)
	if len(cfg.Tags) > 0 {
		summary += " · 标签: " + strings.Join(cfg.Tags, ", ")
	}
	return summary
}

// updateForumSummary 刷新主窗口里的发帖设置摘要。
func (a *downloaderApp) updateForumSummary() {
	if a.forumSummary == nil {
		return
	}
	_ = a.forumSummary.SetText(a.forumSummaryText())
}

// normalizeAPIBase 规范化同步目标地址:补全 http:// 并去掉末尾斜杠。
// 留空时回退到默认地址,因此可以填 localhost:3000、192.168.1.10:3000
// 或完整的 https://example.com/naicha。
func normalizeAPIBase(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultForumAPIBase
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return strings.TrimRight(raw, "/")
}

// validateAPIBase 校验同步目标地址是否可请求。
func validateAPIBase(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("地址格式不正确: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只支持 http/https 地址: %s", raw)
	}
	if strings.TrimSpace(u.Host) == "" {
		return fmt.Errorf("地址里缺少主机名: %s", raw)
	}
	return nil
}

// saveConfig 把界面上的保存目录和论坛设置写入配置文件。
// 只能在 UI 线程调用(要读取控件内容)。
func (a *downloaderApp) saveConfig() {
	saveConfig(appConfig{
		OutputDir: strings.TrimSpace(a.outputDir.Text()),
		Forum:     normalizeForumSettings(a.forum),
	})
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

// parseTagList 把逗号/分号/空白分隔的标签文本拆成去重后的标签列表。
func parseTagList(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ',', '，', ';', '；', '、', ' ', '\t', '\n', '\r':
			return true
		}
		return false
	})
	tags := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		tag := strings.TrimSpace(field)
		if tag == "" {
			continue
		}
		key := strings.ToLower(tag)
		if seen[key] {
			continue
		}
		seen[key] = true
		tags = append(tags, tag)
	}
	return tags
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

// naichaStatePath 返回小红书同步去重状态的存放路径。
// 放在程序配置目录里,换保存目录也不会重复发帖。
func naichaStatePath() string {
	path := configPath()
	if path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(path), "naicha_state.json")
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
