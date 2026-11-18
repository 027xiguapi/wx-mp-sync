// Package config 负责加载与持有公众号同步工具的全局配置。
// 结构参考 wx_channels_download-main/internal/config:统一配置入口 + 子模块命名空间。
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/viper"
)

// AppInfo 持有应用级元数据(版本号、日志器、日志路径),不参与配置文件解码。
type AppInfo struct {
	Version string
	LogPath string
	Logger  *zerolog.Logger
}

// Account 描述一个需要同步的公众号账号及其拉取凭证。
// 凭证(uin/key/pass_ticket/appmsg_token)会过期,失效后需重新抓包获取。
type Account struct {
	Name        string `mapstructure:"name" yaml:"name"`
	Biz         string `mapstructure:"biz" yaml:"biz"`
	Uin         string `mapstructure:"uin" yaml:"uin"`
	Key         string `mapstructure:"key" yaml:"key"`
	PassTicket  string `mapstructure:"pass_ticket" yaml:"pass_ticket"`
	AppmsgToken string `mapstructure:"appmsg_token" yaml:"appmsg_token"`
	AuthorID    string `mapstructure:"author_id" yaml:"author_id"`
	MaxArticles int    `mapstructure:"max_articles" yaml:"max_articles"`
}

// Config 是工具的完整配置。
type Config struct {
	App *AppInfo

	// OutputDir 归档根目录,默认 ./articles。
	OutputDir string
	// ProxyPort MITM 代理端口,默认 2023。
	ProxyPort int
	// AdminPort 本地管理服务端口,默认 8080。
	AdminPort int
	// Interval watch 模式的定时间隔,默认 30m。
	Interval time.Duration
	// Concurrency 并发抓取的文章数,默认 3。
	Concurrency int
	// TimeoutSec 单个 HTTP 请求超时秒数,默认 25。
	TimeoutSec int
	// BatchDelayMS 批量(多选)下载时两篇之间的间隔毫秒数,默认 900。
	BatchDelayMS int

	// Articles 是直接指定的文章 URL 列表(无需凭证)。
	Articles []string
	// Accounts 是需要通过 getmsg 接口自动拉取列表的公众号账号。
	Accounts []Account
	// Bot 是推送到外部 bot/内容服务的配置(可选)。
	Bot BotConfig
	// Naicha 是同步文章到奶茶圈论坛的配置(可选)。
	Naicha NaichaConfig
}

// BotConfig 描述推送文章到外部 bot 服务的配置。
type BotConfig struct {
	Enabled     bool   `mapstructure:"enabled" yaml:"enabled"`
	APIBase     string `mapstructure:"api_base" yaml:"api_base"`
	BotID       string `mapstructure:"bot_id" yaml:"bot_id"`
	Type        string `mapstructure:"type" yaml:"type"`
	UploadAPI   string `mapstructure:"upload_api" yaml:"upload_api"`
	UploadField string `mapstructure:"upload_field" yaml:"upload_field"`
	Cookie      string `mapstructure:"cookie" yaml:"cookie"`
	TimeoutSec  int    `mapstructure:"timeout_sec" yaml:"timeout_sec"`
}

// NaichaConfig 描述把归档文章/小红书笔记发布到奶茶圈论坛的配置。
// 公众号文章走旧版 POST /api/posts;小红书走管理员登录 + /api/posts/create。
type NaichaConfig struct {
	Enabled    bool     `mapstructure:"enabled" yaml:"enabled"`
	APIBase    string   `mapstructure:"api_base" yaml:"api_base"`
	Token      string   `mapstructure:"token" yaml:"token"`
	Category   string   `mapstructure:"category" yaml:"category"`
	Tags       []string `mapstructure:"tags" yaml:"tags"`
	Author     string   `mapstructure:"author" yaml:"author"`
	Auto       bool     `mapstructure:"auto" yaml:"auto"`
	TimeoutSec int      `mapstructure:"timeout_sec" yaml:"timeout_sec"`

	// 复用管理员登录发布小红书笔记时使用:
	// POST /api/auth/login → POST /api/upload → POST /api/posts/create。
	Username string `mapstructure:"username" yaml:"username"`
	Password string `mapstructure:"password" yaml:"password"`
	Cookie   string `mapstructure:"cookie" yaml:"cookie"` // 可选:bbs_session 的值或完整 Cookie
	Board    string `mapstructure:"board" yaml:"board"`   // 小红书发帖分类 boardSlug,默认 new-releases

	XHSEnabled *bool    `mapstructure:"xhs_enabled" yaml:"xhs_enabled"` // 默认 true
	XHSTags    []string `mapstructure:"xhs_tags" yaml:"xhs_tags"`
	XHSCookie  string   `mapstructure:"xhs_cookie" yaml:"xhs_cookie"` // 抓取小红书页面用的 Cookie
}

