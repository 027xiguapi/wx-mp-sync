package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"wx-mp-sync/cmd"
	"wx-mp-sync/internal/config"
)

// AppVer 是构建时注入的版本号。
var AppVer = "260812"

func main() {
	logger, logFile, logPath, err := newAppLogger()
	if err != nil {
		fmt.Printf("Failed to initialize logger: %v\n", err)
		return
	}
	defer logFile.Close()

	cfg := config.New(AppVer, logger)
	cfg.App.LogPath = logPath
	if err := cmd.Execute(cfg); err != nil {
		fmt.Printf("Failed to run: %v\n", err.Error())
		// 双击快捷方式启动时,出错窗口会一闪而过;等待回车以便看清错误信息。
		fmt.Println("\n按回车键退出...")
		_, _ = fmt.Scanln()
	}
}

// newAppLogger 初始化 zerolog,日志写入 %TEMP%\wx_mp_sync\app.log。
func newAppLogger() (*zerolog.Logger, *os.File, string, error) {
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	zerolog.TimeFieldFormat = time.RFC3339Nano

	logDir := filepath.Join(os.TempDir(), "wx_mp_sync")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, nil, "", err
	}
	logPath := filepath.Join(logDir, "app.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return nil, nil, "", err
	}

	logger := zerolog.New(logFile).With().Timestamp().Logger()
	return &logger, logFile, logPath, nil
}
