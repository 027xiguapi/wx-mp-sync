package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "打印版本号",
	Run: func(cmd *cobra.Command, args []string) {
		version := "unknown"
		if Cfg != nil && Cfg.App != nil && Cfg.App.Version != "" {
			version = Cfg.App.Version
		}
		fmt.Printf("wx-mp-sync %s\n", version)
	},
}