// XHSActive 表示是否启用「小红书 → 奶茶圈」同步。
func (c NaichaConfig) XHSActive() bool {
	if !c.Enabled {
		return false
	}
	return c.XHSEnabled == nil || *c.XHSEnabled
}

// fileConfig 只包含可从 config.yaml 解码的字段,避免污染 AppInfo。
type fileConfig struct {
	OutputDir    string        `mapstructure:"output_dir"`
	ProxyPort    int           `mapstructure:"proxy_port"`
	AdminPort    int           `mapstructure:"admin_port"`
	Interval     time.Duration `mapstructure:"interval"`
	Concurrency  int           `mapstructure:"concurrency"`
	TimeoutSec   int           `mapstructure:"timeout_sec"`
	BatchDelayMS int           `mapstructure:"batch_delay_ms"`
	Articles     []string      `mapstructure:"articles"`
	Accounts     []Account     `mapstructure:"accounts"`
	Bot          BotConfig     `mapstructure:"bot_sync"`
	Naicha       NaichaConfig  `mapstructure:"naicha_sync"`
}

// New 创建一个带默认值的 Config。
func New(version string, logger *zerolog.Logger) *Config {
	return &Config{
		App: &AppInfo{
			Version: version,
			Logger:  logger,
		},
		OutputDir:    "./articles",
		ProxyPort:    2023,
		AdminPort:    8080,
		Interval:     30 * time.Minute,
		Concurrency:  3,
		TimeoutSec:   25,
		BatchDelayMS: 900,
	}
}

// LoadFromFile 从指定的 YAML 配置文件加载配置并合并到 cfg。
// 文件不存在时静默使用默认值;文件存在但解析失败则返回错误。
func (c *Config) LoadFromFile(path string) error {
	logger := c.App.Logger
	v := viper.New()
	v.SetConfigFile(path)

	err := v.ReadInConfig()
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) || os.IsNotExist(err) {
			logger.Warn().Str("path", path).Msg("配置文件不存在,使用默认配置")
			return nil
		}
		return fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	var fc fileConfig
	if err := v.Unmarshal(&fc); err != nil {
		return fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}

	if fc.OutputDir != "" {
		c.OutputDir = fc.OutputDir
	}
	if fc.ProxyPort > 0 {
		c.ProxyPort = fc.ProxyPort
	}
	if fc.AdminPort > 0 {
		c.AdminPort = fc.AdminPort
	}
	if fc.Interval > 0 {
		c.Interval = fc.Interval
	}
	if fc.Concurrency > 0 {
		c.Concurrency = fc.Concurrency
	}
	if fc.TimeoutSec > 0 {
		c.TimeoutSec = fc.TimeoutSec
	}
	if fc.BatchDelayMS > 0 {
		c.BatchDelayMS = fc.BatchDelayMS
	}
	c.Articles = fc.Articles
	c.Accounts = fc.Accounts
	c.Bot = fc.Bot
	c.Naicha = fc.Naicha
	return nil
}

// Normalize 补齐字段默认值并做合法性检查,在同步开始前调用。
func (c *Config) Normalize() error {
	if c.OutputDir == "" {
		c.OutputDir = "./articles"
	}
	if c.ProxyPort <= 0 {
		c.ProxyPort = 2023
	}
	if c.AdminPort <= 0 {
		c.AdminPort = 8080
	}
	if c.Interval <= 0 {
		c.Interval = 30 * time.Minute
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 3
	}
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = 25
	}
	if c.BatchDelayMS <= 0 {
		c.BatchDelayMS = 900
	}
	if c.Naicha.APIBase == "" {
		c.Naicha.APIBase = "http://localhost:3002"
	}
	if c.Naicha.Category == "" {
		c.Naicha.Category = "discussion"
	}
	if c.Naicha.TimeoutSec <= 0 {
		c.Naicha.TimeoutSec = 60
	}
	return nil
}
