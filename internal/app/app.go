// Package app 编排公众号同步插件:证书 → 管理服务 → MITM 代理 → 系统代理 → 优雅退出恢复。
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/rs/zerolog"

	"wx-mp-sync/internal/bot"
	"wx-mp-sync/internal/config"
	"wx-mp-sync/internal/proxy"
	"wx-mp-sync/internal/server"
	"wx-mp-sync/internal/store"
	"wx-mp-sync/internal/sync"
	"wx-mp-sync/internal/sysproxy"
)

// Options 是插件的启动参数。
type Options struct {
	ProxyPort      int
	AdminPort      int
	OutputDir      string
	DataDir        string
	LogPath        string
	InjectMatch    string          // 注入匹配域名,默认 mp.weixin.qq.com
	InstallCert    bool            // 自动安装根证书
	SetSystemProxy bool            // 自动设置系统代理
	UninstallCert  bool            // 仅卸载证书后退出
	Version        string
	Bot            config.BotConfig // bot 推送配置
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

	// --- 管理服务 ---
	adminBase := fmt.Sprintf("http://127.0.0.1:%d", opts.AdminPort)
	botAPI := ""
	if botSyncer != nil {
		botAPI = opts.Bot.APIBase + "/api/bot/sync/articles"
	}
	status := &server.Status{
		Version:   opts.Version,
		AdminAddr: adminBase,
		OutputDir: opts.OutputDir,
		CertName:  proxy.CertName,
		BotAPI:    botAPI,
	}
	srv := server.New(opts.OutputDir, opts.LogPath, saver, botSyncer, logger)

	// --- MITM 代理 ---
	p, err := proxy.NewProxy(rootCA, adminBase)
	if err != nil {
		return err
	}
	injectMatch := opts.InjectMatch
	if injectMatch == "" {
		injectMatch = "mp.weixin.qq.com"
	}
	p.AddPlugin(proxy.NewInjectPlugin(proxy.InjectOptions{
		Match:     injectMatch,
		AdminBase: adminBase,
	}))

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
		if err := sysproxy.Enable("127.0.0.1", strconv.Itoa(opts.ProxyPort)); err != nil {
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
	if !opts.InstallCert {
		fmt.Println("  证书:     跳过自动安装")
	}
	fmt.Println("\n请在微信 PC 客户端打开公众号文章,点击右下角「⬇ 同步」按钮。Ctrl+C 退出。")

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
