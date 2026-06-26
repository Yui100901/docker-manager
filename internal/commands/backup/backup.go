package backup

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
	"runtime"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/commands/reverse"
	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	"docker-manager/internal/resourcefilter"
	"docker-manager/internal/version"

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
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, name string) (container.InspectResponse, error)
	SaveImage(ctx context.Context, refs []string, outputFile string) error
	LoadImage(ctx context.Context, inputFile string, output io.Writer) error
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
	Merge        bool
	Output       io.Writer
}

type RestoreOptions struct {
	Name         string
	Replace      bool
	NoStart      bool
	DryRun       bool
	SkipChecksum bool
	Output       io.Writer
}

type BackupManifest struct {
	Version        int                       `json:"version"`
	CreatedAt      string                    `json:"created_at"`
	Tool           version.VersionInfo       `json:"tool,omitempty"`
	SourcePlatform string                    `json:"source_platform,omitempty"`
	Containers     []BackupContainerManifest `json:"containers,omitempty"`

	ContainerName string              `json:"container_name,omitempty"`
	SourceName    string              `json:"source_name,omitempty"`
	Image         string              `json:"image,omitempty"`
	ImageArchive  string              `json:"image_archive,omitempty"`
	InspectFile   string              `json:"inspect_file,omitempty"`
	ComposeFile   string              `json:"compose_file,omitempty"`
	Networks      []BackupResourceRef `json:"networks,omitempty"`
	Volumes       []BackupResourceRef `json:"volumes,omitempty"`
}

type BackupContainerManifest struct {
	ContainerName string              `json:"container_name"`
	SourceName    string              `json:"source_name"`
	Path          string              `json:"path,omitempty"`
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

type restoreDryRunPlan struct {
	ContainerName string
	SourceName    string
	EntryDir      string
	Image         string
	ImageArchive  string
	Networks      []BackupResourceRef
	Volumes       []BackupResourceRef
	Ports         []string
	Exists        bool
	Replace       bool
	NoStart       bool
	Conflicts     []string
}

func NewBackupCommand() *cobra.Command {
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
		Use:   "container <name-pattern...> [backup-dir]",
		Short: "批量备份容器 inspect、镜像、compose、volume 和 network 元数据",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, outputDir := splitBackupContainerArgs(args, opts.OutputDir)
			runOpts := opts
			runOpts.OutputDir = outputDir
			runOpts.Output = cmd.OutOrStdout()
			result, err := backupContainers(cmd.Context(), targets, runOpts)
			if err != nil {
				return fmt.Errorf("备份容器失败: %w", err)
			}
			for _, path := range result.Paths {
				if runOpts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "备份 dry-run 完成: %s\n", path)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "备份已创建: %s\n", path)
				}
			}
			return nil
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	cmd.Flags().BoolVar(&opts.IncludeImage, "include-image", true, "导出容器镜像 tar")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "只预览备份动作，不写入文件")
	cmd.Flags().BoolVar(&opts.Bundle, "bundle", false, "生成离线迁移包 tar.gz，并附带 README、restore 脚本和 checksums")
	cmd.Flags().StringVar(&opts.BundleOutput, "output", "", "离线迁移包输出路径，默认 <backup-dir>.tar.gz")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", "", "批量备份输出根目录；单容器也可继续使用位置参数指定目录")
	cmd.Flags().BoolVar(&opts.Merge, "merge", false, "将多个容器合并为一个批量备份包，可整体 restore")
	return cmd
}

func NewRestoreCommand() *cobra.Command {
	opts := RestoreOptions{}
	cmd := &cobra.Command{
		Use:   "restore <backup-dir-or-archive...>",
		Short: "从 backup container 生成的目录、批量目录或 tar.gz 离线包恢复镜像、网络、volume 和容器",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Output = cmd.OutOrStdout()
			if opts.Name != "" && len(args) > 1 {
				return fmt.Errorf("--name 只支持恢复单个备份")
			}
			for _, arg := range args {
				if err := restoreBackup(cmd.Context(), arg, opts); err != nil {
					return fmt.Errorf("恢复失败: %w", err)
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "恢复 dry-run 完成: %s\n", arg)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "恢复完成: %s\n", arg)
				}
			}
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

type BackupContainersResult struct {
	Paths []string
}

