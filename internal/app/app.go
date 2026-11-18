// Package app 编排公众号同步插件:证书 → 管理服务 → MITM 代理 → 系统代理 → 优雅退出恢复。
package app

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"wx-mp-sync/internal/bot"
	"wx-mp-sync/internal/capture"
	chatgptarchive "wx-mp-sync/internal/chatgpt"
	"wx-mp-sync/internal/config"
	doubaoarchive "wx-mp-sync/internal/doubao"
	"wx-mp-sync/internal/naicha"
	"wx-mp-sync/internal/proxy"
	"wx-mp-sync/internal/server"
	"wx-mp-sync/internal/store"
	"wx-mp-sync/internal/sync"
	"wx-mp-sync/internal/sysproxy"
	xhsarchive "wx-mp-sync/internal/xhsarchive"
	scraperchatgpt "wx-mp-sync/pkg/scraper/chatgpt"
	"wx-mp-sync/pkg/scraper/xhs"
)

// Options 是插件的启动参数。
type Options struct {
	ProxyPort      int
	AdminPort      int
	OutputDir      string
	DataDir        string
	LogPath        string
	InjectMatch    string // 注入匹配域名,默认 mp.weixin.qq.com;逗号分隔可配多个
	InstallCert    bool   // 自动安装根证书
	SetSystemProxy bool   // 自动设置系统代理
	UninstallCert  bool   // 仅卸载证书后退出
	Version        string
	BatchDelayMS   int                 // 批量下载两篇之间的间隔毫秒数
	Bot            config.BotConfig    // bot 推送配置
	Naicha         config.NaichaConfig // 奶茶圈论坛同步配置
	UpstreamProxy  string              // 上级代理(如 Clash),留空时自动读取系统代理
}

