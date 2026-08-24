package reverse

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func backupContainerInspectContext(ctx context.Context, name, backupDir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	inspect, err := containerManager.InspectContext(ctx, name)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	data, err := json.MarshalIndent(inspect, "", "  ")
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return "", err
	}

	backupPath := inspectBackupPath(backupDir, name)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := writePrivateOutput(backupPath, append(data, '\n')); err != nil {
		return "", err
	}
	return backupPath, nil
}

func inspectBackupDir(now time.Time) string {
	return filepath.Join(inspectBackupRoot, now.Format("20060102-150405"))
}

func inspectBackupPath(backupDir, name string) string {
	return filepath.Join(backupDir, sanitizeBackupFileName(name)+".inspect.json")
}

func sanitizeBackupFileName(name string) string {
	name = strings.TrimPrefix(name, "/")
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
		return "container"
	}
	return sb.String()
}
