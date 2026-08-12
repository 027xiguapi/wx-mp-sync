package cmd

import (
	"time"

	"github.com/spf13/cobra"
)

var watchInterval time.Duration

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "定时同步公众号文章",
	Long:  "按配置文件的 interval(或 --interval)循环同步,Ctrl+C 退出。",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWatch(cmd)
	},
}

func init() {
	watchCmd.Flags().DurationVar(&watchInterval, "interval", 0, "同步间隔,如 30m/1h(默认取配置文件 interval)")
	watchCmd.Flags().StringSliceVar(&urls, "url", nil, "待同步的文章 URL,可重复指定")
}

// runWatch 进入定时同步循环。
func runWatch(cmd *cobra.Command) error {
	interval := Cfg.Interval
	if watchInterval > 0 {
		interval = watchInterval
	}
	logger := Cfg.App.Logger
	logger.Info().Dur("interval", interval).Msg("watch mode started")

	for {
		result, err := runSync(cmd.Context(), urls)
		if err != nil {
			logger.Error().Err(err).Msg("本轮同步失败")
		} else {
			printResult(result)
		}

		select {
		case <-time.After(interval):
		case <-cmd.Context().Done():
			logger.Info().Msg("收到退出信号,watch 结束")
			return nil
		}
	}
}
