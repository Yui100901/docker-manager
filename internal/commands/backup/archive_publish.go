package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

func createBackupArtifactStaging(finalPath string) (string, error) {
	if err := rejectBackupPathSymlinks(finalPath); err != nil {
		return "", err
	}
	parent := filepath.Dir(finalPath)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", fmt.Errorf("create backup artifact parent: %w", err)
	}
	if err := rejectBackupPathSymlinks(finalPath); err != nil {
		return "", err
	}
	if _, err := os.Lstat(finalPath); err == nil {
		return "", fmt.Errorf("backup artifact already exists: %s", finalPath)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect backup artifact: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".dm-backup-staging-*")
	if err != nil {
		return "", fmt.Errorf("create backup artifact staging directory: %w", err)
	}
	if err := os.Chmod(staging, 0700); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("secure backup artifact staging directory: %w", err)
	}
	return staging, nil
}

func rejectBackupPathSymlinks(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve backup artifact path: %w", err)
	}
	for current := filepath.Clean(absolute); ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil && backupInfoIsReparsePoint(info) {
			return fmt.Errorf("backup artifact path contains a symbolic link: %s", current)
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect backup artifact path %s: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

// Hard-link publication gives create-if-absent semantics on both Unix and
// Windows and cannot replace a path that was raced into place after preflight.
func publishBackupStagedFile(stagedPath, finalPath string) error {
	if err := rejectBackupPathSymlinks(finalPath); err != nil {
		return err
	}
	if err := os.Link(stagedPath, finalPath); err != nil {
		return fmt.Errorf("publish backup artifact %s: %w", finalPath, err)
	}
	if err := syncBackupDirectory(filepath.Dir(finalPath)); err != nil {
		removeErr := removePublishedBackupStagedFile(stagedPath, finalPath)
		resyncErr := syncBackupDirectory(filepath.Dir(finalPath))
		return errors.Join(
			fmt.Errorf("sync published backup artifact directory: %w", err),
			removeErr,
			resyncErr,
		)
	}
	return nil
}

func removePublishedBackupStagedFile(stagedPath, finalPath string) error {
	finalInfo, err := os.Lstat(finalPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect published backup artifact %s: %w", finalPath, err)
	}
	stagedInfo, err := os.Lstat(stagedPath)
	if err != nil {
		return fmt.Errorf("inspect staged backup artifact %s: %w", stagedPath, err)
	}
	if !finalInfo.Mode().IsRegular() || finalInfo.Mode()&os.ModeSymlink != 0 ||
		!stagedInfo.Mode().IsRegular() || stagedInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(finalInfo, stagedInfo) {
		return fmt.Errorf("refusing to remove replaced backup artifact: %s", finalPath)
	}
	if err := os.Remove(finalPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove published backup artifact %s: %w", finalPath, err)
	}
	return nil
}

func syncBackupDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func verifyBackupStagedFile(ctx context.Context, path string, expectedSize int64, expectedDigest string) error {
	ctx = backupContext(ctx)
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("staged backup artifact is not a regular file: %s", path)
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("staged backup artifact size changed: expected %d, found %d", expectedSize, info.Size())
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() != expectedSize {
		_ = file.Close()
		return fmt.Errorf("staged backup artifact changed while opening: %s", path)
	}
	hash := sha256.New()
	copyErr := backupCopyNWithContext(ctx, hash, file, expectedSize)
	if copyErr == nil {
		var trailing [1]byte
		if n, readErr := file.Read(trailing[:]); n != 0 || (readErr != nil && readErr != io.EOF) {
			copyErr = fmt.Errorf("staged backup artifact changed while it was verified: %s", path)
		}
	}
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expectedDigest {
		return fmt.Errorf("staged backup artifact digest changed: expected %s, found %s", expectedDigest, actual)
	}
	return nil
}
