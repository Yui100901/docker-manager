package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Yui100901/MyGo/file_utils"
	"github.com/docker/docker/api/types/image"
	"github.com/spf13/cobra"
)

//
// @Author yfy2001
// @Date 2025/7/18 09 50
//

type imageService interface {
	List(all bool) ([]image.Summary, error)
	Save(images []string, outputFile string) error
	Load(inputFile string) error
}

var imageManager imageService

func newLoadCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "load [path]",
		Short: "导入Docker镜像，默认从images，以及所有子目录寻找镜像",
		Run: func(cmd *cobra.Command, args []string) {
			path := "images"
			if len(args) > 0 {
				path = args[0]
			}
			if err := loadImages(path); err != nil {
				log.Fatalf("Import failed: %v", err)
			}
		},
	}
	return cmd
}

func newSaveCommand() *cobra.Command {
	var merge bool
	var all bool
	cmd := &cobra.Command{
		Use:   "save [path] [options]",
		Short: "导出Docker镜像，默认为当前路径下的images。",
		Run: func(cmd *cobra.Command, args []string) {
			path := "images"
			if len(args) > 0 {
				path = args[0]
			}
			if _, err := file_utils.CreateDirectory(path); err != nil {
				log.Fatalf("Create directory failed: %v", err)
			}
			if err := saveImages(path, merge, all); err != nil {
				log.Fatalf("Export failed: %v", err)
			}
		},
	}
	cmd.Flags().BoolVarP(&merge, "merge", "m", false, "合并成一个文件images.tar")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "导出所有镜像，包括无tag镜像")
	return cmd
}

func loadImages(path string) error {
	discovery, err := findDockerImageArchives(path)
	if err != nil {
		return err
	}
	total := len(discovery.Archives)
	log.Printf("Load images: found=%d skipped=%d path=%s", total, discovery.Skipped, path)

	var loadErrs []error
	success := 0
	for i, archive := range discovery.Archives {
		log.Printf("Load image archive [%d/%d]: %s", i+1, total, archive)
		if err := imageManager.Load(archive); err != nil {
			wrappedErr := fmt.Errorf("load image archive %s: %w", archive, err)
			log.Println(wrappedErr)
			loadErrs = append(loadErrs, wrappedErr)
			continue
		}
		success++
	}
	failed := len(loadErrs)
	log.Printf("Load summary: found=%d success=%d failed=%d skipped=%d", total, success, failed, discovery.Skipped)
	return errors.Join(loadErrs...)
}

type imageArchiveDiscovery struct {
	Archives []string
	Skipped  int
}

func findDockerImageArchives(path string) (imageArchiveDiscovery, error) {
	info, err := os.Stat(path)
	if err != nil {
		return imageArchiveDiscovery{}, err
	}
	if !info.IsDir() {
		if isDockerImageArchive(path) {
			return imageArchiveDiscovery{Archives: []string{path}}, nil
		}
		log.Printf("Skip non-image archive: %s", path)
		return imageArchiveDiscovery{Skipped: 1}, nil
	}

	var archives []string
	skipped := 0
	err = filepath.WalkDir(path, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !isDockerImageArchive(filePath) {
			log.Printf("Skip non-image archive: %s", filePath)
			skipped++
			return nil
		}
		archives = append(archives, filePath)
		return nil
	})
	return imageArchiveDiscovery{Archives: archives, Skipped: skipped}, err
}

func isDockerImageArchive(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(name, ".tar") ||
		strings.HasSuffix(name, ".tar.gz") ||
		strings.HasSuffix(name, ".tgz")
}

func saveImages(path string, merge bool, all bool) error {
	images, err := imageManager.List(all)
	if err != nil {
		log.Println(err)
		return err
	}
	imageMap := make(map[string]string)
	skipped := 0
	for _, image := range images {
		imageID := image.ID
		if len(image.RepoTags) > 0 {
			imageName := image.RepoTags[0]
			imageName = strings.ReplaceAll(imageName, "/", "_")
			imageName = strings.ReplaceAll(imageName, ":", "-")
			imageMap[imageID] = imageName
		} else {
			if all {
				imageMap[imageID] = strings.ReplaceAll(imageID, ":", "_")
			} else {
				skipped++
			}
		}
	}
	for imageID, imageName := range imageMap {
		log.Println("Export image", imageID, imageName)
	}
	total := len(imageMap)
	log.Printf("Save images: total=%d skipped=%d merge=%v output=%s", total, skipped, merge, path)
	if merge {
		imageIDList := make([]string, 0, len(imageMap))
		for imageID := range imageMap {
			imageIDList = append(imageIDList, imageID)
		}
		outputFile := filepath.Join(path, "images.tar")
		log.Printf("Save merged images [1/1]: images=%d output=%s", total, outputFile)
		if err := imageManager.Save(imageIDList, outputFile); err != nil {
			log.Printf("Save summary: total=%d success=0 failed=1 skipped=%d", total, skipped)
			return err
		}
		log.Printf("Save summary: total=%d success=%d failed=0 skipped=%d", total, total, skipped)
		return nil
	} else {
		var saveErrs []error
		success := 0
		index := 0
		for imageID, imageName := range imageMap {
			index++
			outputFile := filepath.Join(path, imageName+".tar")
			log.Printf("Save image [%d/%d]: %s -> %s", index, total, imageID, outputFile)
			if err := imageManager.Save([]string{imageID}, outputFile); err != nil {
				wrappedErr := fmt.Errorf("export image %s to %s: %w", imageID, outputFile, err)
				log.Println(wrappedErr)
				saveErrs = append(saveErrs, wrappedErr)
				continue
			}
			success++
		}
		log.Printf("Save summary: total=%d success=%d failed=%d skipped=%d", total, success, len(saveErrs), skipped)
		return errors.Join(saveErrs...)
	}
}
