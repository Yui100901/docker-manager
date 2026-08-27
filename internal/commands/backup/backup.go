package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"docker-manager/internal/audit"
	"docker-manager/internal/runcontrol"
	"docker-manager/internal/version"
)

func backupContainers(ctx context.Context, patterns []string, opts BackupOptions) (BackupContainersResult, error) {
	if err := checkBackupContext(ctx); err != nil {
		return BackupContainersResult{}, err
	}
	if len(patterns) == 0 {
		return BackupContainersResult{}, fmt.Errorf("必须提供至少一个容器名称或通配符")
	}
	if (opts.Encrypt || opts.SplitSize != "") && !opts.Bundle {
		return BackupContainersResult{}, fmt.Errorf("--encrypt 和 --split-size 仅在 --bundle 时可用")
	}
	if opts.SigningKey != "" && !opts.Bundle {
		return BackupContainersResult{}, fmt.Errorf("--signing-key 仅在 --bundle 时可用")
	}
	if opts.Bundle {
		if _, err := archiveOptionsFromBackup(opts); err != nil {
			return BackupContainersResult{}, err
		}
	}
	targets, err := resolveBackupContainerTargets(ctx, patterns)
	if err != nil {
		return BackupContainersResult{}, err
	}
	if len(targets) == 0 {
		return BackupContainersResult{}, fmt.Errorf("未匹配任何容器")
	}
	if err := runcontrol.CheckItems(ctx, "container", len(targets)); err != nil {
		return BackupContainersResult{}, err
	}
	opts, err = resolveBackupOutputOptions(targets, opts, time.Now())
	if err != nil {
		return BackupContainersResult{}, err
	}
	if err := authorizeBackupMutation(ctx, targets, opts); err != nil {
		return BackupContainersResult{}, fmt.Errorf("审计授权失败，未执行备份: %w", err)
	}
	if len(targets) == 1 && !opts.Merge {
		singleOpts := opts
		outputDir, err := backupContainer(ctx, targets[0], singleOpts)
		if err != nil {
			return BackupContainersResult{}, err
		}
		return BackupContainersResult{Paths: []string{outputDir}}, nil
	}
	if opts.Merge {
		return backupContainersMerged(ctx, targets, opts)
	}
	return backupContainersSeparate(ctx, targets, opts)
}

// authorizeBackupMutation runs after target resolution but before any backup
// directory or image archive is created. Backup is an explicit export command,
// so it does not require a second interactive confirmation; the audit policy
// still gets a chance to fail closed when its sink is unavailable.
func authorizeBackupMutation(ctx context.Context, targets []string, opts BackupOptions) error {
	session := audit.FromContext(ctx)
	if session == nil || opts.DryRun || len(targets) == 0 {
		return nil
	}
	candidates, err := backupMutationCandidates(targets, opts)
	if err != nil {
		return err
	}
	_, err = session.AuthorizeMutation(ctx, audit.MutationRequest{
		Scope:        audit.MutationFilesystem,
		Confirmation: audit.Confirmation{Provided: true, Mechanism: "backup"},
		Candidates:   candidates,
	})
	return err
}

func resolveBackupOutputOptions(targets []string, opts BackupOptions, now time.Time) (BackupOptions, error) {
	if len(targets) > 1 && opts.BundleOutput != "" && !opts.Merge {
		return BackupOptions{}, fmt.Errorf("多个独立备份不能使用单个 --bundle-output；请使用 --output-dir 或添加 --merge")
	}
	if opts.OutputDir == "" {
		if len(targets) == 1 && !opts.Merge {
			opts.OutputDir = defaultBackupDir(now, safeBackupName(targets[0]))
		} else {
			opts.OutputDir = defaultBackupBatchDir(now)
		}
	}
	return opts, nil
}

