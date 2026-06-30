package backup

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func writeChecksums(root string) error {
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == backupChecksumName {
			return nil
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s  %s", sum, rel))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(lines)
	return os.WriteFile(filepath.Join(root, backupChecksumName), []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

func verifyBackupChecksums(root string) (bool, error) {
	checksumPath := filepath.Join(root, backupChecksumName)
	file, err := os.Open(checksumPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("Checksum file not found, skip verification: %s", checksumPath)
			return false, nil
		}
		return false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	checked := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		expected, rel, err := parseChecksumLine(line)
		if err != nil {
			return true, fmt.Errorf("%s:%d: %w", backupChecksumName, lineNumber, err)
		}
		if rel == backupChecksumName {
			continue
		}
		target, err := safeExtractPath(root, rel)
		if err != nil {
			return true, fmt.Errorf("%s:%d: %w", backupChecksumName, lineNumber, err)
		}
		actual, err := fileSHA256(target)
		if err != nil {
			return true, fmt.Errorf("checksum target %s: %w", rel, err)
		}
		if !strings.EqualFold(actual, expected) {
			return true, fmt.Errorf("checksum mismatch for %s: expected %s actual %s", rel, expected, actual)
		}
		checked++
	}
	if err := scanner.Err(); err != nil {
		return true, err
	}
	log.Printf("Checksum verification checked files: %d", checked)
	return true, nil
}

func parseChecksumLine(line string) (string, string, error) {
	sum, rel, ok := strings.Cut(line, "  ")
	if !ok {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return "", "", fmt.Errorf("invalid checksum line")
		}
		sum, rel = fields[0], fields[1]
	}
	sum = strings.TrimSpace(sum)
	rel = strings.TrimSpace(rel)
	if len(sum) != sha256.Size*2 {
		return "", "", fmt.Errorf("invalid sha256 length")
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return "", "", fmt.Errorf("invalid sha256: %w", err)
	}
	if rel == "" {
		return "", "", fmt.Errorf("empty checksum path")
	}
	return sum, filepath.ToSlash(rel), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
