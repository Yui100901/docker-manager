package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func readLimitedBackupJSON(path string, limit int64, value interface{}) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("JSON input must be a regular file: %s", path)
	}
	data, err := readLimitedBackupFile(context.Background(), path, limit)
	if err != nil {
		return err
	}
	if err := validateBackupJSONStructure(data); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func validateBackupJSONStructure(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var depth int
	var tokens int64
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		tokens++
		if tokens > maxBackupJSONTokens {
			return fmt.Errorf("JSON input exceeds the %d-token limit", maxBackupJSONTokens)
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				if depth > maxBackupJSONDepth {
					return fmt.Errorf("JSON input exceeds the maximum depth of %d", maxBackupJSONDepth)
				}
			case '}', ']':
				depth--
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("JSON input has unbalanced containers")
	}
	return nil
}

func validateRestoreJSONBudget(backupDir string, manifest BackupManifest) error {
	return validateRestoreJSONBudgetWithLimit(backupDir, manifest, maxBackupJSONTotalBytes)
}

func validateRestoreJSONBudgetWithLimit(backupDir string, manifest BackupManifest, totalLimit int64) error {
	if len(manifest.Containers) > maxBackupContainers {
		return fmt.Errorf("backup manifest exceeds the %d-container limit", maxBackupContainers)
	}
	budget := newBackupByteBudget("restore JSON inputs", totalLimit)
	seen := make(map[string]struct{})
	add := func(path string, perFileLimit int64, description string) error {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		absolute = filepath.Clean(absolute)
		if _, exists := seen[absolute]; exists {
			return nil
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", description, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must be a regular file", description)
		}
		if info.Size() > perFileLimit {
			return fmt.Errorf("%s exceeds the %d-byte limit", description, perFileLimit)
		}
		if err := budget.Add(info.Size()); err != nil {
			return err
		}
		seen[absolute] = struct{}{}
		return nil
	}
	if err := add(filepath.Join(backupDir, backupManifestName), maxBackupManifestJSONBytes, "backup manifest"); err != nil {
		return err
	}
	var resourceRefs int
	for _, entry := range manifest.Containers {
		entryDir, err := restoreEntryDir(backupDir, entry)
		if err != nil {
			return err
		}
		inspectFile := entry.InspectFile
		if inspectFile == "" {
			inspectFile = backupInspectName
		}
		inspectPath, err := backupFilePath(entryDir, inspectFile)
		if err != nil {
			return err
		}
		if err := add(inspectPath, maxBackupInspectJSONBytes, "container inspect JSON"); err != nil {
			return err
		}
		resourceRefs += len(entry.Networks) + len(entry.Volumes)
		if resourceRefs > maxBackupResourceRefs {
			return fmt.Errorf("backup manifest exceeds the %d-resource-reference limit", maxBackupResourceRefs)
		}
		for _, ref := range entry.Networks {
			path, err := backupFilePath(entryDir, ref.File)
			if err != nil {
				return err
			}
			if err := add(path, maxBackupResourceJSONBytes, "network JSON"); err != nil {
				return err
			}
		}
		for _, ref := range entry.Volumes {
			path, err := backupFilePath(entryDir, ref.File)
			if err != nil {
				return err
			}
			if err := add(path, maxBackupResourceJSONBytes, "volume JSON"); err != nil {
				return err
			}
		}
	}
	return nil
}
