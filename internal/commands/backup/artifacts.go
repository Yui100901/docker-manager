package backup

import (
	"docker-manager/internal/commands/reverse"
	"encoding/json"
	"fmt"
	"github.com/docker/docker/api/types/container"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

func writeComposeFile(path string, inspect container.InspectResponse) error {
	parser := reverse.NewParser(inspect, reverse.ReverseOptions{
		PreserveVolumes: true,
		ReverseType:     reverse.ReverseCompose,
	})
	result := reverse.NewReverseResult([]reverse.ParsedResult{parser.ToResult()}, reverse.ReverseOptions{
		PreserveVolumes: true,
		ReverseType:     reverse.ReverseCompose,
	})
	return os.WriteFile(path, []byte(result.DockerComposeFileString()), 0644)
}

func writeJSONFile(path string, value interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func writeBackupBundleArtifacts(outputDir string, manifest BackupManifest) error {
	if err := writeBackupReadme(filepath.Join(outputDir, backupReadmeName), manifest); err != nil {
		return fmt.Errorf("write readme: %w", err)
	}
	if err := writeRestoreScript(filepath.Join(outputDir, backupRestoreName)); err != nil {
		return fmt.Errorf("write restore script: %w", err)
	}
	if err := writeChecksums(outputDir); err != nil {
		return fmt.Errorf("write checksums: %w", err)
	}
	return nil
}

func writeBackupReadme(path string, manifest BackupManifest) error {
	var sb strings.Builder
	sb.WriteString("# docker-manager offline backup\n\n")
	sb.WriteString("## Backup metadata\n\n")
	sb.WriteString("- Created at: `" + manifest.CreatedAt + "`\n")
	sb.WriteString("- Created by: `dm " + valueOrUnknown(manifest.Tool.Version) + "`\n")
	sb.WriteString("- Source commit: `" + valueOrUnknown(manifest.Tool.Commit) + "`\n")
	sb.WriteString("- Source build date: `" + valueOrUnknown(manifest.Tool.BuildDate) + "`\n")
	sb.WriteString("- Source platform: `" + valueOrUnknown(manifest.SourcePlatform) + "`\n\n")
	sb.WriteString("## Contents\n\n")
	sb.WriteString("- `manifest.json`: migration manifest; `containers` contains one or more container entries\n")
	if len(manifest.Containers) == 1 && manifest.Containers[0].Path == "" {
		sb.WriteString("- `container.inspect.json`: Docker inspect JSON\n")
		sb.WriteString("- `docker-compose.yml`: compose generated from the container\n")
		if manifest.Containers[0].ImageArchive != "" {
			sb.WriteString("- `" + manifest.Containers[0].ImageArchive + "`: image archive\n")
		}
		if len(manifest.Containers[0].Networks) > 0 {
			sb.WriteString("- `networks/`: network metadata\n")
		}
		if len(manifest.Containers[0].Volumes) > 0 {
			sb.WriteString("- `volumes/`: volume metadata\n")
		}
	} else {
		sb.WriteString("- `containers/`: per-container backup directories\n")
	}
	sb.WriteString("- `checksums.txt`: SHA256 checksums\n")
	sb.WriteString("- `restore.sh`: helper restore script\n\n")
	sb.WriteString("## Prerequisites\n\n")
	sb.WriteString("- Install `dm` on the target host and make sure it is available in `PATH`.\n")
	sb.WriteString("- The target host must be able to reach a running Docker daemon with permission to load images and create networks, volumes and containers.\n")
	sb.WriteString("- Review container names, ports, bind mounts, named volumes and custom networks before using `--replace`.\n")
	sb.WriteString("- If this backup contains bind mounts, the target host must already have compatible host paths and permissions.\n\n")
	sb.WriteString("## Checksum verification\n\n")
	sb.WriteString("`dm restore` verifies `checksums.txt` by default before it touches Docker. If verification fails, restore stops before loading images or creating resources. Use `--skip-checksum` only after manually confirming the package integrity.\n\n")
	sb.WriteString("## Restore\n\n")
	sb.WriteString("```bash\n")
	sb.WriteString("dm restore .\n")
	sb.WriteString("# or restore directly from the archive:\n")
	sb.WriteString("dm restore <backup>.tar.gz\n")
	sb.WriteString("```\n\n")
	sb.WriteString("## Containers\n\n")
	for _, entry := range manifest.Containers {
		location := "."
		if entry.Path != "" {
			location = entry.Path
		}
		line := "- `" + entry.ContainerName + "` from `" + location + "`"
		if entry.Image != "" {
			line += " image `" + entry.Image + "`"
		}
		sb.WriteString(line + "\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

func writeRestoreScript(path string) error {
	content := `#!/usr/bin/env sh
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if ! command -v dm >/dev/null 2>&1; then
  echo "Error: dm is not available in PATH. Install docker-manager on the target host first." >&2
  exit 127
fi

echo "docker-manager restore helper"
dm version || true
echo "Backup directory: $DIR"
echo "Prerequisite: Docker daemon must be reachable and the current user must be allowed to manage Docker resources."
if [ -f "$DIR/checksums.txt" ]; then
  echo "Checksum: dm restore will verify checksums.txt by default. Use --skip-checksum only after manual verification."
else
  echo "Checksum: checksums.txt not found; dm restore will skip checksum verification."
fi

dm restore "$DIR" "$@"
`
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		return err
	}
	return os.Chmod(path, 0755)
}

func currentSourcePlatform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func resourceRefNames(refs []BackupResourceRef) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.Name != "" {
			names = append(names, ref.Name)
		}
	}
	if len(names) == 0 {
		return "-"
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func checksumPlanText(skip bool, checksumPath string) string {
	if skip {
		return "跳过 (--skip-checksum)"
	}
	if _, err := os.Stat(checksumPath); err == nil {
		return "已校验 checksums.txt"
	}
	return "未找到 checksums.txt，将跳过校验"
}
