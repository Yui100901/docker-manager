package main

import (
	"docker-manager/docker"
	"docker-manager/pull"
	"docker-manager/reverse"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
)

//
// @Author yfy2001
// @Date 2025/7/18 09 43
//

func main() {
	// 初始化 managers 并注入到各个包
	im, err := docker.NewImageManager()
	if err != nil {
		log.Fatalf("无法初始化 Docker ImageManager: %v", err)
	}
	// 将 imageManager 注入到本包的变量（在 cmd.go）
	imageManager = im

	cm, err := docker.NewContainerManager()
	if err != nil {
		log.Fatalf("无法初始化 Docker ContainerManager: %v", err)
	}
	reverse.SetContainerManager(cm)

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

	rootCmd.AddCommand(newLoadCommand())
	rootCmd.AddCommand(newSaveCommand())
	rootCmd.AddCommand(reverse.NewReverseCommand())
	rootCmd.AddCommand(pull.NewPullCommand())
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