func backupContainers(ctx context.Context, patterns []string, opts BackupOptions) (BackupContainersResult, error) {
	if len(patterns) == 0 {
		return BackupContainersResult{}, fmt.Errorf("必须提供至少一个容器名称或通配符")
	}
	targets, err := resolveBackupContainerTargets(ctx, patterns)
	if err != nil {
		return BackupContainersResult{}, err
	}
	if len(targets) == 0 {
		return BackupContainersResult{}, fmt.Errorf("未匹配任何容器")
	}
	if len(targets) == 1 && !opts.Merge {
		singleOpts := opts
		outputDir, err := backupContainer(ctx, targets[0], singleOpts)
		if err != nil {
			return BackupContainersResult{}, err
		}
		return BackupContainersResult{Paths: []string{outputDir}}, nil
	}
	if opts.BundleOutput != "" && !opts.Merge {
		return BackupContainersResult{}, fmt.Errorf("多个独立备份不能使用单个 --output；请使用 --output-dir 或添加 --merge")
	}
	if opts.Merge {
		return backupContainersMerged(ctx, targets, opts)
	}
	return backupContainersSeparate(ctx, targets, opts)
}

func backupContainersSeparate(ctx context.Context, targets []string, opts BackupOptions) (BackupContainersResult, error) {
	root := opts.OutputDir
	if root == "" {
		root = defaultBackupBatchDir(time.Now())
	}
	var result BackupContainersResult
	for _, target := range targets {
		childOpts := opts
		childOpts.OutputDir = filepath.Join(root, safeBackupName(target))
		childOpts.BundleOutput = ""
		outputDir, err := backupContainer(ctx, target, childOpts)
		if err != nil {
			return result, fmt.Errorf("backup %s: %w", target, err)
		}
		result.Paths = append(result.Paths, outputDir)
	}
	return result, nil
}

func backupContainersMerged(ctx context.Context, targets []string, opts BackupOptions) (BackupContainersResult, error) {
	root := opts.OutputDir
	if root == "" {
		root = defaultBackupBatchDir(time.Now())
	}
	manifest := BackupManifest{
		Version:        1,
		CreatedAt:      time.Now().Format(time.RFC3339),
		Tool:           version.CurrentInfo(),
		SourcePlatform: currentSourcePlatform(),
	}
	for _, target := range targets {
		childRel := filepath.ToSlash(filepath.Join("containers", safeBackupName(target)))
		childOpts := opts
		childOpts.OutputDir = filepath.Join(root, filepath.FromSlash(childRel))
		childOpts.Bundle = false
		childOpts.BundleOutput = ""
		outputDir, err := backupContainer(ctx, target, childOpts)
		if err != nil {
			return BackupContainersResult{}, fmt.Errorf("backup %s: %w", target, err)
		}
		entry := BackupContainerManifest{
			ContainerName: target,
			SourceName:    target,
			Path:          childRel,
		}
		if !opts.DryRun {
			childManifest, err := readBackupManifest(outputDir)
			if err != nil {
				return BackupContainersResult{}, err
			}
			if len(childManifest.Containers) == 0 {
				return BackupContainersResult{}, fmt.Errorf("backup %s manifest does not contain containers", target)
			}
			entry = childManifest.Containers[0]
			entry.Path = childRel
		}
		manifest.Containers = append(manifest.Containers, entry)
	}
	if !opts.DryRun {
		if err := writeJSONFile(filepath.Join(root, backupManifestName), manifest); err != nil {
			return BackupContainersResult{}, fmt.Errorf("write manifest: %w", err)
		}
		if opts.Bundle {
			if err := writeBackupBundleArtifacts(root, manifest); err != nil {
				return BackupContainersResult{}, err
			}
			archivePath := opts.BundleOutput
			if archivePath == "" {
				archivePath = root + ".tar.gz"
			}
			if err := createBackupArchive(root, archivePath); err != nil {
				return BackupContainersResult{}, err
			}
			log.Printf("Backup batch bundle: %s", archivePath)
		}
	}
	log.Printf("Backup batch summary: containers=%d output=%s merge=true", len(targets), root)
	return BackupContainersResult{Paths: []string{root}}, nil
}

