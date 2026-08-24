package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"docker-manager/internal/resourcefilter"
	"errors"
	"fmt"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"
)

func createBackupArchive(sourceDir, archivePath string) error {
	return createBackupArchiveWithContext(context.Background(), sourceDir, archivePath)
}

func createBackupArchiveWithContext(ctx context.Context, sourceDir, archivePath string) error {
	return createBackupArchiveWithOptions(ctx, sourceDir, archivePath, backupArchiveOptions{})
}

func createBackupArchiveWithOptions(ctx context.Context, sourceDir, archivePath string, opts backupArchiveOptions) error {
	ctx, cancel := context.WithTimeout(backupContext(ctx), maxBackupArchiveOperationTime)
	defer cancel()
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	sourceInfo, err := os.Lstat(sourceDir)
	if err != nil {
		return err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.IsDir() {
		return fmt.Errorf("backup archive source must be a real directory: %s", sourceDir)
	}
	if err := requireBackupPathOutsideRoot(sourceDir, archivePath, "bundle output"); err != nil {
		return err
	}
	archiveAbs, _ := filepath.Abs(archivePath)
	file, err := openBackupArchiveWriter(ctx, archivePath, opts)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	var tarBudget backupTarBudget

	walkErr := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
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
		archiveName := filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("backup archive source contains a symbolic link: %s", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !entry.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("backup archive source contains a non-regular file: %s", path)
		}
		if err := tarBudget.Add(archiveName, info.Size(), info.Mode().IsRegular()); err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = archiveName
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
		copyErr := backupCopyNWithContext(ctx, tw, in, info.Size())
		closeErr := in.Close()
		return errors.Join(copyErr, closeErr)
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return errors.Join(walkErr, file.Abort())
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return errors.Join(err, file.Abort())
	}
	if err := gz.Close(); err != nil {
		return errors.Join(err, file.Abort())
	}
	if err := file.Close(); err != nil {
		return errors.Join(err, file.Abort())
	}
	return nil
}

func resolveRestoreBackupDirWithOptions(ctx context.Context, path string, opts RestoreOptions) (string, func(), error) {
	ctx, cancelOperation := context.WithTimeout(backupContext(ctx), maxBackupArchiveOperationTime)
	defer cancelOperation()
	limits, err := resolveRestoreLimits(opts)
	if err != nil {
		return "", nil, err
	}
	if err := checkBackupContext(ctx); err != nil {
		return "", nil, err
	}
	if !isBackupArchive(path) && !isBackupArchivePart(path) && !isEncryptedBackupArchive(path) {
		if opts.TrustedPublicKey != "" {
			if err := requireBackupKeyOutsideRoot(path, opts.TrustedPublicKey, "trusted public key"); err != nil {
				return "", nil, err
			}
		}
		if opts.Confirm && !opts.DryRun {
			return snapshotRestoreBackupDir(ctx, path, limits)
		}
		return path, nil, nil
	}
	tempDir, err := os.MkdirTemp("", "dm-restore-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	workDir := filepath.Join(tempDir, "work")
	extractDir := filepath.Join(tempDir, "extract")
	if err := os.Mkdir(workDir, 0700); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.Mkdir(extractDir, 0700); err != nil {
		cleanup()
		return "", nil, err
	}
	archivePath := path
	diskBudget := newBackupByteBudget("restore temporary files", limits.temporaryBytes())
	if isBackupArchivePart(path) {
		joined := filepath.Join(workDir, strings.TrimSuffix(filepath.Base(path), ".part-001"))
		if err := joinBackupArchivePartsWithLimits(ctx, path, joined, diskBudget, limits); err != nil {
			cleanup()
			return "", nil, err
		}
		archivePath = joined
	}
	if isEncryptedBackupArchive(archivePath) {
		decrypted := filepath.Join(workDir, strings.TrimSuffix(filepath.Base(archivePath), ".enc"))
		if err := decryptBackupArchiveWithLimits(ctx, archivePath, decrypted, opts.PassphraseFile, diskBudget, limits); err != nil {
			cleanup()
			return "", nil, err
		}
		archivePath = decrypted
	}
	if err := extractBackupArchiveWithLimits(ctx, archivePath, extractDir, diskBudget, limits); err != nil {
		cleanup()
		return "", nil, err
	}
	root, err := findExtractedBackupRoot(extractDir)
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

func isEncryptedBackupArchive(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".enc")
}

func isBackupArchivePart(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".part-001")
}

