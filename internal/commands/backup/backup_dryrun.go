package backup

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"docker-manager/internal/docker"
)

func printBackupDryRunPlan(w io.Writer, outputDir string, manifest BackupManifest, opts BackupOptions) {
	fmt.Fprintf(w, "备份 dry-run 计划: %s\n", outputDir)
	fmt.Fprintf(w, "  来源 Docker: %s\n", docker.Endpoint())
	fmt.Fprintf(w, "  容器数量: %d\n", len(manifest.Containers))
	fmt.Fprintf(w, "  源平台: %s\n", manifest.SourcePlatform)
	fmt.Fprintf(w, "  将生成文件: %s, %s, %s\n", backupManifestName, backupInspectName, backupComposeName)
	if opts.IncludeImage {
		fmt.Fprintln(w, "  镜像归档: 启用")
	} else {
		fmt.Fprintln(w, "  镜像归档: 跳过 (--no-image)")
	}
	if opts.Bundle {
		archivePath := opts.BundleOutput
		if archivePath == "" {
			archivePath = outputDir + ".tar.gz"
		}
		if archiveOpts, err := archiveOptionsFromBackup(opts); err == nil {
			archivePath = backupArchiveOutputPath(archivePath, archiveOpts)
		}
		fmt.Fprintf(w, "  离线包: %s\n", archivePath)
		if opts.Encrypt {
			fmt.Fprintln(w, "  加密: 启用")
		}
		if opts.SplitSize != "" {
			fmt.Fprintf(w, "  分卷大小: %s\n", opts.SplitSize)
		}
		fmt.Fprintf(w, "  将生成附加文件: %s, %s, %s\n", backupReadmeName, backupRestoreName, backupChecksumName)
	}
	for _, entry := range manifest.Containers {
		location := outputDir
		if entry.Path != "" {
			location = filepath.Join(outputDir, filepath.FromSlash(entry.Path))
		}
		fmt.Fprintf(w, "  - 容器: %s source=%s location=%s\n", entry.ContainerName, entry.SourceName, location)
		if entry.Image != "" {
			fmt.Fprintf(w, "    镜像: %s\n", entry.Image)
		}
		if entry.ImageArchive != "" {
			fmt.Fprintf(w, "    镜像归档路径: %s\n", entry.ImageArchive)
		}
		if len(entry.Networks) > 0 {
			fmt.Fprintf(w, "    network 元数据: %s\n", resourceRefNames(entry.Networks))
		}
		if len(entry.Volumes) > 0 {
			fmt.Fprintf(w, "    volume 元数据: %s\n", resourceRefNames(entry.Volumes))
		}
		if len(entry.Mounts) > 0 {
			fmt.Fprintf(w, "    挂载依赖: %s\n", backupMountSummary(entry.Mounts))
		}
		if len(entry.Devices) > 0 {
			fmt.Fprintf(w, "    设备依赖: %s\n", backupDeviceSummary(entry.Devices))
		}
	}
	fmt.Fprintln(w, "  校验: dry-run 已确认 inspect 可读、compose 可生成、network/volume 元数据可读取；不会写入文件或导出镜像。")
}

func backupMountSummary(refs []BackupMountRef) string {
	if len(refs) == 0 {
		return "-"
	}
	values := make([]string, 0, len(refs))
	for _, ref := range refs {
		target := ref.Destination
		if target == "" {
			target = ref.Source
		}
		values = append(values, fmt.Sprintf("%s:%s(%s)", ref.Type, target, ref.Verification))
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func backupDeviceSummary(refs []BackupDeviceRef) string {
	if len(refs) == 0 {
		return "-"
	}
	values := make([]string, 0, len(refs))
	for _, ref := range refs {
		target := ref.PathInContainer
		if target == "" {
			target = ref.PathOnHost
		}
		values = append(values, fmt.Sprintf("%s(%s)", target, ref.Verification))
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}