func resolveBackupContainerTargets(ctx context.Context, patterns []string) ([]string, error) {
	svc, err := newBackupDockerService()
	if err != nil {
		return nil, err
	}
	containers, err := svc.ListContainers(ctx, true)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var targets []string
	for _, pattern := range patterns {
		matches := matchBackupContainerTargets(containers, pattern)
		if len(matches) == 0 {
			return nil, fmt.Errorf("容器 %q 未匹配任何容器", pattern)
		}
		for _, match := range matches {
			if !seen[match] {
				seen[match] = true
				targets = append(targets, match)
			}
		}
	}
	return targets, nil
}

func matchBackupContainerTargets(containers []container.Summary, pattern string) []string {
	var matches []string
	for _, c := range containers {
		if !backupContainerMatchesPattern(c, pattern) {
			continue
		}
		name := backupContainerTargetName(c)
		if name != "" {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	return matches
}

func backupContainerMatchesPattern(c container.Summary, pattern string) bool {
	return resourcefilter.Match(resourcefilter.ContainerCandidates(c), []string{pattern}, resourcefilter.ContainerKeys...)
}

func firstContainerName(names []string) string {
	return resourcefilter.FirstContainerName(names)
}

func backupContainerTargetName(c container.Summary) string {
	name := firstContainerName(c.Names)
	if name != "" {
		return name
	}
	return c.ID
}

func splitBackupContainerArgs(args []string, outputDir string) ([]string, string) {
	if outputDir != "" || len(args) < 2 {
		return args, outputDir
	}
	last := args[len(args)-1]
	if looksLikeBackupPathArg(last) {
		return args[:len(args)-1], last
	}
	return args, outputDir
}

func looksLikeBackupPathArg(value string) bool {
	return filepath.IsAbs(value) ||
		strings.HasPrefix(value, ".") ||
		strings.ContainsAny(value, `/\`)
}

func backupContainer(ctx context.Context, name string, opts BackupOptions) (string, error) {
	if opts.Output == nil {
		opts.Output = io.Discard
	}
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

	createdAt := time.Now().Format(time.RFC3339)
	containerManifest := BackupContainerManifest{
		ContainerName: containerName,
		SourceName:    name,
		InspectFile:   backupInspectName,
		ComposeFile:   backupComposeName,
	}
	if inspect.Config != nil {
		containerManifest.Image = inspect.Config.Image
	}

	if opts.DryRun {
		if opts.IncludeImage && containerManifest.Image != "" {
			imageFile := filepath.Join("images", safeBackupName(containerManifest.Image)+".tar")
			containerManifest.ImageArchive = filepath.ToSlash(imageFile)
		}
		networks, err := inspectBackupNetworkRefs(ctx, svc, inspect)
		if err != nil {
			return "", err
		}
		containerManifest.Networks = networks
		volumes, err := inspectBackupVolumeRefs(ctx, svc, inspect)
		if err != nil {
			return "", err
		}
		containerManifest.Volumes = volumes
		manifest := BackupManifest{
			Version:        1,
			CreatedAt:      createdAt,
			Tool:           version.CurrentInfo(),
			SourcePlatform: currentSourcePlatform(),
			Containers:     []BackupContainerManifest{containerManifest},
		}
		printBackupDryRunPlan(opts.Output, outputDir, manifest, opts)
		log.Printf("Dry run backup container: name=%s output=%s includeImage=%v networks=%d volumes=%d bundle=%v", name, outputDir, opts.IncludeImage, len(networks), len(volumes), opts.Bundle)
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

	if opts.IncludeImage && containerManifest.Image != "" {
		imageDir := filepath.Join(outputDir, "images")
		if err := os.MkdirAll(imageDir, 0755); err != nil {
			return "", err
		}
		imageFile := filepath.Join("images", safeBackupName(containerManifest.Image)+".tar")
		if err := svc.SaveImage(ctx, []string{containerManifest.Image}, filepath.Join(outputDir, imageFile)); err != nil {
			return "", fmt.Errorf("save image %s: %w", containerManifest.Image, err)
		}
		containerManifest.ImageArchive = filepath.ToSlash(imageFile)
	}

	networks, err := backupNetworks(ctx, svc, outputDir, inspect)
	if err != nil {
		return "", err
	}
	containerManifest.Networks = networks

	volumes, err := backupVolumes(ctx, svc, outputDir, inspect)
	if err != nil {
		return "", err
	}
	containerManifest.Volumes = volumes

	manifest := BackupManifest{
		Version:        1,
		CreatedAt:      createdAt,
		Tool:           version.CurrentInfo(),
		SourcePlatform: currentSourcePlatform(),
		Containers:     []BackupContainerManifest{containerManifest},
	}
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
	log.Printf("Backup summary: container=%s output=%s image=%v networks=%d volumes=%d", containerName, outputDir, containerManifest.ImageArchive != "", len(containerManifest.Networks), len(containerManifest.Volumes))
	return outputDir, nil
}

func restoreBackup(ctx context.Context, backupDir string, opts RestoreOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}
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

	return restoreBackupDir(ctx, backupDir, opts)
}

func restoreBackupDir(ctx context.Context, backupDir string, opts RestoreOptions) error {
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	svc, err := newBackupDockerService()
	if err != nil {
		return err
	}
	manifest, err := readBackupManifest(backupDir)
	if err != nil {
		return err
	}
	if len(manifest.Containers) == 0 {
		return fmt.Errorf("manifest does not contain any containers")
	}
	if opts.Name != "" && len(manifest.Containers) != 1 {
		return fmt.Errorf("--name 只支持恢复单个备份")
	}
	if opts.DryRun {
		fmt.Fprintf(opts.Output, "恢复 dry-run 计划: %s\n", backupDir)
		fmt.Fprintf(opts.Output, "  容器数量: %d\n", len(manifest.Containers))
		fmt.Fprintf(opts.Output, "  checksum: %s\n", checksumPlanText(opts.SkipChecksum, filepath.Join(backupDir, backupChecksumName)))
	}
	for _, entry := range manifest.Containers {
		if err := restoreBackupContainerEntry(ctx, svc, backupDir, entry, opts); err != nil {
			return err
		}
	}
	log.Printf("Restore summary: containers=%d source=%s", len(manifest.Containers), backupDir)
	return nil
}

func restoreBackupContainerEntry(ctx context.Context, svc backupDockerService, backupDir string, entry BackupContainerManifest, opts RestoreOptions) error {
	entryDir := backupDir
	if entry.Path != "" {
		var err error
		entryDir, err = backupFilePath(backupDir, entry.Path)
		if err != nil {
			return err
		}
	}
	inspect, err := readContainerInspect(entryDir, entry)
	if err != nil {
		return err
	}
	targetName := opts.Name
	if targetName == "" {
		targetName = entry.ContainerName
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
		if !opts.DryRun {
			return fmt.Errorf("container %s already exists; use --replace to overwrite", targetName)
		}
	}

	if opts.DryRun {
		plan, err := buildRestoreDryRunPlan(entryDir, entry, inspect, targetName, exists, opts)
		if err != nil {
			return err
		}
		printRestoreDryRunContainerPlan(opts.Output, plan)
		log.Printf("Dry run restore: backup=%s container=%s replace=%v noStart=%v image=%v networks=%d volumes=%d", entryDir, targetName, opts.Replace, opts.NoStart, plan.ImageArchive != "", len(plan.Networks), len(plan.Volumes))
		return nil
	}

	if entry.ImageArchive != "" {
		imagePath, err := backupFilePath(entryDir, entry.ImageArchive)
		if err != nil {
			return err
		}
		if err := svc.LoadImage(ctx, imagePath, opts.Output); err != nil {
			return fmt.Errorf("load image archive %s: %w", imagePath, err)
		}
	}

	for _, ref := range entry.Networks {
		netMeta, err := readNetworkInspect(entryDir, ref)
		if err != nil {
			return err
		}
		if err := svc.CreateNetwork(ctx, netMeta); err != nil {
			return fmt.Errorf("restore network %s: %w", ref.Name, err)
		}
	}

	for _, ref := range entry.Volumes {
		volMeta, err := readVolumeInspect(entryDir, ref)
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
	log.Printf("Restore container summary: container=%s id=%s started=%v", targetName, id, !opts.NoStart)
	return nil
}

func buildRestoreDryRunPlan(entryDir string, entry BackupContainerManifest, inspect container.InspectResponse, targetName string, exists bool, opts RestoreOptions) (restoreDryRunPlan, error) {
	plan := restoreDryRunPlan{
		ContainerName: targetName,
		SourceName:    entry.ContainerName,
		EntryDir:      entryDir,
		Image:         entry.Image,
		Networks:      append([]BackupResourceRef(nil), entry.Networks...),
		Volumes:       append([]BackupResourceRef(nil), entry.Volumes...),
		Ports:         restorePortBindings(inspect),
		Exists:        exists,
		Replace:       opts.Replace,
		NoStart:       opts.NoStart,
	}
	if plan.Image == "" && inspect.Config != nil {
		plan.Image = inspect.Config.Image
	}
	if entry.ImageArchive != "" {
		imagePath, err := backupFilePath(entryDir, entry.ImageArchive)
		if err != nil {
			return plan, err
		}
		if _, err := os.Stat(imagePath); err != nil {
			return plan, fmt.Errorf("image archive %s: %w", entry.ImageArchive, err)
		}
		plan.ImageArchive = entry.ImageArchive
	}
	for _, ref := range entry.Networks {
		if _, err := readNetworkInspect(entryDir, ref); err != nil {
			return plan, err
		}
	}
	for _, ref := range entry.Volumes {
		if _, err := readVolumeInspect(entryDir, ref); err != nil {
			return plan, err
		}
	}
	if exists && !opts.Replace {
		plan.Conflicts = append(plan.Conflicts, fmt.Sprintf("目标容器 %s 已存在；实际恢复需要 --replace 或更换 --name", targetName))
	}
	if len(plan.Ports) > 0 {
		plan.Conflicts = append(plan.Conflicts, "请确认目标宿主机端口未被占用: "+strings.Join(plan.Ports, ", "))
	}
	return plan, nil
}

func printRestoreDryRunContainerPlan(w io.Writer, plan restoreDryRunPlan) {
	fmt.Fprintf(w, "  - 容器: %s", plan.ContainerName)
	if plan.SourceName != "" && plan.SourceName != plan.ContainerName {
		fmt.Fprintf(w, " source=%s", plan.SourceName)
	}
	fmt.Fprintf(w, " backup=%s\n", plan.EntryDir)
	if plan.Image != "" {
		fmt.Fprintf(w, "    镜像: %s\n", plan.Image)
	}
	if plan.ImageArchive != "" {
		fmt.Fprintf(w, "    将导入镜像归档: %s\n", plan.ImageArchive)
	}
	if len(plan.Networks) > 0 {
		fmt.Fprintf(w, "    将创建/复用 network: %s\n", resourceRefNames(plan.Networks))
	}
	if len(plan.Volumes) > 0 {
		fmt.Fprintf(w, "    将创建/复用 volume: %s\n", resourceRefNames(plan.Volumes))
	}
	if len(plan.Ports) > 0 {
		fmt.Fprintf(w, "    端口绑定: %s\n", strings.Join(plan.Ports, ", "))
	}
	switch {
	case plan.Exists && plan.Replace:
		fmt.Fprintln(w, "    目标状态: 已存在，实际恢复会先删除后重建")
	case plan.Exists:
		fmt.Fprintln(w, "    目标状态: 已存在，存在覆盖冲突")
	default:
		fmt.Fprintln(w, "    目标状态: 不存在，将创建")
	}
	if plan.NoStart {
		fmt.Fprintln(w, "    启动策略: 只创建，不启动")
	} else {
		fmt.Fprintln(w, "    启动策略: 创建后启动")
	}
	if len(plan.Conflicts) > 0 {
		fmt.Fprintln(w, "    预检提示:")
		for _, conflict := range plan.Conflicts {
			fmt.Fprintf(w, "      - %s\n", conflict)
		}
	}
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

func inspectBackupNetworkRefs(ctx context.Context, svc backupDockerService, inspect container.InspectResponse) ([]BackupResourceRef, error) {
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

	refs := make([]BackupResourceRef, 0, len(names))
	for _, name := range names {
		if _, err := svc.InspectNetwork(ctx, name); err != nil {
			return nil, fmt.Errorf("inspect network %s: %w", name, err)
		}
		rel := filepath.Join("networks", safeBackupName(name)+".json")
		refs = append(refs, BackupResourceRef{Name: name, File: filepath.ToSlash(rel)})
	}
	return refs, nil
}

func inspectBackupVolumeRefs(ctx context.Context, svc backupDockerService, inspect container.InspectResponse) ([]BackupResourceRef, error) {
	names := namedVolumes(inspect)
	refs := make([]BackupResourceRef, 0, len(names))
	for _, name := range names {
		if _, err := svc.InspectVolume(ctx, name); err != nil {
			return nil, fmt.Errorf("inspect volume %s: %w", name, err)
		}
		rel := filepath.Join("volumes", safeBackupName(name)+".json")
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

func printBackupDryRunPlan(w io.Writer, outputDir string, manifest BackupManifest, opts BackupOptions) {
	fmt.Fprintf(w, "备份 dry-run 计划: %s\n", outputDir)
	fmt.Fprintf(w, "  容器数量: %d\n", len(manifest.Containers))
	fmt.Fprintf(w, "  源平台: %s\n", manifest.SourcePlatform)
	fmt.Fprintf(w, "  将生成文件: %s, %s, %s\n", backupManifestName, backupInspectName, backupComposeName)
	if opts.IncludeImage {
		fmt.Fprintln(w, "  镜像归档: 启用")
	} else {
		fmt.Fprintln(w, "  镜像归档: 跳过 (--include-image=false)")
	}
	if opts.Bundle {
		archivePath := opts.BundleOutput
		if archivePath == "" {
			archivePath = outputDir + ".tar.gz"
		}
		fmt.Fprintf(w, "  离线包: %s\n", archivePath)
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
	}
	fmt.Fprintln(w, "  校验: dry-run 已确认 inspect 可读、compose 可生成、network/volume 元数据可读取；不会写入文件或导出镜像。")
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

func restorePortBindings(inspect container.InspectResponse) []string {
	if inspect.HostConfig == nil || len(inspect.HostConfig.PortBindings) == 0 {
		return nil
	}
	var ports []string
	for port, bindings := range inspect.HostConfig.PortBindings {
		if len(bindings) == 0 {
			ports = append(ports, string(port))
			continue
		}
		for _, binding := range bindings {
			host := binding.HostIP
			if host == "" {
				host = "0.0.0.0"
			}
			if binding.HostPort == "" {
				ports = append(ports, fmt.Sprintf("%s->%s", host, port))
				continue
			}
			ports = append(ports, fmt.Sprintf("%s:%s->%s", host, binding.HostPort, port))
		}
	}
	sort.Strings(ports)
	return ports
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
	if isBackupRootDir(tempDir) {
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
		if isBackupRootDir(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("archive does not contain %s", backupManifestName)
}

func isBackupRootDir(dir string) bool {
	return isSingleBackupDir(dir)
}

func isSingleBackupDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, backupManifestName))
	return err == nil
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
	manifest = normalizeBackupManifest(manifest)
	return manifest, nil
}

func normalizeBackupManifest(manifest BackupManifest) BackupManifest {
	if len(manifest.Containers) == 0 && (manifest.ContainerName != "" || manifest.InspectFile != "" || manifest.ComposeFile != "") {
		manifest.Containers = []BackupContainerManifest{{
			ContainerName: manifest.ContainerName,
			SourceName:    manifest.SourceName,
			Image:         manifest.Image,
			ImageArchive:  manifest.ImageArchive,
			InspectFile:   manifest.InspectFile,
			ComposeFile:   manifest.ComposeFile,
			Networks:      manifest.Networks,
			Volumes:       manifest.Volumes,
		}}
	}
	return manifest
}

func readContainerInspect(backupDir string, manifest BackupContainerManifest) (container.InspectResponse, error) {
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
	return resourcefilter.NormalizeContainerName(name)
}

func defaultBackupDir(now time.Time, containerName string) string {
	return filepath.Join(backupRoot, safeBackupName(containerName)+"-"+now.Format("20060102-150405"))
}

func defaultBackupBatchDir(now time.Time) string {
	return filepath.Join(backupRoot, "batch-"+now.Format("20060102-150405"))
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

func (s *dockerBackupService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	return s.cli.ContainerList(ctx, container.ListOptions{All: all})
}

func (s *dockerBackupService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, name)
}

func (s *dockerBackupService) SaveImage(ctx context.Context, refs []string, outputFile string) error {
	if ctx == nil {
		ctx = context.Background()
	}
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

	return backupCopyWithContext(ctx, file, reader)
}

func (s *dockerBackupService) LoadImage(ctx context.Context, inputFile string, output io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if output == nil {
		output = io.Discard
	}
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
	return backupCopyWithContext(ctx, output, resp.Body)
}

func backupCopyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
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
