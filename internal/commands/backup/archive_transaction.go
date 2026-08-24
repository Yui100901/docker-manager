package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	backupSplitTransactionVersion     = 1
	backupSplitTransactionFile        = "transaction.json"
	backupSplitTransactionMarkerBytes = 4 << 10
)

var errBackupTransactionLocked = errors.New("another split backup transaction is active")

type backupSplitTransactionMarker struct {
	Version int    `json:"version"`
	Archive string `json:"archive"`
	Staging string `json:"staging"`
}

type recoveredSplitTransaction struct {
	stagingDir string
	payload    []string
	parts      []string
}

func backupSplitTransactionPath(basePath string) string {
	return basePath + ".parts.pending.json"
}

func createBackupSplitTransaction(ctx context.Context, basePath, stagingDir string) (*os.File, error) {
	if err := checkBackupContext(ctx); err != nil {
		return nil, err
	}
	marker := backupSplitTransactionMarker{
		Version: backupSplitTransactionVersion,
		Archive: filepath.Base(basePath),
		Staging: filepath.Base(stagingDir),
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	stagedPath := filepath.Join(stagingDir, backupSplitTransactionFile)
	file, err := createBackupTransactionFile(stagedPath)
	if err != nil {
		return nil, fmt.Errorf("create split backup transaction marker: %w", err)
	}
	closeWithError := func(baseErr error) (*os.File, error) {
		return nil, errors.Join(baseErr, file.Close())
	}
	written, err := file.Write(data)
	if err != nil {
		return closeWithError(err)
	}
	if written != len(data) {
		return closeWithError(io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return closeWithError(err)
	}
	digest := sha256.Sum256(data)
	if err := verifyBackupStagedFile(ctx, stagedPath, int64(len(data)), hex.EncodeToString(digest[:])); err != nil {
		return closeWithError(err)
	}
	if err := lockBackupTransactionFile(file); err != nil {
		return closeWithError(fmt.Errorf("lock split backup transaction: %w", err))
	}
	if err := publishBackupStagedFile(stagedPath, backupSplitTransactionPath(basePath)); err != nil {
		unlockErr := unlockBackupTransactionFile(file)
		return closeWithError(errors.Join(err, unlockErr))
	}
	return file, nil
}

func recoverInterruptedSplitArchive(ctx context.Context, basePath string) error {
	pendingPath := backupSplitTransactionPath(basePath)
	pendingInfo, err := os.Lstat(pendingPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect split backup transaction marker: %w", err)
	}
	if !pendingInfo.Mode().IsRegular() || pendingInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("split backup transaction marker is not a regular file: %s", pendingPath)
	}
	file, err := openBackupTransactionFile(pendingPath)
	if err != nil {
		return fmt.Errorf("open split backup transaction marker: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect opened split backup transaction marker: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() != pendingInfo.Size() || !os.SameFile(pendingInfo, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("split backup transaction marker changed while opening: %s", pendingPath)
	}
	if err := lockBackupTransactionFile(file); err != nil {
		_ = file.Close()
		return fmt.Errorf("lock split backup transaction %s: %w", pendingPath, err)
	}
	locked := true
	defer func() {
		if locked {
			_ = unlockBackupTransactionFile(file)
			_ = file.Close()
		}
	}()

	currentInfo, err := os.Lstat(pendingPath)
	if err != nil || !currentInfo.Mode().IsRegular() || currentInfo.Mode()&os.ModeSymlink != 0 ||
		currentInfo.Size() != openedInfo.Size() || !os.SameFile(currentInfo, openedInfo) {
		return fmt.Errorf("split backup transaction marker changed while locking: %s", pendingPath)
	}
	marker, err := readBackupSplitTransactionMarker(file, basePath)
	if err != nil {
		return err
	}
	recovered, err := inspectRecoveredSplitTransaction(ctx, basePath, marker, openedInfo)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(backupPartsManifestPath(basePath)); os.IsNotExist(err) {
		for _, partPath := range recovered.parts {
			if err := os.Remove(partPath); err != nil {
				return fmt.Errorf("remove interrupted split backup part %s: %w", partPath, err)
			}
		}
		if len(recovered.parts) > 0 {
			if err := syncBackupDirectory(filepath.Dir(basePath)); err != nil {
				return fmt.Errorf("sync recovered split backup parts: %w", err)
			}
		}
	} else if err != nil {
		return fmt.Errorf("inspect split backup commit manifest: %w", err)
	}
	for _, stagedPath := range recovered.payload {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		if err := os.Remove(stagedPath); err != nil {
			return fmt.Errorf("remove interrupted split backup staged payload %s: %w", stagedPath, err)
		}
	}
	if err := removeBackupSplitTransactionMarker(file, basePath); err != nil {
		return err
	}
	unlockErr := unlockBackupTransactionFile(file)
	closeErr := file.Close()
	locked = false
	cleanupErr := removeRecoveredSplitStaging(recovered)
	return errors.Join(unlockErr, closeErr, cleanupErr)
}

func readBackupSplitTransactionMarker(file *os.File, basePath string) (backupSplitTransactionMarker, error) {
	var marker backupSplitTransactionMarker
	info, err := file.Stat()
	if err != nil {
		return marker, err
	}
	if info.Size() <= 0 || info.Size() > backupSplitTransactionMarkerBytes {
		return marker, fmt.Errorf("split backup transaction marker has invalid size %d", info.Size())
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return marker, err
	}
	data, err := io.ReadAll(io.LimitReader(file, backupSplitTransactionMarkerBytes+1))
	if err != nil {
		return marker, err
	}
	if int64(len(data)) != info.Size() || len(data) > backupSplitTransactionMarkerBytes {
		return marker, fmt.Errorf("split backup transaction marker changed while reading")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return marker, fmt.Errorf("parse split backup transaction marker: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return marker, fmt.Errorf("split backup transaction marker contains trailing data")
	}
	if marker.Version != backupSplitTransactionVersion || marker.Archive != filepath.Base(basePath) {
		return marker, fmt.Errorf("split backup transaction marker does not match %s", basePath)
	}
	if marker.Staging == "" || marker.Staging == "." || marker.Staging != filepath.Base(marker.Staging) ||
		strings.ContainsAny(marker.Staging, `/\\:`) || !strings.HasPrefix(marker.Staging, ".dm-backup-staging-") {
		return marker, fmt.Errorf("split backup transaction marker contains an invalid staging directory")
	}
	return marker, nil
}

func inspectRecoveredSplitTransaction(ctx context.Context, basePath string, marker backupSplitTransactionMarker, pendingInfo os.FileInfo) (recoveredSplitTransaction, error) {
	if err := checkBackupContext(ctx); err != nil {
		return recoveredSplitTransaction{}, err
	}
	stagingDir := filepath.Join(filepath.Dir(basePath), marker.Staging)
	stagingInfo, err := os.Lstat(stagingDir)
	if err != nil {
		return recoveredSplitTransaction{}, fmt.Errorf("inspect interrupted split backup staging directory: %w", err)
	}
	if !stagingInfo.IsDir() || stagingInfo.Mode()&os.ModeSymlink != 0 {
		return recoveredSplitTransaction{}, fmt.Errorf("interrupted split backup staging path is not a real directory: %s", stagingDir)
	}
	markerPath := filepath.Join(stagingDir, backupSplitTransactionFile)
	stagedMarkerInfo, err := os.Lstat(markerPath)
	if err != nil {
		return recoveredSplitTransaction{}, fmt.Errorf("inspect staged split backup transaction marker: %w", err)
	}
	if !stagedMarkerInfo.Mode().IsRegular() || stagedMarkerInfo.Mode()&os.ModeSymlink != 0 ||
		stagedMarkerInfo.Size() != pendingInfo.Size() || !os.SameFile(stagedMarkerInfo, pendingInfo) {
		return recoveredSplitTransaction{}, fmt.Errorf("split backup transaction marker is not owned by staging directory %s", stagingDir)
	}

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return recoveredSplitTransaction{}, err
	}
	if len(entries) > maxBackupArchivePartCount+2 {
		return recoveredSplitTransaction{}, fmt.Errorf("interrupted split backup staging exceeds the %d-entry limit", maxBackupArchivePartCount+2)
	}
	recovered := recoveredSplitTransaction{stagingDir: stagingDir}
	for _, entry := range entries {
		if err := checkBackupContext(ctx); err != nil {
			return recoveredSplitTransaction{}, err
		}
		path := filepath.Join(stagingDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return recoveredSplitTransaction{}, err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return recoveredSplitTransaction{}, fmt.Errorf("interrupted split backup staging contains a non-regular entry: %s", path)
		}
		switch entry.Name() {
		case backupSplitTransactionFile:
			continue
		case "parts.json":
			recovered.payload = append(recovered.payload, path)
			continue
		}
		index, ok := splitPartIndexForName(basePath, entry.Name())
		if !ok {
			return recoveredSplitTransaction{}, fmt.Errorf("interrupted split backup staging contains an unexpected file: %s", path)
		}
		recovered.payload = append(recovered.payload, path)
		finalPath := splitPartPath(basePath, index)
		finalInfo, err := os.Lstat(finalPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return recoveredSplitTransaction{}, err
		}
		if !finalInfo.Mode().IsRegular() || finalInfo.Mode()&os.ModeSymlink != 0 ||
			finalInfo.Size() != info.Size() || !os.SameFile(finalInfo, info) {
			return recoveredSplitTransaction{}, fmt.Errorf("refusing to remove unowned split backup part: %s", finalPath)
		}
		recovered.parts = append(recovered.parts, finalPath)
	}
	return recovered, nil
}

func splitPartIndexForName(basePath, name string) (int, bool) {
	prefix := filepath.Base(basePath) + ".part-"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if len(suffix) != 3 {
		return 0, false
	}
	index, err := strconv.Atoi(suffix)
	if err != nil || index <= 0 || index > maxBackupArchivePartCount || filepath.Base(splitPartPath(basePath, index)) != name {
		return 0, false
	}
	return index, true
}

func removeBackupSplitTransactionMarker(file *os.File, basePath string) error {
	pendingPath := backupSplitTransactionPath(basePath)
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	pendingInfo, err := os.Lstat(pendingPath)
	if err != nil {
		return fmt.Errorf("inspect split backup transaction marker before removal: %w", err)
	}
	if !pendingInfo.Mode().IsRegular() || pendingInfo.Mode()&os.ModeSymlink != 0 ||
		pendingInfo.Size() != openedInfo.Size() || !os.SameFile(pendingInfo, openedInfo) {
		return fmt.Errorf("refusing to remove a replaced split backup transaction marker: %s", pendingPath)
	}
	if err := os.Remove(pendingPath); err != nil {
		return fmt.Errorf("remove split backup transaction marker: %w", err)
	}
	if err := syncBackupDirectory(filepath.Dir(basePath)); err != nil {
		return fmt.Errorf("sync split backup transaction marker removal: %w", err)
	}
	return nil
}

func removeRecoveredSplitStaging(recovered recoveredSplitTransaction) error {
	return removeSplitStagingMarker(recovered.stagingDir)
}

func removeSplitStagingPayload(ctx context.Context, stagingDir string) error {
	info, err := os.Lstat(stagingDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("split backup staging path is not a real directory: %s", stagingDir)
	}
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return err
	}
	if len(entries) > maxBackupArchivePartCount+2 {
		return fmt.Errorf("split backup staging exceeds the %d-entry limit", maxBackupArchivePartCount+2)
	}
	for _, entry := range entries {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		if entry.Name() == backupSplitTransactionFile {
			continue
		}
		path := filepath.Join(stagingDir, entry.Name())
		entryInfo, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !entryInfo.Mode().IsRegular() || entryInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("split backup staging contains a non-regular entry: %s", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func removeSplitStagingMarker(stagingDir string) error {
	markerPath := filepath.Join(stagingDir, backupSplitTransactionFile)
	var cleanupErrs []error
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		cleanupErrs = append(cleanupErrs, err)
	}
	if err := os.Remove(stagingDir); err != nil && !os.IsNotExist(err) {
		cleanupErrs = append(cleanupErrs, err)
	}
	return errors.Join(cleanupErrs...)
}

func rejectExistingBackupSplitArtifacts(basePath string) error {
	for _, path := range []string{backupPartsManifestPath(basePath), backupSplitTransactionPath(basePath)} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("backup artifact already exists: %s", path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Dir(basePath))
	if err != nil {
		return err
	}
	prefix := filepath.Base(basePath) + ".part-"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			return fmt.Errorf("backup artifact already exists: %s", filepath.Join(filepath.Dir(basePath), entry.Name()))
		}
	}
	return nil
}