// Run 启动插件并阻塞直到 ctx 取消。
func Run(ctx context.Context, opts Options, logger *zerolog.Logger) error {
	if opts.UninstallCert {
		fmt.Println("卸载根证书…")
		if err := proxy.UninstallCA(proxy.CertName); err != nil {
			return fmt.Errorf("卸载证书失败: %w", err)
		}
		fmt.Println("根证书已卸载")
		return nil
	}

	// --- 证书 ---
	rootCA, err := proxy.LoadOrCreateRootCA(opts.DataDir)
	if err != nil {
		return fmt.Errorf("初始化证书失败: %w", err)
	}
	if opts.InstallCert {
		installed, err := proxy.CheckCAInstalled(proxy.CertName)
		if err != nil {
			logger.Warn().Err(err).Msg("检查证书状态失败")
			fmt.Println("⚠️ 无法读取系统证书库,HTTPS 注入可能不生效。可右键「以管理员身份运行」本程序。")
		}
		if err == nil && !installed {
			der, derr := rootCA.CertDER()
			if derr != nil {
				return derr
			}
			fmt.Println("首次运行,需要把根证书安装到系统(将弹出管理员授权窗口)…")
			if err := proxy.InstallCA(der); err != nil {
				logger.Error().Err(err).Msg("安装根证书失败,HTTPS 页面注入将不生效")
				fmt.Println("❌ 根证书未安装成功,微信里的「同步」按钮将不会出现。")
				fmt.Println("   请关闭本窗口,然后右键 wx-mp-sync.exe 选择「以管理员身份运行」重试,")
				fmt.Println("   或手动安装证书: 双击 " + rootCA.CertPath() + " 并选「受信任的根证书颁发机构」。")
			} else {
				fmt.Println("✔ 根证书安装成功")
			}
		}
	}

	// --- 输出目录与状态存储 ---
	if err := os.MkdirAll(opts.OutputDir, 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}
	st, err := store.New(filepath.Join(opts.OutputDir, ".wx_mp_sync_state.json"))
	if err != nil {
		return fmt.Errorf("初始化状态存储失败: %w", err)
	}

	cfg := config.New(opts.Version, logger)
	cfg.OutputDir = opts.OutputDir
	saver := sync.NewArticleSaver(cfg, st, logger)

	// bot 推送(可选)
	botSyncer := bot.New(bot.Config{
		Enabled:     opts.Bot.Enabled,
		APIBase:     opts.Bot.APIBase,
		BotID:       opts.Bot.BotID,
		Type:        opts.Bot.Type,
		UploadAPI:   opts.Bot.UploadAPI,
		UploadField: opts.Bot.UploadField,
		Cookie:      opts.Bot.Cookie,
		TimeoutSec:  opts.Bot.TimeoutSec,
	}, logger)

	// 奶茶圈论坛同步(可选):把归档文章发布到 POST /api/posts。
	var naichaClient *naicha.Client
	if opts.Naicha.Enabled {
		naichaClient = naicha.New(naicha.Config{
			APIBase:    opts.Naicha.APIBase,
			Token:      opts.Naicha.Token,
			Category:   opts.Naicha.Category,
			Tags:       opts.Naicha.Tags,
			Author:     opts.Naicha.Author,
			Auto:       opts.Naicha.Auto,
			TimeoutSec: opts.Naicha.TimeoutSec,
			StatePath:  filepath.Join(opts.OutputDir, ".naicha_sync_state.json"),
			Username:   opts.Naicha.Username,
			Password:   opts.Naicha.Password,
			Cookie:     opts.Naicha.Cookie,
			Board:      naichaBoard(opts.Naicha),
		}, logger)
	}

	// --- 管理服务 ---
	adminBase := fmt.Sprintf("http://127.0.0.1:%d", opts.AdminPort)
	botAPI := ""
	if botSyncer != nil {
		botAPI = opts.Bot.APIBase + "/api/bot/sync/articles"
	}
	naichaAPI := ""
	naichaAuto := false
	if naichaClient != nil {
		naichaAPI = naichaClient.APIBase()
		naichaAuto = opts.Naicha.Auto
	}
	status := &server.Status{
		Version:    opts.Version,
		AdminAddr:  adminBase,
		OutputDir:  opts.OutputDir,
		CertName:   proxy.CertName,
		BotAPI:     botAPI,
		NaichaAPI:  naichaAPI,
		NaichaAuto: naichaAuto,
	}
	srv := server.New(opts.OutputDir, opts.LogPath, saver, botSyncer, logger)
	srv.SetNaicha(naichaClient)

	// 小红书抓取器:本地归档始终可用;发帖只是复用同一个抓取器。
	xhsScraper := xhs.NewClient()
	xhsScraper.Cookie = opts.Naicha.XHSCookie
	srv.SetXHS(xhsScraper, naichaBoard(opts.Naicha), opts.Naicha.XHSTags)
	srv.SetXHSArchiver(xhsarchive.NewArchiver(opts.OutputDir, xhsScraper, logger))
	status.XHSDownloadEnabled = true

	// 小红书 → 奶茶圈:复用管理员登录,抓取笔记正文和图片后发帖。
	xhsEnabled := false
	if opts.Naicha.XHSActive() {
		xhsEnabled = naichaClient != nil && naichaClient.SessionReady()
	}
	status.XHSEnabled = xhsEnabled
	status.NaichaBoard = naichaBoard(opts.Naicha)

	if opts.BatchDelayMS > 0 {
		srv.SetBatchDelay(time.Duration(opts.BatchDelayMS) * time.Millisecond)
	}
	// 豆包分享归档:Markdown + 图片 + metadata.json。
	srv.SetDoubaoArchiver(doubaoarchive.NewArchiver(opts.OutputDir, logger))

	// 上级代理:优先用显式配置,否则在接管系统代理前读取现有设置(通常是 Clash)。
	upstream := opts.UpstreamProxy
	if upstream == "" {
		if cur, err := sysproxy.Fetch(); err == nil && cur.Enabled() {
			if !isSelfProxy(cur, opts.ProxyPort) {
				upstream = "http://" + net.JoinHostPort(cur.Hostname, cur.Port)
			}
		}
	}
	// ChatGPT 分享归档:需要访问 chatgpt.com,没有上级代理时多半连不上。
	chatgptClient := scraperchatgpt.NewClient()
	chatgptClient.Proxy = upstream
	srv.SetChatGPTArchiver(chatgptarchive.NewArchiver(opts.OutputDir, chatgptClient, chatgptClient.HTTPClient(), logger))
	// 捕获公众号凭证:用于管理页「公众号最新文章」批量下载。
	captureStore := capture.NewStore(12 * time.Hour)
	srv.SetCapture(captureStore)

	// --- MITM 代理 ---
	p, err := proxy.NewProxy(rootCA, adminBase, upstream)
	if err != nil {
		return err
	}
	injectMatch := opts.InjectMatch
	if injectMatch == "" {
		injectMatch = "mp.weixin.qq.com,weixin.sogou.com,www.doubao.com,chatgpt.com,chat.openai.com"
		// 只有启用「小红书 → 奶茶圈」时才注入小红书页面按钮,避免无用按钮。
		if opts.Naicha.XHSActive() {
			injectMatch += ",www.xiaohongshu.com"
		}
	}
	// 支持逗号分隔多个域名(例如把搜狗微信搜索页也纳入注入范围)。
	for _, match := range splitInjectMatch(injectMatch) {
		if isDoubaoMatch(match) {
			p.AddPlugin(proxy.NewDoubaoInjectPlugin(proxy.InjectOptions{
				Match:     match,
				AdminBase: adminBase,
			}))
			continue
		}
		if isChatGPTMatch(match) {
			p.AddPlugin(proxy.NewChatGPTInjectPlugin(proxy.InjectOptions{
				Match:     match,
				AdminBase: adminBase,
			}))
			continue
		}
		if isXHSMatch(match) {
			if xhsEnabled {
				p.AddPlugin(proxy.NewXHSInjectPlugin(proxy.InjectOptions{
					Match:     match,
					AdminBase: adminBase,
				}))
			}
			continue
		}
		p.AddPlugin(proxy.NewInjectPlugin(proxy.InjectOptions{
			Match:     match,
			AdminBase: adminBase,
		}))
	}
	// 捕获公众号凭证(仍由 HTTP 代理观察 mp.weixin.qq.com 的请求)。
	p.AddPlugin(proxy.NewCapturePlugin(captureStore))

	// 先启动服务,再设置系统代理,避免代理指向空端口。
	if err := srv.Start(opts.AdminPort); err != nil {
		return fmt.Errorf("启动管理服务失败: %w", err)
	}
	defer srv.Stop()
	if err := p.Start(opts.ProxyPort); err != nil {
		return fmt.Errorf("启动代理服务失败: %w", err)
	}
	defer p.Stop()
	proxyAddr := p.Addr()
	status.ProxyAddr = proxyAddr

	// --- 系统代理 ---
	var savedProxy *sysproxy.Settings
	if opts.SetSystemProxy {
		if cur, err := sysproxy.Fetch(); err == nil {
			savedProxy = cur
		}
		if err := sysproxy.Enable("127.0.0.1", strconv.Itoa(p.Port())); err != nil {
			logger.Error().Err(err).Msg("设置系统代理失败(可用 --no-proxy 手动配置)")
		} else {
			status.SystemProxy = proxyAddr
			logger.Info().Str("addr", proxyAddr).Msg("系统代理已设置")
		}
	}
	srv.SetStatus(status)

	// --- 启动信息 ---
	fmt.Printf("\nv%s 公众号文章同步插件\n", opts.Version)
	fmt.Printf("  代理服务: %s\n", proxyAddr)
	fmt.Printf("  管理页面: %s\n", adminBase)
	if status.SystemProxy != "" {
		fmt.Printf("  系统代理: 已设置到 %s(退出时自动恢复)\n", proxyAddr)
	} else {
		fmt.Printf("  系统代理: 未设置\n")
	}
	if upstream != "" {
		fmt.Printf("  上级代理: %s\n", upstream)
	}
	if naichaClient != nil {
		auto := ""
		if opts.Naicha.Auto {
			auto = ",保存后自动同步"
		}
		fmt.Printf("  奶茶圈:   %s%s\n", naichaClient.APIBase(), auto)
		if xhsEnabled {
			fmt.Printf("  小红书:   已启用,发帖分类 %s(管理页粘贴笔记链接,或在笔记页点「⬆ 同步到奶茶圈」)\n", naichaBoard(opts.Naicha))
		} else if opts.Naicha.XHSActive() {
			fmt.Println("  小红书:   等待配置管理员 username / password(或 cookie)")
		}
	}
	fmt.Println("  小红书本地: 已启用(管理页「批量下载小红书到本地」,或 CLI: wx-mp-sync xhs --file links.txt)")
	if !opts.InstallCert {
		fmt.Println("  证书:     跳过自动安装")
	}
	fmt.Println("\n请在微信 PC 客户端打开公众号文章,点右下角「⬇ 同步」归档,或「⬇ 下载」直接下载 ZIP。Ctrl+C 退出。")
	fmt.Println("豆包分享:在浏览器打开 www.doubao.com/thread/... 页面,点右下角「⬇ 下载本页」;也可在管理页批量粘贴链接。")
	fmt.Println("ChatGPT 分享:打开 chatgpt.com/s/... 页面点「⬇ 下载本页」,或在管理页粘贴链接;chatgpt.com 通常需要 --upstream-proxy 或系统代理。")
	fmt.Println("若微信里没有出现按钮,请关闭文章窗口后重新打开(旧页面不会重新注入)。")

	<-ctx.Done()
	fmt.Println("\n正在退出,恢复系统代理…")
	if opts.SetSystemProxy {
		if err := sysproxy.Restore(savedProxy); err != nil {
			logger.Warn().Err(err).Msg("恢复系统代理失败,请手动检查网络设置")
		} else {
			fmt.Println("系统代理已恢复")
		}
	}
	return nil
}

