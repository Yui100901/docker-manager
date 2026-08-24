package pull

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Yui100901/MyGo/struct_utils"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	pullArchiveStagingPrefix = ".docker-manager-pull-"
	windowsReparsePointAttr  = 0x400
)

func prepareWorkspace(info *ImageInfo) (string, error) {
	pattern := fmt.Sprintf("%s_%s", info.Image, info.Tag)
	return os.MkdirTemp("", pattern)
}

func createManifestFile(info *ImageInfo, manifest *ocispec.Manifest, tempDir string) error {
	configFileName, err := configBlobFileName(manifest.Config)
	if err != nil {
		return err
	}
	manifestContent := []*ImageManifest{
		{
			Config:   configFileName,
			Layers:   getLayerPaths(manifest.Layers),
			RepoTags: []string{fmt.Sprintf("%s:%s", imagePath(info), info.Tag)},
		},
	}

	data, err := struct_utils.MarshalData(manifestContent, struct_utils.JSON)
	if err != nil {
		return fmt.Errorf("序列化清单失败: %w", err)
	}

	return writeFileWithinRoot(tempDir, "manifest.json", data, 0644)
}

func getLayerPaths(layers []ocispec.Descriptor) []string {
	paths := make([]string, 0, len(layers))
	for _, layer := range layers {
		paths = append(paths, fmt.Sprintf("%s/layer.tar", sha256Hash(string(layer.Digest))))
	}
	return paths
}

func packageImage(ctx context.Context, tempDir, outputFile string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return createTarArchiveWithContext(ctx, tempDir, outputFile)
}

