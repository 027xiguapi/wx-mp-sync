package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"wx-mp-sync/internal/sync"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "同步公众号文章(执行一次)",
	Long:  "抓取并归档公众号文章。来源:配置文件的 articles/accounts,或 --url 直接指定。",
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := runSync(cmd.Context(), urls)
		if err != nil {
			return err
		}
		printResult(result)
		return nil
	},
}

func init() {
	syncCmd.Flags().StringSliceVar(&urls, "url", nil, "待同步的文章 URL,可重复指定(--url a --url b)")
}

// runSync 执行一次同步,返回统计结果。
func runSync(ctx context.Context, extra []string) (*sync.Result, error) {
	if outputFlag != "" {
		Cfg.OutputDir = outputFlag
	}
	if Cfg.App == nil || Cfg.App.Logger == nil {
		return nil, fmt.Errorf("logger not initialized")
	}
	s := sync.NewSyncer(Cfg, Cfg.App.Logger)
	return s.Run(ctx, extra)
}

// printResult 打印同步统计与明细。
func printResult(result *sync.Result) {
	if result == nil {
		return
	}
	logger := Cfg.App.Logger
	logger.Info().
		Int("total", result.Total).
		Int("synced", result.Synced).
		Int("skipped", result.Skipped).
		Int("failed", result.Failed).
		Msg("sync finished")

	fmt.Printf("同步结果: 总数=%d 新增=%d 已跳过=%d 失败=%d\n",
		result.Total, result.Synced, result.Skipped, result.Failed)
	for _, p := range result.Paths {
		fmt.Printf("  [已同步] %s\n", p)
	}
	for _, f := range result.Failures {
		fmt.Printf("  [失败] %s: %s\n", f.URL, f.Error)
	}
}
