package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
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

type SaveOptions struct {
	Merge   bool
	All     bool
	DryRun  bool
	Filters []string
}

type imageExportTarget struct {
	ID   string
	Name string
}

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
	var dryRun bool
	var filters []string
	cmd := &cobra.Command{
		Use:   "save [path] [options]",
		Short: "导出Docker镜像，默认为当前路径下的images。",
		Run: func(cmd *cobra.Command, args []string) {
			path := "images"
			if len(args) > 0 {
				path = args[0]
			}
			if !dryRun {
				if _, err := file_utils.CreateDirectory(path); err != nil {
					log.Fatalf("Create directory failed: %v", err)
				}
			}
			opts := SaveOptions{
				Merge:   merge,
				All:     all,
				DryRun:  dryRun,
				Filters: filters,
			}
			if err := saveImagesWithOptions(path, opts); err != nil {
				log.Fatalf("Export failed: %v", err)
			}
		},
	}
	cmd.Flags().BoolVarP(&merge, "merge", "m", false, "合并成一个文件images.tar")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "导出所有镜像，包括无tag镜像")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "仅预览将导出的镜像，不写入文件")
	cmd.Flags().StringArrayVarP(&filters, "filter", "f", nil, "筛选要导出的镜像，支持镜像名/tag/ID和通配符，可重复指定")
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
	return saveImagesWithOptions(path, SaveOptions{Merge: merge, All: all})
}

func saveImagesWithOptions(path string, opts SaveOptions) error {
	images, err := imageManager.List(opts.All)
	if err != nil {
		log.Println(err)
		return err
	}
	targets, skipped := buildImageExportTargets(images, opts)
	for _, target := range targets {
		log.Println("Export image", target.ID, target.Name)
	}
	total := len(targets)
	log.Printf("Save images: total=%d skipped=%d merge=%v dryRun=%v output=%s filters=%s", total, skipped, opts.Merge, opts.DryRun, path, strings.Join(opts.Filters, ","))

	if opts.DryRun {
		for i, target := range targets {
			outputFile := filepath.Join(path, target.Name+".tar")
			if opts.Merge {
				outputFile = filepath.Join(path, "images.tar")
			}
			log.Printf("Dry run save image [%d/%d]: %s -> %s", i+1, total, target.ID, outputFile)
		}
		log.Printf("Save summary: total=%d success=0 failed=0 skipped=%d dryRun=true", total, skipped)
		return nil
	}

	if opts.Merge {
		imageIDList := make([]string, 0, len(targets))
		for _, target := range targets {
			imageIDList = append(imageIDList, target.ID)
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
		for i, target := range targets {
			outputFile := filepath.Join(path, target.Name+".tar")
			log.Printf("Save image [%d/%d]: %s -> %s", i+1, total, target.ID, outputFile)
			if err := imageManager.Save([]string{target.ID}, outputFile); err != nil {
				wrappedErr := fmt.Errorf("export image %s to %s: %w", target.ID, outputFile, err)
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

func buildImageExportTargets(images []image.Summary, opts SaveOptions) ([]imageExportTarget, int) {
	var targets []imageExportTarget
	skipped := 0
	for _, image := range images {
		imageID := image.ID
		if !matchesImageFilters(image, opts.Filters) {
			skipped++
			continue
		}
		if len(image.RepoTags) > 0 {
			imageName := image.RepoTags[0]
			imageName = strings.ReplaceAll(imageName, "/", "_")
			imageName = strings.ReplaceAll(imageName, ":", "-")
			targets = append(targets, imageExportTarget{ID: imageID, Name: imageName})
		} else {
			if opts.All {
				targets = append(targets, imageExportTarget{
					ID:   imageID,
					Name: strings.ReplaceAll(imageID, ":", "_"),
				})
			} else {
				skipped++
			}
		}
	}
	return targets, skipped
}

func matchesImageFilters(img image.Summary, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	candidates := imageFilterCandidates(img)
	for _, filter := range filters {
		for _, candidate := range candidates {
			if wildcardMatch(filter, candidate) || candidate == filter || strings.HasPrefix(candidate, filter) {
				return true
			}
		}
	}
	return false
}

func imageFilterCandidates(img image.Summary) []string {
	candidates := []string{img.ID, strings.TrimPrefix(img.ID, "sha256:")}
	if shortID := strings.TrimPrefix(img.ID, "sha256:"); len(shortID) > 12 {
		candidates = append(candidates, shortID[:12])
	}
	for _, tag := range img.RepoTags {
		candidates = append(candidates, tag)
		repo, version := splitRepoTag(tag)
		if repo != "" {
			candidates = append(candidates, repo)
			if slash := strings.LastIndex(repo, "/"); slash >= 0 && slash < len(repo)-1 {
				candidates = append(candidates, repo[slash+1:])
			}
		}
		if version != "" {
			candidates = append(candidates, version)
		}
	}
	return candidates
}

func splitRepoTag(ref string) (string, string) {
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, ""
}

func wildcardMatch(pattern, value string) bool {
	re, err := regexp.Compile("^" + wildcardToRegex(pattern) + "$")
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

func wildcardToRegex(pattern string) string {
	var sb strings.Builder
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteByte('.')
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return sb.String()
}
