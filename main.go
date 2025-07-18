package main

import (
	"docker-manager/pull"
	"docker-manager/reverse"
	"fmt"
	"github.com/spf13/cobra"
	"os"
)

//
// @Author yfy2001
// @Date 2025/7/18 09 43
//

func main() {
	rootCmd := &cobra.Command{
		Use:   "dm <command>",
		Short: "Docker小工具，可用于管理容器.\nAuthor:Yui",
		Run: func(cmd *cobra.Command, args []string) {
			err := cmd.Help()
			if err != nil {
				return
			}
		},
	}

	rootCmd.AddCommand(newBuildCommand())
	rootCmd.AddCommand(newCleanCommand())
	rootCmd.AddCommand(newImportCommand())
	rootCmd.AddCommand(newExportCommand())
	rootCmd.AddCommand(reverse.NewReverseCommand())
	rootCmd.AddCommand(pull.NewPullCommand())
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