// isDoubaoMatch 判断注入域名是否豆包;豆包页面使用单独的下载按钮脚本。
func isDoubaoMatch(pattern string) bool {
	return strings.Contains(strings.ToLower(pattern), "doubao.com")
}

// isChatGPTMatch 判断注入域名是否 ChatGPT;使用单独的分享下载脚本。
func isChatGPTMatch(pattern string) bool {
	p := strings.ToLower(pattern)
	return strings.Contains(p, "chatgpt.com") || strings.Contains(p, "chat.openai.com")
}

// isXHSMatch 判断注入域名是否小红书;使用单独的笔记同步脚本。
func isXHSMatch(pattern string) bool {
	return strings.Contains(strings.ToLower(pattern), "xiaohongshu.com")
}

// naichaBoard 返回小红书同步使用的发帖分类,默认 new-releases。
func naichaBoard(cfg config.NaichaConfig) string {
	board := strings.TrimSpace(cfg.Board)
	if board == "" {
		board = "new-releases"
	}
	return board
}

// isSelfProxy 判断系统代理是否指向本程序自己的代理端口(避免自我循环)。
func isSelfProxy(s *sysproxy.Settings, proxyPort int) bool {
	if s == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(s.Hostname))
	return (host == "127.0.0.1" || host == "localhost") && s.Port == strconv.Itoa(proxyPort)
}

// splitInjectMatch 把逗号/分号/空白分隔的注入域名拆开,去重并忽略空项。
func splitInjectMatch(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', ' ', '\t', '\n', '\r':
			return true
		}
		return false
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return []string{"mp.weixin.qq.com", "weixin.sogou.com"}
	}
	return out
}
