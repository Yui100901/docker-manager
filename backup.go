package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"docker-manager/docker"
	"docker-manager/reverse"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

const (
	backupManifestName = "manifest.json"
	backupInspectName  = "container.inspect.json"
	backupComposeName  = "docker-compose.yml"
	backupReadmeName   = "README.md"
	backupRestoreName  = "restore.sh"
	backupChecksumName = "checksums.txt"
	backupRoot         = "docker-backups"
)

type backupDockerService interface {
	InspectContainer(ctx context.Context, name string) (container.InspectResponse, error)
	SaveImage(ctx context.Context, refs []string, outputFile string) error
	LoadImage(ctx context.Context, inputFile string) error
	InspectNetwork(ctx context.Context, name string) (network.Inspect, error)
	CreateNetwork(ctx context.Context, inspect network.Inspect) error
	InspectVolume(ctx context.Context, name string) (volume.Volume, error)
	CreateVolume(ctx context.Context, vol volume.Volume) error
	ContainerExists(ctx context.Context, name string) (bool, error)
	RemoveContainer(ctx context.Context, name string) error
	CreateContainer(ctx context.Context, inspect container.InspectResponse, name string) (string, error)
	StartContainer(ctx context.Context, id string) error
}

var newBackupDockerService = func() (backupDockerService, error) {
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerBackupService{cli: cli}, nil
}

type dockerBackupService struct {
	cli *client.Client
}

type BackupOptions struct {
	OutputDir    string
	IncludeImage bool
	DryRun       bool
	Bundle       bool
	BundleOutput string
}

type RestoreOptions struct {
	Name         string
	Replace      bool
	NoStart      bool
	DryRun       bool
	SkipChecksum bool
}

type BackupManifest struct {
	Version       int                 `json:"version"`
	CreatedAt     string              `json:"created_at"`
	ContainerName string              `json:"container_name"`
	SourceName    string              `json:"source_name"`
	Image         string              `json:"image,omitempty"`
	ImageArchive  string              `json:"image_archive,omitempty"`
	InspectFile   string              `json:"inspect_file"`
	ComposeFile   string              `json:"compose_file"`
	Networks      []BackupResourceRef `json:"networks,omitempty"`
	Volumes       []BackupResourceRef `json:"volumes,omitempty"`
}

type BackupResourceRef struct {
	Name string `json:"name"`
	File string `json:"file"`
}

func newBackupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "备份 Docker 资源",
	}
	cmd.AddCommand(newBackupContainerCommand())
	return cmd
}

func newBackupContainerCommand() *cobra.Command {
	opts := BackupOptions{IncludeImage: true}
	cmd := &cobra.Command{
		Use:   "container <name> [backup-dir]",
		Short: "备份容器 inspect、镜像、compose、volume 和 network 元数据",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.OutputDir = ""
			if len(args) == 2 {
				opts.OutputDir = args[1]
			}
			outputDir, err := backupContainer(cmd.Context(), args[0], opts)
			if err != nil {
				return fmt.Errorf("backup container failed: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backup created: %s\n", outputDir)
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.IncludeImage, "include-image", true, "导出容器镜像 tar")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "只预览备份动作，不写入文件")
	cmd.Flags().BoolVar(&opts.Bundle, "bundle", false, "生成离线迁移包 tar.gz，并附带 README、restore 脚本和 checksums")
	cmd.Flags().StringVar(&opts.BundleOutput, "output", "", "离线迁移包输出路径，默认 <backup-dir>.tar.gz")
	return cmd
}

