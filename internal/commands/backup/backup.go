package backup

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"docker-manager/internal/version"
)

func backupContainers(ctx context.Context, patterns []string, opts BackupOptions) (BackupContainersResult, error) {
	if err := checkBackupContext(ctx); err != nil {
		return BackupContainersResult{}, err
	}
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
		if err := checkBackupContext(ctx); err != nil {
			return result, err
		}
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
	if err := checkBackupContext(ctx); err != nil {
		return BackupContainersResult{}, err
	}
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
		if err := checkBackupContext(ctx); err != nil {
			return BackupContainersResult{}, err
		}
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
		if err := checkBackupContext(ctx); err != nil {
			return BackupContainersResult{}, err
		}
		if err := writeJSONFile(filepath.Join(root, backupManifestName), manifest); err != nil {
			return BackupContainersResult{}, fmt.Errorf("write manifest: %w", err)
		}
		if opts.Bundle {
			if err := writeBackupBundleArtifactsWithContext(ctx, root, manifest); err != nil {
				return BackupContainersResult{}, err
			}
			archivePath := opts.BundleOutput
			if archivePath == "" {
				archivePath = root + ".tar.gz"
			}
			if err := createBackupArchiveWithContext(ctx, root, archivePath); err != nil {
				return BackupContainersResult{}, err
			}
			log.Printf("Backup batch bundle: %s", archivePath)
		}
	}
	log.Printf("Backup batch summary: containers=%d output=%s merge=true", len(targets), root)
	return BackupContainersResult{Paths: []string{root}}, nil
}

func backupContainer(ctx context.Context, name string, opts BackupOptions) (string, error) {
	if err := checkBackupContext(ctx); err != nil {
		return "", err
	}
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
	if err := checkBackupContext(ctx); err != nil {
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
	containerManifest.Mounts = backupMountRefs(inspect)
	containerManifest.Devices = backupDeviceRefs(inspect)

	if opts.DryRun {
		if err := checkBackupContext(ctx); err != nil {
			return "", err
		}
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
		if err := checkBackupContext(ctx); err != nil {
			return "", err
		}
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
		if err := checkBackupContext(ctx); err != nil {
			return "", err
		}
		if err := writeBackupBundleArtifactsWithContext(ctx, outputDir, manifest); err != nil {
			return "", err
		}
		archivePath := opts.BundleOutput
		if archivePath == "" {
			archivePath = outputDir + ".tar.gz"
		}
		if err := createBackupArchiveWithContext(ctx, outputDir, archivePath); err != nil {
			return "", err
		}
		log.Printf("Backup bundle: %s", archivePath)
	}
	log.Printf("Backup summary: container=%s output=%s image=%v networks=%d volumes=%d", containerName, outputDir, containerManifest.ImageArchive != "", len(containerManifest.Networks), len(containerManifest.Volumes))
	return outputDir, nil
}
