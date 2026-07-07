package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"docker-manager/internal/resourcefilter"
	"encoding/json"
	"fmt"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func createBackupArchive(sourceDir, archivePath string) error {
	return createBackupArchiveWithContext(context.Background(), sourceDir, archivePath)
}

func createBackupArchiveWithContext(ctx context.Context, sourceDir, archivePath string) error {
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(archivePath), 0755); err != nil {
		return err
	}
	archiveAbs, _ := filepath.Abs(archivePath)
	file, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gz := gzip.NewWriter(file)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		pathAbs, _ := filepath.Abs(path)
		if archiveAbs != "" && pathAbs == archiveAbs {
			return nil
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
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
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		return backupCopyWithContext(ctx, tw, in)
	})
}

func resolveRestoreBackupDir(path string) (string, func(), error) {
	return resolveRestoreBackupDirWithContext(context.Background(), path)
}

func resolveRestoreBackupDirWithContext(ctx context.Context, path string) (string, func(), error) {
	if err := checkBackupContext(ctx); err != nil {
		return "", nil, err
	}
	if !isBackupArchive(path) {
		return path, nil, nil
	}
	tempDir, err := os.MkdirTemp("", "dm-restore-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	if err := extractBackupArchiveWithContext(ctx, path, tempDir); err != nil {
		cleanup()
		return "", nil, err
	}
	root, err := findExtractedBackupRoot(tempDir)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return root, cleanup, nil
}

func isBackupArchive(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")
}

func extractBackupArchive(archivePath, destDir string) error {
	return extractBackupArchiveWithContext(context.Background(), archivePath, destDir)
}

func extractBackupArchiveWithContext(ctx context.Context, archivePath, destDir string) error {
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeExtractPath(destDir, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if err := backupCopyWithContext(ctx, out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}

func safeExtractPath(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	return filepath.Join(root, clean), nil
}

func findExtractedBackupRoot(tempDir string) (string, error) {
	if isBackupRootDir(tempDir) {
		return tempDir, nil
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(tempDir, entry.Name())
		if isBackupRootDir(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("archive does not contain %s", backupManifestName)
}

func isBackupRootDir(dir string) bool {
	return isSingleBackupDir(dir)
}

func isSingleBackupDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, backupManifestName))
	return err == nil
}

func readBackupManifest(backupDir string) (BackupManifest, error) {
	var manifest BackupManifest
	data, err := os.ReadFile(filepath.Join(backupDir, backupManifestName))
	if err != nil {
		return manifest, fmt.Errorf("read manifest: %w", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Version != 1 {
		return manifest, fmt.Errorf("unsupported backup manifest version %d", manifest.Version)
	}
	manifest = normalizeBackupManifest(manifest)
	return manifest, nil
}

func normalizeBackupManifest(manifest BackupManifest) BackupManifest {
	if len(manifest.Containers) == 0 && (manifest.ContainerName != "" || manifest.InspectFile != "" || manifest.ComposeFile != "") {
		manifest.Containers = []BackupContainerManifest{{
			ContainerName: manifest.ContainerName,
			SourceName:    manifest.SourceName,
			Image:         manifest.Image,
			ImageArchive:  manifest.ImageArchive,
			InspectFile:   manifest.InspectFile,
			ComposeFile:   manifest.ComposeFile,
			Networks:      manifest.Networks,
			Volumes:       manifest.Volumes,
		}}
	}
	return manifest
}

func readContainerInspect(backupDir string, manifest BackupContainerManifest) (container.InspectResponse, error) {
	inspectFile := manifest.InspectFile
	if inspectFile == "" {
		inspectFile = backupInspectName
	}
	var inspect container.InspectResponse
	path, err := backupFilePath(backupDir, inspectFile)
	if err != nil {
		return inspect, err
	}
	if err := readJSON(path, &inspect); err != nil {
		return inspect, fmt.Errorf("read container inspect: %w", err)
	}
	return inspect, nil
}

func readNetworkInspect(backupDir string, ref BackupResourceRef) (network.Inspect, error) {
	var value network.Inspect
	path, err := backupFilePath(backupDir, ref.File)
	if err != nil {
		return value, err
	}
	if err := readJSON(path, &value); err != nil {
		return value, fmt.Errorf("read network %s: %w", ref.Name, err)
	}
	return value, nil
}

func readVolumeInspect(backupDir string, ref BackupResourceRef) (volume.Volume, error) {
	var value volume.Volume
	path, err := backupFilePath(backupDir, ref.File)
	if err != nil {
		return value, err
	}
	if err := readJSON(path, &value); err != nil {
		return value, fmt.Errorf("read volume %s: %w", ref.Name, err)
	}
	return value, nil
}

func backupFilePath(backupDir, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".." {
		return "", fmt.Errorf("invalid backup file path %q", rel)
	}
	return filepath.Join(backupDir, clean), nil
}

func readJSON(path string, value interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func normalizeContainerName(name string) string {
	return resourcefilter.NormalizeContainerName(name)
}

func defaultBackupDir(now time.Time, containerName string) string {
	return filepath.Join(backupRoot, safeBackupName(containerName)+"-"+now.Format("20060102-150405"))
}

func defaultBackupBatchDir(now time.Time) string {
	return filepath.Join(backupRoot, "batch-"+now.Format("20060102-150405"))
}

func safeBackupName(name string) string {
	name = normalizeContainerName(name)
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
	if sb.Len() == 0 {
		return "resource"
	}
	return sb.String()
}

func isBuiltinNetwork(name string) bool {
	switch name {
	case "", "bridge", "host", "none":
		return true
	default:
		return false
	}
}
