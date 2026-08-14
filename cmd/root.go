package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"wx-mp-sync/internal/app"
	"wx-mp-sync/internal/config"
	"wx-mp-sync/internal/winutil"
)

var (
	// Cfg 是全局配置,由 Execute 注入。
	Cfg *config.Config

	configFilepath string
	debug          bool
	// urls 由 sync/watch 的 --url 标志写入。
	urls []string

	// 插件相关标志。
	proxyPort      int
	adminPort      int
	outputFlag     string
	dataDirFlag    string
	injectMatch    string
	uninstallCert  bool
	noProxy        bool
	noInstallCert  bool
)

var rootCmd = &cobra.Command{
	Use:   "wx-mp-sync",
	Short: "公众号文章同步插件",
	Long:  "在微信 PC 客户端浏览公众号文章时注入「同步」按钮,一键把文章(HTML+Markdown+图片)归档到本地。",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Name() == "version" || cmd.Name() == "help" {
			return nil
		}
		return loadConfig()
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := buildAppOptions(cmd)
		if err != nil {
			return err
		}
		return app.Run(cmd.Context(), opts, Cfg.App.Logger)
	},
	SilenceErrors: true,
	SilenceUsage:  true,
}

func init() {
	// 禁用 cobra 的「mousetrap」:否则在 Windows 上双击快捷方式启动时,
	// cobra 会提示 "This is a command line tool" 并直接退出。
	cobra.MousetrapHelpText = ""

	rootCmd.PersistentFlags().StringVarP(&configFilepath, "config", "c", "", "配置文件路径(默认 ./config.yaml)")
	rootCmd.PersistentFlags().BoolVar(&debug, "debug", false, "开启调试日志")

	rootCmd.Flags().IntVar(&proxyPort, "proxy-port", 2023, "MITM 代理端口")
	rootCmd.Flags().IntVar(&adminPort, "admin-port", 8080, "本地管理服务端口")
	rootCmd.PersistentFlags().StringVar(&outputFlag, "output", "", "文章归档目录(默认取配置 output_dir)")
	rootCmd.Flags().StringVar(&dataDirFlag, "data-dir", "", "数据目录(存放根证书,默认 %APPDATA%/wx-mp-sync)")
	rootCmd.Flags().StringVar(&injectMatch, "inject-match", "mp.weixin.qq.com", "注入按钮匹配的域名(一般无需修改)")
	rootCmd.Flags().BoolVar(&uninstallCert, "uninstall-cert", false, "卸载根证书后退出")
	rootCmd.Flags().BoolVar(&noProxy, "no-proxy", false, "不自动设置系统代理(需手动配置)")
	rootCmd.Flags().BoolVar(&noInstallCert, "no-install-cert", false, "不自动安装根证书")

	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(watchCmd)
	rootCmd.AddCommand(versionCmd)
}

// Execute 是程序入口,由 main.go 调用。会注册 Ctrl+C 信号以优雅退出,
// 并注册控制台关闭处理(点窗口 × 时也能触发清理,恢复系统代理)。
func Execute(cfg *config.Config) error {
	Cfg = cfg
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 点击控制台窗口关闭按钮 / 注销 / 关机时,触发同样的清理逻辑
	_ = winutil.RegisterConsoleCloseHandler(stop)
	return rootCmd.ExecuteContext(ctx)
}

// loadConfig 读取配置文件并合并到全局配置,文件不存在时用默认值。
func loadConfig() error {
	if debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	if Cfg == nil {
		return fmt.Errorf("config not initialized")
	}

	path := configFilepath
	if path == "" {
		path = "config.yaml"
	}
	if _, err := os.Stat(path); err == nil {
		if err := Cfg.LoadFromFile(path); err != nil {
			return err
		}
	} else if configFilepath != "" {
		return fmt.Errorf("配置文件不存在: %s", path)
	}
	return Cfg.Normalize()
}

// buildAppOptions 由命令行标志 + 配置组装插件启动参数。
func buildAppOptions(cmd *cobra.Command) (app.Options, error) {
	if outputFlag != "" {
		Cfg.OutputDir = outputFlag
	}

	dataDir := dataDirFlag
	if dataDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base = "."
		}
		dataDir = filepath.Join(base, "wx-mp-sync")
	}

	logPath := ""
	if Cfg.App != nil {
		logPath = Cfg.App.LogPath
	}

	// 命令行标志未显式指定时,回退到 config.yaml(或默认值)
	proxyPortVal := proxyPort
	if !cmd.Flags().Changed("proxy-port") {
		proxyPortVal = Cfg.ProxyPort
	}
	adminPortVal := adminPort
	if !cmd.Flags().Changed("admin-port") {
		adminPortVal = Cfg.AdminPort
	}

	return app.Options{
		ProxyPort:      proxyPortVal,
		AdminPort:      adminPortVal,
		OutputDir:      Cfg.OutputDir,
		DataDir:        dataDir,
		LogPath:        logPath,
		InjectMatch:    injectMatch,
		InstallCert:    !noInstallCert,
		SetSystemProxy: !noProxy,
		UninstallCert:  uninstallCert,
		Version:        Cfg.App.Version,
		Bot:            Cfg.Bot,
	}, nil
}
