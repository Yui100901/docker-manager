package main

import (
	"github.com/Yui100901/MyGo/command/docker"
	"github.com/Yui100901/MyGo/file_utils"
	"github.com/Yui100901/MyGo/log_utils"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"strings"
)

//
// @Author yfy2001
// @Date 2025/7/18 09 50
//

func newBuildCommand() *cobra.Command {
	var path string
	var export bool

	cmd := &cobra.Command{
		Use:   "build [options] <path>",
		Short: "构建Docker镜像，默认为当前目录",
		Run: func(cmd *cobra.Command, args []string) {
			if _, err := os.Stat(filepath.Join(path, "Dockerfile")); os.IsNotExist(err) {
				log_utils.Error.Fatal("Dockerfile does not exist!")
			}
			if err := build(path, export); err != nil {
				log_utils.Error.Fatalf("构建失败: %v", err)
			}
		},
	}
	cmd.Flags().StringVarP(&path, "path", "p", "", "路径参数")
	cmd.Flags().BoolVarP(&export, "export", "e", false, "是否导出容器")
	return cmd
}

func newCleanCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "清理Docker镜像",
		Run: func(cmd *cobra.Command, args []string) {
			if err := clean(); err != nil {
				log_utils.Error.Fatalf("Failed to clean images: %v", err)
			}
		},
	}
	return cmd
}

func newImportCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <path>",
		Short: "导入Docker镜像，默认从images，以及所有子目录寻找镜像",
		Run: func(cmd *cobra.Command, args []string) {
			path := "images"
			if len(args) > 0 {
				path = args[0]
			}
			if err := importImages(path); err != nil {
				log_utils.Error.Fatalf("Import failed: %v", err)
			}
		},
	}
	return cmd
}

func newExportCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export <path>",
		Short: "导出Docker镜像，默认为当前路径下的images",
		Run: func(cmd *cobra.Command, args []string) {
			path := "images"
			if len(args) > 0 {
				path = args[0]
			}
			if _, err := file_utils.CreateDirectory(path); err != nil {
				log_utils.Error.Fatalf("Create directory failed: %v", err)
			}
			if err := exportImages(path); err != nil {
				log_utils.Error.Fatalf("Export failed: %v", err)
			}
		},
	}
	return cmd
}

func build(path string, export bool) error {
	fileData, err := file_utils.NewFileData(path)
	if err != nil {
		return err
	}
	os.Chdir(fileData.AbsPath)
	if err := docker.BuildImage(fileData.Filename); err != nil {
		return err
	}
	if export {
		if err := docker.Save(fileData.Filename, "."); err != nil {
			return err
		}
	}
	return nil
}

func clean() error {
	return docker.ImagePrune()
}

func importImages(path string) error {
	fileData, err := file_utils.NewFileData(path)
	if err != nil {
		return err
	}
	files, _, err := file_utils.TraverseDirFiles(fileData.AbsPath, true)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := docker.Load(file.Path); err != nil {
			return err
		}
	}
	return nil
}

func exportImages(path string) error {
	if err := docker.ImagePrune(); err != nil {
		log_utils.Warn.Println("清理镜像发生错误！")
	}
	images, err := docker.ImageListFormatted()
	if err != nil {
		log_utils.Error.Println(err)
		return err
	}
	for _, image := range strings.Split(images, "\n") {
		if image != "" {
			strings.ReplaceAll(image, ":", "_")
			if err := docker.Save(image, path); err != nil {
				log_utils.Error.Printf("Failed to save image %s: %v", image, err)
			}
		}
	}
	return nil
}