func extractBackupArchive(archivePath, destDir string) error {
	return extractBackupArchiveWithContext(context.Background(), archivePath, destDir)
}

func extractBackupArchiveWithContext(ctx context.Context, archivePath, destDir string) error {
	return extractBackupArchiveWithBudget(ctx, archivePath, destDir, newBackupByteBudget("restore temporary files", maxBackupTemporaryBytes))
}

func extractBackupArchiveWithBudget(ctx context.Context, archivePath, destDir string, diskBudget *backupByteBudget) error {
	return extractBackupArchiveWithLimits(ctx, archivePath, destDir, diskBudget, defaultBackupLimits())
}

func extractBackupArchiveWithLimits(ctx context.Context, archivePath, destDir string, diskBudget *backupByteBudget, limits backupLimits) error {
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	archiveInfo, err := os.Lstat(archivePath)
	if err != nil {
		return err
	}
	if !archiveInfo.Mode().IsRegular() || archiveInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("backup archive must be a regular file")
	}
	if archiveInfo.Size() > limits.archiveBytes {
		return fmt.Errorf("backup archive exceeds the %d-byte limit", limits.archiveBytes)
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
	tarBudget := backupTarBudget{maxExpanded: limits.expandedBytes}
	seen := make(map[string]struct{})
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
		if err := rejectBackupPathSymlinks(target); err != nil {
			return err
		}
		if _, exists := seen[header.Name]; exists {
			return fmt.Errorf("backup archive contains duplicate path %q", header.Name)
		}
		seen[header.Name] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := tarBudget.Add(header.Name, header.Size, false); err != nil {
				return err
			}
			if header.Size != 0 {
				return fmt.Errorf("backup archive directory %q has a non-zero size", header.Name)
			}
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := tarBudget.Add(header.Name, header.Size, true); err != nil {
				return err
			}
			if err := diskBudget.Add(header.Size); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			if err := backupCopyNWithContext(ctx, out, tr, header.Size); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			if err := tarBudget.Add(header.Name, header.Size, false); err != nil {
				return err
			}
			return fmt.Errorf("backup archive contains unsupported entry type %d at %q", header.Typeflag, header.Name)
		}
	}
}

func safeExtractPath(root, name string) (string, error) {
	clean, err := canonicalBackupRelativePath(name)
	if err != nil {
		return "", fmt.Errorf("invalid archive path %q: %w", name, err)
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
	return readBackupManifestWithLimit(backupDir, maxBackupManifestJSONBytes)
}

func readBackupManifestWithLimit(backupDir string, limit int64) (BackupManifest, error) {
	var manifest BackupManifest
	if limit <= 0 || limit > maxBackupManifestJSONBytes {
		limit = maxBackupManifestJSONBytes
	}
	if err := readLimitedBackupJSON(filepath.Join(backupDir, backupManifestName), limit, &manifest); err != nil {
		return manifest, fmt.Errorf("read manifest: %w", err)
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
	if err := readLimitedBackupJSON(path, maxBackupInspectJSONBytes, &inspect); err != nil {
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
	if err := readLimitedBackupJSON(path, maxBackupResourceJSONBytes, &value); err != nil {
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
	if err := readLimitedBackupJSON(path, maxBackupResourceJSONBytes, &value); err != nil {
		return value, fmt.Errorf("read volume %s: %w", ref.Name, err)
	}
	return value, nil
}

func backupFilePath(backupDir, rel string) (string, error) {
	clean, err := canonicalBackupRelativePath(rel)
	if err != nil {
		return "", fmt.Errorf("invalid backup file path %q: %w", rel, err)
	}
	return filepath.Join(backupDir, clean), nil
}

func canonicalBackupRelativePath(name string) (string, error) {
	if name == "" || strings.Contains(name, `\`) || strings.Contains(name, ":") {
		return "", fmt.Errorf("path must use portable forward-slash components")
	}
	if !fs.ValidPath(name) || pathpkg.Clean(name) != name {
		return "", fmt.Errorf("path is not canonical")
	}
	local := filepath.FromSlash(name)
	if filepath.VolumeName(local) != "" || !filepath.IsLocal(local) {
		return "", fmt.Errorf("path is not local")
	}
	return local, nil
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
