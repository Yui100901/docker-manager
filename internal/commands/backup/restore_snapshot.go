package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func snapshotRestoreBackupDir(ctx context.Context, source string) (string, func(), error) {
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
		if entry.IsDir() {
			return os.Mkdir(target, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("restore source contains a non-regular file: %s", path)
		}
		return copyRestoreSnapshotFile(ctx, path, target)
	})
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return destination, cleanup, nil
}

func copyRestoreSnapshotFile(ctx context.Context, source, destination string) error {
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
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := backupCopyWithContext(ctx, output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}