func newRestoreCommand() *cobra.Command {
	opts := RestoreOptions{}
	cmd := &cobra.Command{
		Use:   "restore <backup-dir-or-archive>",
		Short: "从 backup container 生成的目录或 tar.gz 离线包恢复镜像、网络、volume 和容器",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := restoreBackup(cmd.Context(), args[0], opts); err != nil {
				return fmt.Errorf("restore failed: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restore completed: %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Name, "name", "", "恢复为新的容器名，默认使用备份中的容器名")
	cmd.Flags().BoolVar(&opts.Replace, "replace", false, "如果目标容器已存在则先删除")
	cmd.Flags().BoolVar(&opts.NoStart, "no-start", false, "只创建容器，不启动")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "只预览恢复动作，不修改 Docker")
	cmd.Flags().BoolVar(&opts.SkipChecksum, "skip-checksum", false, "跳过 checksums.txt 完整性校验")
	return cmd
}

func backupContainer(ctx context.Context, name string, opts BackupOptions) (string, error) {
	svc, err := newBackupDockerService()
	if err != nil {
		return "", err
	}
	inspect, err := svc.InspectContainer(ctx, name)
	if err != nil {
		return "", err
	}

	containerName := normalizeContainerName(inspect.Name)
	if containerName == "" {
		containerName = normalizeContainerName(name)
	}
	outputDir := opts.OutputDir
	if outputDir == "" {
		outputDir = defaultBackupDir(time.Now(), containerName)
	}

	manifest := BackupManifest{
		Version:       1,
		CreatedAt:     time.Now().Format(time.RFC3339),
		ContainerName: containerName,
		SourceName:    name,
		InspectFile:   backupInspectName,
		ComposeFile:   backupComposeName,
	}
	if inspect.Config != nil {
		manifest.Image = inspect.Config.Image
	}

	if opts.DryRun {
		log.Printf("Dry run backup container: name=%s output=%s includeImage=%v", name, outputDir, opts.IncludeImage)
		return outputDir, nil
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", err
	}
	if err := writeJSONFile(filepath.Join(outputDir, backupInspectName), inspect); err != nil {
		return "", fmt.Errorf("write inspect: %w", err)
	}
	if err := writeComposeFile(filepath.Join(outputDir, backupComposeName), inspect); err != nil {
		return "", fmt.Errorf("write compose: %w", err)
	}

	if opts.IncludeImage && manifest.Image != "" {
		imageDir := filepath.Join(outputDir, "images")
		if err := os.MkdirAll(imageDir, 0755); err != nil {
			return "", err
		}
		imageFile := filepath.Join("images", safeBackupName(manifest.Image)+".tar")
		if err := svc.SaveImage(ctx, []string{manifest.Image}, filepath.Join(outputDir, imageFile)); err != nil {
			return "", fmt.Errorf("save image %s: %w", manifest.Image, err)
		}
		manifest.ImageArchive = filepath.ToSlash(imageFile)
	}

	networks, err := backupNetworks(ctx, svc, outputDir, inspect)
	if err != nil {
		return "", err
	}
	manifest.Networks = networks

	volumes, err := backupVolumes(ctx, svc, outputDir, inspect)
	if err != nil {
		return "", err
	}
	manifest.Volumes = volumes

	if err := writeJSONFile(filepath.Join(outputDir, backupManifestName), manifest); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}
	if opts.Bundle {
		if err := writeBackupBundleArtifacts(outputDir, manifest); err != nil {
			return "", err
		}
		archivePath := opts.BundleOutput
		if archivePath == "" {
			archivePath = outputDir + ".tar.gz"
		}
		if err := createBackupArchive(outputDir, archivePath); err != nil {
			return "", err
		}
		log.Printf("Backup bundle: %s", archivePath)
	}
	log.Printf("Backup summary: container=%s output=%s image=%v networks=%d volumes=%d", containerName, outputDir, manifest.ImageArchive != "", len(manifest.Networks), len(manifest.Volumes))
	return outputDir, nil
}

func restoreBackup(ctx context.Context, backupDir string, opts RestoreOptions) error {
	resolvedDir, cleanup, err := resolveRestoreBackupDir(backupDir)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	backupDir = resolvedDir

	if !opts.SkipChecksum {
		verified, err := verifyBackupChecksums(backupDir)
		if err != nil {
			return fmt.Errorf("verify checksums: %w", err)
		}
		if verified {
			log.Printf("Checksum verification passed: %s", backupDir)
		}
	} else {
		log.Printf("Skip checksum verification: %s", backupDir)
	}

	svc, err := newBackupDockerService()
	if err != nil {
		return err
	}
	manifest, err := readBackupManifest(backupDir)
	if err != nil {
		return err
	}
	inspect, err := readContainerInspect(backupDir, manifest)
	if err != nil {
		return err
	}
	targetName := opts.Name
	if targetName == "" {
		targetName = manifest.ContainerName
	}
	if targetName == "" {
		targetName = normalizeContainerName(inspect.Name)
	}
	if targetName == "" {
		return fmt.Errorf("backup does not contain a container name; use --name")
	}

	exists, err := svc.ContainerExists(ctx, targetName)
	if err != nil {
		return err
	}
	if exists && !opts.Replace {
		return fmt.Errorf("container %s already exists; use --replace to overwrite", targetName)
	}

	if opts.DryRun {
		log.Printf("Dry run restore: backup=%s container=%s replace=%v noStart=%v", backupDir, targetName, opts.Replace, opts.NoStart)
		return nil
	}

	if manifest.ImageArchive != "" {
		imagePath, err := backupFilePath(backupDir, manifest.ImageArchive)
		if err != nil {
			return err
		}
		if err := svc.LoadImage(ctx, imagePath); err != nil {
			return fmt.Errorf("load image archive %s: %w", imagePath, err)
		}
	}

	for _, ref := range manifest.Networks {
		netMeta, err := readNetworkInspect(backupDir, ref)
		if err != nil {
			return err
		}
		if err := svc.CreateNetwork(ctx, netMeta); err != nil {
			return fmt.Errorf("restore network %s: %w", ref.Name, err)
		}
	}

	for _, ref := range manifest.Volumes {
		volMeta, err := readVolumeInspect(backupDir, ref)
		if err != nil {
			return err
		}
		if err := svc.CreateVolume(ctx, volMeta); err != nil {
			return fmt.Errorf("restore volume %s: %w", ref.Name, err)
		}
	}

	if exists {
		if err := svc.RemoveContainer(ctx, targetName); err != nil {
			return fmt.Errorf("remove existing container %s: %w", targetName, err)
		}
	}

	id, err := svc.CreateContainer(ctx, inspect, targetName)
	if err != nil {
		return fmt.Errorf("create container %s: %w", targetName, err)
	}
	if !opts.NoStart {
		if err := svc.StartContainer(ctx, id); err != nil {
			return fmt.Errorf("start container %s: %w", targetName, err)
		}
	}
	log.Printf("Restore summary: container=%s id=%s started=%v", targetName, id, !opts.NoStart)
	return nil
}

func backupNetworks(ctx context.Context, svc backupDockerService, outputDir string, inspect container.InspectResponse) ([]BackupResourceRef, error) {
	if inspect.NetworkSettings == nil || len(inspect.NetworkSettings.Networks) == 0 {
		return nil, nil
	}
	var names []string
	for name := range inspect.NetworkSettings.Networks {
		if isBuiltinNetwork(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var refs []BackupResourceRef
	for _, name := range names {
		netMeta, err := svc.InspectNetwork(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("inspect network %s: %w", name, err)
		}
		rel := filepath.Join("networks", safeBackupName(name)+".json")
		if err := writeJSONFile(filepath.Join(outputDir, rel), netMeta); err != nil {
			return nil, fmt.Errorf("write network %s: %w", name, err)
		}
		refs = append(refs, BackupResourceRef{Name: name, File: filepath.ToSlash(rel)})
	}
	return refs, nil
}

func backupVolumes(ctx context.Context, svc backupDockerService, outputDir string, inspect container.InspectResponse) ([]BackupResourceRef, error) {
	names := namedVolumes(inspect)
	if len(names) == 0 {
		return nil, nil
	}
	var refs []BackupResourceRef
	for _, name := range names {
		volMeta, err := svc.InspectVolume(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("inspect volume %s: %w", name, err)
		}
		rel := filepath.Join("volumes", safeBackupName(name)+".json")
		if err := writeJSONFile(filepath.Join(outputDir, rel), volMeta); err != nil {
			return nil, fmt.Errorf("write volume %s: %w", name, err)
		}
		refs = append(refs, BackupResourceRef{Name: name, File: filepath.ToSlash(rel)})
	}
	return refs, nil
}

func namedVolumes(inspect container.InspectResponse) []string {
	seen := map[string]bool{}
	for _, mount := range inspect.Mounts {
		if string(mount.Type) != "volume" || mount.Name == "" {
			continue
		}
		seen[mount.Name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

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
	sb.WriteString("## Contents\n\n")
	sb.WriteString("- `manifest.json`: backup manifest\n")
	sb.WriteString("- `container.inspect.json`: Docker inspect JSON\n")
	sb.WriteString("- `docker-compose.yml`: compose generated from the container\n")
	if manifest.ImageArchive != "" {
		sb.WriteString("- `" + manifest.ImageArchive + "`: image archive\n")
	}
	if len(manifest.Networks) > 0 {
		sb.WriteString("- `networks/`: network metadata\n")
	}
	if len(manifest.Volumes) > 0 {
		sb.WriteString("- `volumes/`: volume metadata\n")
	}
	sb.WriteString("- `checksums.txt`: SHA256 checksums\n")
	sb.WriteString("- `restore.sh`: helper restore script\n\n")
	sb.WriteString("## Restore\n\n")
	sb.WriteString("```bash\n")
	sb.WriteString("dm restore .\n")
	sb.WriteString("# or restore directly from the archive:\n")
	sb.WriteString("dm restore <backup>.tar.gz\n")
	sb.WriteString("```\n\n")
	sb.WriteString("Container: `" + manifest.ContainerName + "`\n\n")
	if manifest.Image != "" {
		sb.WriteString("Image: `" + manifest.Image + "`\n\n")
	}
	sb.WriteString("Created at: `" + manifest.CreatedAt + "`\n")
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

func writeRestoreScript(path string) error {
	content := "#!/usr/bin/env sh\nset -eu\nDIR=$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\ndm restore \"$DIR\" \"$@\"\n"
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		return err
	}
	return os.Chmod(path, 0755)
}

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

func createBackupArchive(sourceDir, archivePath string) error {
	if err := os.MkdirAll(filepath.Dir(archivePath), 0755); err != nil {
		return err
	}
	archiveAbs, _ := filepath.Abs(archivePath)
	file, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gz := gzip.NewWriter(file)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
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
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
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
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
}

func resolveRestoreBackupDir(path string) (string, func(), error) {
	if !isBackupArchive(path) {
		return path, nil, nil
	}
	tempDir, err := os.MkdirTemp("", "dm-restore-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	if err := extractBackupArchive(path, tempDir); err != nil {
		cleanup()
		return "", nil, err
	}
	root, err := findExtractedBackupRoot(tempDir)
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

func extractBackupArchive(archivePath, destDir string) error {
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
	for {
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
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}

func safeExtractPath(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	return filepath.Join(root, clean), nil
}

func findExtractedBackupRoot(tempDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(tempDir, backupManifestName)); err == nil {
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
		if _, err := os.Stat(filepath.Join(candidate, backupManifestName)); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("archive does not contain %s", backupManifestName)
}

func readBackupManifest(backupDir string) (BackupManifest, error) {
	var manifest BackupManifest
	data, err := os.ReadFile(filepath.Join(backupDir, backupManifestName))
	if err != nil {
		return manifest, fmt.Errorf("read manifest: %w", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Version != 1 {
		return manifest, fmt.Errorf("unsupported backup manifest version %d", manifest.Version)
	}
	return manifest, nil
}

func readContainerInspect(backupDir string, manifest BackupManifest) (container.InspectResponse, error) {
	inspectFile := manifest.InspectFile
	if inspectFile == "" {
		inspectFile = backupInspectName
	}
	var inspect container.InspectResponse
	path, err := backupFilePath(backupDir, inspectFile)
	if err != nil {
		return inspect, err
	}
	if err := readJSON(path, &inspect); err != nil {
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
	if err := readJSON(path, &value); err != nil {
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
	if err := readJSON(path, &value); err != nil {
		return value, fmt.Errorf("read volume %s: %w", ref.Name, err)
	}
	return value, nil
}

func backupFilePath(backupDir, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".." {
		return "", fmt.Errorf("invalid backup file path %q", rel)
	}
	return filepath.Join(backupDir, clean), nil
}

func readJSON(path string, value interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func normalizeContainerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func defaultBackupDir(now time.Time, containerName string) string {
	return filepath.Join(backupRoot, safeBackupName(containerName)+"-"+now.Format("20060102-150405"))
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

func (s *dockerBackupService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, name)
}

func (s *dockerBackupService) SaveImage(ctx context.Context, refs []string, outputFile string) error {
	reader, err := s.cli.ImageSave(ctx, refs)
	if err != nil {
		return err
	}
	defer reader.Close()

	file, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, reader)
	return err
}

func (s *dockerBackupService) LoadImage(ctx context.Context, inputFile string) error {
	file, err := os.Open(inputFile)
	if err != nil {
		return err
	}
	defer file.Close()

	resp, err := s.cli.ImageLoad(ctx, file, client.ImageLoadWithQuiet(false))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func (s *dockerBackupService) InspectNetwork(ctx context.Context, name string) (network.Inspect, error) {
	return s.cli.NetworkInspect(ctx, name, network.InspectOptions{})
}

func (s *dockerBackupService) CreateNetwork(ctx context.Context, inspect network.Inspect) error {
	if isBuiltinNetwork(inspect.Name) {
		return nil
	}
	if _, err := s.cli.NetworkInspect(ctx, inspect.Name, network.InspectOptions{}); err == nil {
		log.Printf("Skip existing network: %s", inspect.Name)
		return nil
	} else if !client.IsErrNotFound(err) {
		return err
	}
	enableIPv4 := inspect.EnableIPv4
	enableIPv6 := inspect.EnableIPv6
	createOptions := network.CreateOptions{
		Driver:     inspect.Driver,
		Scope:      inspect.Scope,
		EnableIPv4: &enableIPv4,
		EnableIPv6: &enableIPv6,
		IPAM:       &inspect.IPAM,
		Internal:   inspect.Internal,
		Attachable: inspect.Attachable,
		Ingress:    inspect.Ingress,
		ConfigOnly: inspect.ConfigOnly,
		Options:    inspect.Options,
		Labels:     inspect.Labels,
	}
	if inspect.ConfigFrom.Network != "" {
		createOptions.ConfigFrom = &inspect.ConfigFrom
	}
	_, err := s.cli.NetworkCreate(ctx, inspect.Name, createOptions)
	return err
}

func (s *dockerBackupService) InspectVolume(ctx context.Context, name string) (volume.Volume, error) {
	return s.cli.VolumeInspect(ctx, name)
}

func (s *dockerBackupService) CreateVolume(ctx context.Context, vol volume.Volume) error {
	if _, err := s.cli.VolumeInspect(ctx, vol.Name); err == nil {
		log.Printf("Skip existing volume: %s", vol.Name)
		return nil
	} else if !client.IsErrNotFound(err) {
		return err
	}
	_, err := s.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:       vol.Name,
		Driver:     vol.Driver,
		DriverOpts: vol.Options,
		Labels:     vol.Labels,
	})
	return err
}

func (s *dockerBackupService) ContainerExists(ctx context.Context, name string) (bool, error) {
	_, err := s.cli.ContainerInspect(ctx, name)
	if err == nil {
		return true, nil
	}
	if client.IsErrNotFound(err) {
		return false, nil
	}
	return false, err
}

func (s *dockerBackupService) RemoveContainer(ctx context.Context, name string) error {
	return s.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true, RemoveVolumes: false})
}

func (s *dockerBackupService) CreateContainer(ctx context.Context, inspect container.InspectResponse, name string) (string, error) {
	networkingConfig := restoreNetworkingConfig(inspect)
	resp, err := s.cli.ContainerCreate(ctx, inspect.Config, inspect.HostConfig, networkingConfig, nil, name)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (s *dockerBackupService) StartContainer(ctx context.Context, id string) error {
	return s.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func restoreNetworkingConfig(inspect container.InspectResponse) *network.NetworkingConfig {
	if inspect.NetworkSettings == nil || len(inspect.NetworkSettings.Networks) == 0 {
		return nil
	}
	endpoints := make(map[string]*network.EndpointSettings)
	for name, settings := range inspect.NetworkSettings.Networks {
		if settings == nil {
			continue
		}
		endpoints[name] = &network.EndpointSettings{
			IPAMConfig: settings.IPAMConfig,
			Links:      settings.Links,
			Aliases:    settings.Aliases,
			MacAddress: settings.MacAddress,
			DriverOpts: settings.DriverOpts,
			GwPriority: settings.GwPriority,
		}
	}
	if len(endpoints) == 0 {
		return nil
	}
	return &network.NetworkingConfig{EndpointsConfig: endpoints}
}
