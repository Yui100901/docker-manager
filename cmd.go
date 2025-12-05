package main

import (
	"docker-manager/docker"
	"path/filepath"
	"strings"

	"github.com/Yui100901/MyGo/file_utils"
	"github.com/Yui100901/MyGo/log_utils"
	"github.com/spf13/cobra"
)

//
// @Author yfy2001
// @Date 2025/7/18 09 50
//

func newLoadCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import [path]",
		Short: "导入Docker镜像，默认从images，以及所有子目录寻找镜像",
		Run: func(cmd *cobra.Command, args []string) {
			path := "images"
			if len(args) > 0 {
				path = args[0]
			}
			if err := loadImages(path); err != nil {
				log_utils.Error.Fatalf("Import failed: %v", err)
			}
		},
	}
	return cmd
}

func newSaveCommand() *cobra.Command {
	var merge bool
	cmd := &cobra.Command{
		Use:   "export [path] [options]",
		Short: "导出Docker镜像，默认为当前路径下的images。",
		Run: func(cmd *cobra.Command, args []string) {
			path := "images"
			if len(args) > 0 {
				path = args[0]
			}
			if _, err := file_utils.CreateDirectory(path); err != nil {
				log_utils.Error.Fatalf("Create directory failed: %v", err)
			}
			if err := saveImages(path, merge); err != nil {
				log_utils.Error.Fatalf("Export failed: %v", err)
			}
		},
	}
	cmd.Flags().BoolVarP(&merge, "merge", "m", true, "合并成一个文件images.tar")
	return cmd
}

func loadImages(path string) error {
	fileData, err := file_utils.NewFileData(path)
	if err != nil {
		return err
	}
	files, _, err := file_utils.TraverseDirFiles(fileData.AbsPath, true)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := docker.LoadImage(file.Path); err != nil {
			return err
		}
	}
	return nil
}

func saveImages(path string, merge bool) error {
	images, err := docker.ListImages()
	if err != nil {
		log_utils.Error.Println(err)
		return err
	}
	imageMap := make(map[string]string)
	for _, image := range images {
		if len(image.RepoTags) > 0 {
			imageID := image.ID
			imageName := image.RepoTags[0]
			imageName = strings.ReplaceAll(imageName, "/", "_")
			imageName = strings.ReplaceAll(imageName, ":", "-")
			imageMap[imageID] = imageName
		}
	}
	if merge {
		imageIDList := make([]string, 0, len(imageMap))
		for imageID := range imageMap {
			imageIDList = append(imageIDList, imageID)
		}
		return docker.SaveImages(imageIDList, "images.tar")
	} else {
		for imageID, imageName := range imageMap {
			err := docker.SaveImages([]string{imageID}, filepath.Join(path, imageName))
			if err != nil {
				log_utils.Error.Println(err)
			}
		}
	}
	return nil
}