func backupMutationCandidates(targets []string, opts BackupOptions) ([]audit.CandidateInput, error) {
	outputDirs := make([]string, 0, max(1, len(targets)))
	switch {
	case len(targets) == 1 && !opts.Merge:
		outputDirs = append(outputDirs, opts.OutputDir)
	case opts.Merge:
		outputDirs = append(outputDirs, opts.OutputDir)
	default:
		for _, target := range targets {
			outputDirs = append(outputDirs, filepath.Join(opts.OutputDir, safeBackupName(target)))
		}
	}

	candidates := make([]audit.CandidateInput, 0, len(outputDirs)*3)
	for _, outputDir := range outputDirs {
		candidates = append(candidates, audit.CandidateInput{
			Kind:       "backup-directory",
			Action:     "write",
			Identifier: outputDir,
			Display:    outputDir,
		})
		if opts.IncludeImage {
			imageDir := filepath.Join(outputDir, "images")
			candidates = append(candidates, audit.CandidateInput{
				Kind:       "image-archive",
				Action:     "save",
				Identifier: imageDir,
				Display:    imageDir,
			})
		}
		if !opts.Bundle {
			continue
		}
		bundleOpts := opts
		if len(outputDirs) > 1 {
			bundleOpts.BundleOutput = ""
		}
		archiveOpts, archivePath, err := resolveBackupBundleOptions(outputDir, bundleOpts)
		if err != nil {
			return nil, err
		}
		publishedPath := backupPublishedArchivePath(archivePath, archiveOpts)
		candidates = append(candidates, audit.CandidateInput{
			Kind:       "backup-archive",
			Action:     "write",
			Identifier: publishedPath,
			Display:    publishedPath,
		})
	}
	return candidates, nil
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

func backupContainersMerged(ctx context.Context, targets []string, opts BackupOptions) (result BackupContainersResult, resultErr error) {
	if err := checkBackupContext(ctx); err != nil {
		return BackupContainersResult{}, err
	}
	root := opts.OutputDir
	if root == "" {
		root = defaultBackupBatchDir(time.Now())
	}
	var archiveOpts backupArchiveOptions
	var archivePath string
	if opts.Bundle {
		var err error
		archiveOpts, archivePath, err = resolveBackupBundleOptions(root, opts)
		if err != nil {
			return BackupContainersResult{}, err
		}
	}
	ephemeralPlaintext := opts.Bundle && opts.Encrypt && !opts.DryRun
	var plaintextDir *privateBackupDirectory
	if !opts.DryRun {
		if ephemeralPlaintext {
			var createErr error
			plaintextDir, createErr = createPrivateBackupDirectory(root)
			if createErr != nil {
				return BackupContainersResult{}, createErr
			}
		} else if err := ensurePrivateBackupDirectory(root); err != nil {
			return BackupContainersResult{}, err
		}
		defer func() {
			if !ephemeralPlaintext {
				return
			}
			if err := plaintextDir.removeAll(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove encrypted backup plaintext tree %s: %w", root, err))
				result = BackupContainersResult{}
			}
		}()
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
		childOpts.Encrypt = false
		childOpts.PassphraseFile = ""
		childOpts.SplitSize = ""
		childOpts.SigningKey = ""
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
			if opts.SigningKey != "" {
				if err := signBackupChecksumsWithContext(ctx, root, opts.SigningKey); err != nil {
					return BackupContainersResult{}, err
				}
			}
			if err := createBackupArchiveWithOptions(ctx, root, archivePath, archiveOpts); err != nil {
				return BackupContainersResult{}, err
			}
			log.Printf("Backup batch bundle: %s", archivePath)
		} else if err := writeChecksumsWithContext(ctx, root); err != nil {
			return BackupContainersResult{}, fmt.Errorf("write checksums: %w", err)
		}
	}
	log.Printf("Backup batch summary: containers=%d output=%s merge=true", len(targets), root)
	resultPath := root
	if ephemeralPlaintext {
		resultPath = backupPublishedArchivePath(archivePath, archiveOpts)
	}
	return BackupContainersResult{Paths: []string{resultPath}}, nil
}