func createTarArchiveWithContext(ctx context.Context, sourceDir, outputFile string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	outputRoot, outputName, outputPath, parentInfo, err := openPullArchiveOutput(outputFile)
	if err != nil {
		return err
	}
	defer outputRoot.Close()

	stagingName, file, stagingOwner, err := createPullArchiveStaging(outputRoot)
	if err != nil {
		return fmt.Errorf("创建归档临时文件失败: %w", err)
	}
	fileOpen := true
	cleanupStaging := true
	defer func() {
		if fileOpen {
			_ = file.Close()
		}
		if cleanupStaging {
			_ = removeOwnedPullArchiveStaging(outputRoot, stagingName, stagingOwner)
		}
	}()

	tw := tar.NewWriter(file)
	walkErr := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == sourceDir {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if entry.IsDir() && !strings.HasSuffix(header.Name, "/") {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		copyErr := copyWithContext(ctx, tw, src)
		closeErr := src.Close()
		return errors.Join(copyErr, closeErr)
	})
	closeTarErr := tw.Close()
	syncFileErr := file.Sync()
	stagedInfo, statFileErr := file.Stat()
	closeFileErr := file.Close()
	fileOpen = false
	if walkErr != nil {
		return fmt.Errorf("创建归档失败: %w", walkErr)
	}
	if closeTarErr != nil {
		return fmt.Errorf("完成归档失败: %w", closeTarErr)
	}
	if syncFileErr != nil {
		return fmt.Errorf("同步归档临时文件失败: %w", syncFileErr)
	}
	if statFileErr != nil {
		return fmt.Errorf("检查归档临时文件失败: %w", statFileErr)
	}
	if closeFileErr != nil {
		return fmt.Errorf("关闭归档临时文件失败: %w", closeFileErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyPullArchivePublication(outputRoot, stagingName, outputName, outputPath, parentInfo, stagedInfo); err != nil {
		return err
	}
	if err := outputRoot.Rename(stagingName, outputName); err != nil {
		return fmt.Errorf("发布归档 %s 失败: %w", outputPath, err)
	}
	cleanupStaging = false
	return nil
}

func openPullArchiveOutput(outputFile string) (*os.Root, string, string, os.FileInfo, error) {
	outputPath, err := filepath.Abs(outputFile)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("解析归档输出路径失败: %w", err)
	}
	outputPath = filepath.Clean(outputPath)
	outputName := filepath.Base(outputPath)
	if outputName == "." || outputName == string(os.PathSeparator) || !filepath.IsLocal(outputName) {
		return nil, "", "", nil, fmt.Errorf("归档输出文件名无效: %s", outputFile)
	}
	if err := rejectPullArchiveOutputLinks(outputPath); err != nil {
		return nil, "", "", nil, err
	}

	parentPath := filepath.Dir(outputPath)
	volume := filepath.VolumeName(parentPath)
	rootPath := string(os.PathSeparator)
	if volume != "" {
		rootPath = volume + string(os.PathSeparator)
	}
	relativeParent, err := filepath.Rel(rootPath, parentPath)
	if err != nil || relativeParent == ".." || strings.HasPrefix(relativeParent, ".."+string(os.PathSeparator)) {
		return nil, "", "", nil, fmt.Errorf("解析归档输出目录失败: %s", parentPath)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("打开归档输出路径根目录失败: %w", err)
	}
	components := strings.Split(filepath.ToSlash(relativeParent), "/")
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		info, statErr := root.Lstat(component)
		if os.IsNotExist(statErr) {
			if mkdirErr := root.Mkdir(component, 0755); mkdirErr != nil && !os.IsExist(mkdirErr) {
				_ = root.Close()
				return nil, "", "", nil, fmt.Errorf("创建归档输出目录 %s 失败: %w", component, mkdirErr)
			}
			info, statErr = root.Lstat(component)
		}
		if statErr != nil {
			_ = root.Close()
			return nil, "", "", nil, fmt.Errorf("检查归档输出目录 %s 失败: %w", component, statErr)
		}
		if pullArchiveInfoIsReparsePoint(info) || !info.IsDir() {
			_ = root.Close()
			return nil, "", "", nil, fmt.Errorf("拒绝通过符号链接或重解析点写入归档输出: %s", filepath.Join(root.Name(), component))
		}

		nextRoot, openErr := root.OpenRoot(component)
		if openErr != nil {
			_ = root.Close()
			return nil, "", "", nil, fmt.Errorf("打开归档输出目录 %s 失败: %w", component, openErr)
		}
		openedInfo, openedErr := nextRoot.Stat(".")
		currentInfo, currentErr := root.Lstat(component)
		if openedErr != nil || currentErr != nil || pullArchiveInfoIsReparsePoint(currentInfo) ||
			!currentInfo.IsDir() || !os.SameFile(openedInfo, currentInfo) {
			_ = nextRoot.Close()
			_ = root.Close()
			return nil, "", "", nil, fmt.Errorf("归档输出目录在打开时发生变化: %s", filepath.Join(root.Name(), component))
		}
		_ = root.Close()
		root = nextRoot
	}

	parentInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, "", "", nil, fmt.Errorf("检查归档输出目录失败: %w", err)
	}
	if err := verifyPullArchiveOutputPath(root, outputName, outputPath, parentInfo); err != nil {
		_ = root.Close()
		return nil, "", "", nil, err
	}
	return root, outputName, outputPath, parentInfo, nil
}

func createPullArchiveStaging(root *os.Root) (string, *os.File, os.FileInfo, error) {
	for range 128 {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", nil, nil, err
		}
		name := pullArchiveStagingPrefix + hex.EncodeToString(random) + ".tmp"
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() {
				_ = file.Close()
				_ = root.Remove(name)
				if statErr != nil {
					return "", nil, nil, statErr
				}
				return "", nil, nil, fmt.Errorf("归档临时文件不是普通文件: %s", name)
			}
			return name, file, info, nil
		}
		if !os.IsExist(err) {
			return "", nil, nil, err
		}
	}
	return "", nil, nil, fmt.Errorf("无法分配唯一的归档临时文件名")
}

func removeOwnedPullArchiveStaging(root *os.Root, stagingName string, ownerInfo os.FileInfo) error {
	currentInfo, err := root.Lstat(stagingName)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查归档临时文件失败: %w", err)
	}
	if ownerInfo == nil || pullArchiveInfoIsReparsePoint(currentInfo) || !currentInfo.Mode().IsRegular() ||
		!ownerInfo.Mode().IsRegular() || !os.SameFile(currentInfo, ownerInfo) {
		return fmt.Errorf("拒绝删除已被替换的归档临时文件: %s", stagingName)
	}
	if err := root.Remove(stagingName); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除归档临时文件失败: %w", err)
	}
	return nil
}

