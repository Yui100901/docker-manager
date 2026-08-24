package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func snapshotRestoreBackupDir(ctx context.Context, source string, limits backupLimits) (string, func(), error) {
	rootInfo, err := os.Lstat(source)
	if err != nil {
		return "", nil, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", nil, fmt.Errorf("restore source must be a real directory, not a symlink or non-directory: %s", source)
	}
	tempRoot, err := os.MkdirTemp("", "dm-restore-snapshot-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tempRoot) }
	destination := filepath.Join(tempRoot, "backup")
	if err := os.Mkdir(destination, 0700); err != nil {
		cleanup()
		return "", nil, err
	}
	diskBudget := newBackupByteBudget("restore temporary files", limits.temporaryBytes())
	tarBudget := backupTarBudget{maxExpanded: limits.expandedBytes}
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("restore source contains a symbolic link: %s", path)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := tarBudget.Add(filepath.ToSlash(relative), info.Size(), info.Mode().IsRegular()); err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Mkdir(target, 0700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("restore source contains a non-regular file: %s", path)
		}
		if err := diskBudget.Add(info.Size()); err != nil {
			return err
		}
		return copyRestoreSnapshotFile(ctx, path, target, info.Size())
	})
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return destination, cleanup, nil
}

func copyRestoreSnapshotFile(ctx context.Context, source, destination string, expectedSize int64) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("restore source changed to a non-regular file: %s", source)
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("restore source changed size before snapshot: %s", source)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := backupCopyNWithContext(ctx, output, input, expectedSize); err != nil {
		_ = output.Close()
		return err
	}
	var trailing [1]byte
	if n, readErr := input.Read(trailing[:]); n != 0 || (readErr != nil && readErr != io.EOF) {
		_ = output.Close()
		return fmt.Errorf("restore source changed while it was snapshotted: %s", source)
	}
	return output.Close()
}
