package pull

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Yui100901/MyGo/struct_utils"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func prepareWorkspace(info *ImageInfo) (string, error) {
	pattern := fmt.Sprintf("%s_%s", info.Image, info.Tag)
	return os.MkdirTemp(".", pattern)
}

func createManifestFile(info *ImageInfo, manifest *ocispec.Manifest, tempDir string) error {
	manifestContent := []*ImageManifest{
		{
			Config:   strings.TrimPrefix(string(manifest.Config.Digest), "sha256:") + ".json",
			Layers:   getLayerPaths(manifest.Layers),
			RepoTags: []string{fmt.Sprintf("%s:%s", imagePath(info), info.Tag)},
		},
	}

	data, err := struct_utils.MarshalData(manifestContent, struct_utils.JSON)
	if err != nil {
		return fmt.Errorf("序列化清单失败: %w", err)
	}

	return os.WriteFile(filepath.Join(tempDir, "manifest.json"), data, 0644)
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
	if err := os.MkdirAll(filepath.Dir(outputFile), 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}
	return createTarArchiveWithContext(ctx, tempDir, outputFile)
}

func createTarArchiveWithContext(ctx context.Context, sourceDir, outputFile string) error {
	partPath := partialDownloadPath(outputFile)
	_ = os.Remove(partPath)

	file, err := os.Create(partPath)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(partPath)
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
		defer func() {
			if cerr := src.Close(); cerr != nil {
				log.Printf("警告: 关闭文件 %s 失败: %v", path, cerr)
			}
		}()
		return copyWithContext(ctx, tw, src)
	})
	closeTarErr := tw.Close()
	closeFileErr := file.Close()
	if walkErr != nil {
		return walkErr
	}
	if closeTarErr != nil {
		return closeTarErr
	}
	if closeFileErr != nil {
		return closeFileErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = os.Remove(outputFile)
	if err := os.Rename(partPath, outputFile); err != nil {
		return err
	}
	cleanup = false
	return nil
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
	if expected == "" {
		return nil
	}
	if err := expected.Validate(); err != nil {
		return err
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			log.Printf("警告: 关闭文件 %s 失败: %v", path, cerr)
		}
	}()

	verifier := expected.Verifier()
	if _, err := io.Copy(verifier, file); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("digest 校验失败 %s: 期望 %s", path, expected)
	}
	return nil
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