func verifyPullArchivePublication(root *os.Root, stagingName, outputName, outputPath string, parentInfo, stagedInfo os.FileInfo) error {
	if err := verifyPullArchiveOutputPath(root, outputName, outputPath, parentInfo); err != nil {
		return err
	}
	currentStagedInfo, err := root.Lstat(stagingName)
	if err != nil {
		return fmt.Errorf("检查归档临时文件失败: %w", err)
	}
	if pullArchiveInfoIsReparsePoint(currentStagedInfo) || !currentStagedInfo.Mode().IsRegular() ||
		!stagedInfo.Mode().IsRegular() || !os.SameFile(currentStagedInfo, stagedInfo) {
		return fmt.Errorf("归档临时文件在发布前发生变化: %s", stagingName)
	}
	return nil
}

func verifyPullArchiveOutputPath(root *os.Root, outputName, outputPath string, parentInfo os.FileInfo) error {
	if err := rejectPullArchiveOutputLinks(outputPath); err != nil {
		return err
	}
	currentParentInfo, err := os.Lstat(filepath.Dir(outputPath))
	if err != nil {
		return fmt.Errorf("检查归档输出目录失败: %w", err)
	}
	openedParentInfo, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("检查已打开的归档输出目录失败: %w", err)
	}
	if pullArchiveInfoIsReparsePoint(currentParentInfo) || !currentParentInfo.IsDir() ||
		!os.SameFile(parentInfo, openedParentInfo) || !os.SameFile(parentInfo, currentParentInfo) {
		return fmt.Errorf("归档输出目录在写入期间发生变化: %s", filepath.Dir(outputPath))
	}
	info, err := root.Lstat(outputName)
	switch {
	case err == nil:
		if pullArchiveInfoIsReparsePoint(info) || !info.Mode().IsRegular() {
			return fmt.Errorf("拒绝替换非普通归档输出: %s", outputPath)
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("检查归档输出失败: %w", err)
	}
	return nil
}

func rejectPullArchiveOutputLinks(outputPath string) error {
	for current, output := filepath.Clean(outputPath), true; ; current, output = filepath.Dir(current), false {
		info, err := os.Lstat(current)
		if err == nil {
			if pullArchiveInfoIsReparsePoint(info) {
				return fmt.Errorf("拒绝通过符号链接或重解析点写入归档输出: %s", current)
			}
			if output {
				if !info.Mode().IsRegular() {
					return fmt.Errorf("拒绝替换非普通归档输出: %s", current)
				}
			} else if !info.IsDir() {
				return fmt.Errorf("归档输出路径祖先不是目录: %s", current)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("检查归档输出路径 %s 失败: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func pullArchiveInfoIsReparsePoint(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return info != nil
	}
	// os.FileInfo deliberately hides a few data-preserving Windows reparse
	// tags from FileMode. Inspect the standard-library Sys value so all tags
	// are rejected without adding platform-specific files or syscalls.
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return false
	}
	attributes := value.FieldByName("FileAttributes")
	return attributes.IsValid() && attributes.CanUint() && attributes.Uint()&windowsReparsePointAttr != 0
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func sha256Hash(input string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(input)))
}

func verifyFileDigest(path string, expected digest.Digest) error {
	return verifyFileDigestWithContext(context.Background(), path, expected)
}

func resolveOutputFile(info *ImageInfo, opts PullOptions) (string, error) {
	if opts.Output != "" {
		return opts.Output, nil
	}

	outputDir := opts.OutputDir
	if outputDir == "" {
		outputDir = "."
	}
	return filepath.Join(outputDir, defaultOutputFileName(info)), nil
}

func defaultOutputFileName(info *ImageInfo) string {
	name := strings.ReplaceAll(imagePath(info), "/", "_")
	tag := sanitizeOutputName(info.Tag)
	if tag == "" {
		tag = "latest"
	}
	return fmt.Sprintf("%s_%s.tar", name, tag)
}

func sanitizeOutputName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' {
			sb.WriteRune(r)
			continue
		}
		switch r {
		case '.', '-', '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	return sb.String()
}
