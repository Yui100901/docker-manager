package backup

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"docker-manager/internal/resourcefilter"
	"docker-manager/internal/version"

	"github.com/docker/docker/api/types/container"
)

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
		return BackupContainersResult{}, fmt.Errorf("多个独立备份不能使用单个 --bundle-output；请使用 --output-dir 或添加 --merge")
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
		log.Printf("Dry run backup: name=%s output=%s includeImage=%v networks=%d volumes=%d bundle=%v", name, outputDir, opts.IncludeImage, len(networks), len(volumes), opts.Bundle)
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
		fmt.Fprintln(w, "  镜像归档: 跳过 (--no-image)")
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