func backupContainer(ctx context.Context, name string, opts BackupOptions) (resultPath string, resultErr error) {
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
	var archiveOpts backupArchiveOptions
	var archivePath string
	if opts.Bundle {
		archiveOpts, archivePath, err = resolveBackupBundleOptions(outputDir, opts)
		if err != nil {
			return "", err
		}
	}
	ephemeralPlaintext := opts.Bundle && opts.Encrypt
	var plaintextDir *privateBackupDirectory

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

	if ephemeralPlaintext {
		plaintextDir, err = createPrivateBackupDirectory(outputDir)
		if err != nil {
			return "", err
		}
	} else if err := ensurePrivateBackupDirectory(outputDir); err != nil {
		return "", err
	}
	defer func() {
		if !ephemeralPlaintext {
			return
		}
		if err := plaintextDir.removeAll(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove encrypted backup plaintext tree %s: %w", outputDir, err))
			resultPath = ""
		}
	}()
	if err := writeJSONFile(filepath.Join(outputDir, backupInspectName), inspect); err != nil {
		return "", fmt.Errorf("write inspect: %w", err)
	}
	if err := writeComposeFile(filepath.Join(outputDir, backupComposeName), inspect); err != nil {
		return "", fmt.Errorf("write compose: %w", err)
	}

	if opts.IncludeImage && containerManifest.Image != "" {
		imageDir := filepath.Join(outputDir, "images")
		if err := ensurePrivateBackupDirectory(imageDir); err != nil {
			return "", err
		}
		imageFile := filepath.Join("images", safeBackupName(containerManifest.Image)+".tar")
		if err := checkBackupContext(ctx); err != nil {
			return "", err
		}
		if err := svc.SaveImage(ctx, []string{containerManifest.Image}, filepath.Join(outputDir, imageFile)); err != nil {
			return "", fmt.Errorf("save image %s: %w", containerManifest.Image, err)
		}
		if err := os.Chmod(filepath.Join(outputDir, imageFile), 0600); err != nil {
			return "", fmt.Errorf("secure image archive %s: %w", containerManifest.Image, err)
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
		if opts.SigningKey != "" {
			if err := signBackupChecksumsWithContext(ctx, outputDir, opts.SigningKey); err != nil {
				return "", err
			}
		}
		if err := createBackupArchiveWithOptions(ctx, outputDir, archivePath, archiveOpts); err != nil {
			return "", err
		}
		log.Printf("Backup bundle: %s", archivePath)
	} else if err := writeChecksumsWithContext(ctx, outputDir); err != nil {
		return "", fmt.Errorf("write checksums: %w", err)
	}
	log.Printf("Backup summary: container=%s output=%s image=%v networks=%d volumes=%d", containerName, outputDir, containerManifest.ImageArchive != "", len(containerManifest.Networks), len(containerManifest.Volumes))
	if ephemeralPlaintext {
		return backupPublishedArchivePath(archivePath, archiveOpts), nil
	}
	return outputDir, nil
}

func backupPublishedArchivePath(archivePath string, opts backupArchiveOptions) string {
	if opts.SplitSize > 0 {
		return splitPartPath(archivePath, 1)
	}
	return archivePath
}

func resolveBackupBundleOptions(root string, opts BackupOptions) (backupArchiveOptions, string, error) {
	archiveOpts, err := archiveOptionsFromBackup(opts)
	if err != nil {
		return backupArchiveOptions{}, "", err
	}
	if opts.SigningKey != "" {
		if err := requireBackupSensitiveFileOutsideRoot(root, opts.SigningKey, "signing key"); err != nil {
			return backupArchiveOptions{}, "", err
		}
	}
	if archiveOpts.Encrypt {
		if err := requireBackupSensitiveFileOutsideRoot(root, archiveOpts.PassphraseFile, "passphrase file"); err != nil {
			return backupArchiveOptions{}, "", err
		}
	}
	archivePath := opts.BundleOutput
	if archivePath == "" {
		archivePath = root + ".tar.gz"
	}
	archivePath = backupArchiveOutputPath(archivePath, archiveOpts)
	if err := requireBackupPathOutsideRoot(root, archivePath, "bundle output"); err != nil {
		return backupArchiveOptions{}, "", err
	}
	return archiveOpts, archivePath, nil
}
