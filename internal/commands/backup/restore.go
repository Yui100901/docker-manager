package backup

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"docker-manager/internal/runcontrol"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

func restoreBackup(ctx context.Context, backupDir string, opts RestoreOptions) error {
	ctx = backupContext(ctx)
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	if !opts.DryRun && !opts.Confirm {
		return fmt.Errorf("实际恢复需要显式 --confirm；未确认时请使用 dry-run 计划")
	}
	if opts.TrustedPublicKey != "" && opts.SkipChecksum {
		return fmt.Errorf("trusted public key verification requires checksum verification")
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	prepared, err := prepareRestoreBackup(ctx, backupDir, opts)
	if err != nil {
		return err
	}
	defer prepared.Close()
	if !opts.DryRun {
		if err := validatePreparedRestoreSet(ctx, []*preparedRestoreBackup{prepared}, opts); err != nil {
			return err
		}
	}
	return executePreparedRestore(ctx, prepared, opts)
}

func restoreBackupDir(ctx context.Context, backupDir string, opts RestoreOptions) error {
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	if !opts.DryRun && !opts.Confirm {
		return fmt.Errorf("actual restore requires explicit confirmation")
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	limits, err := resolveRestoreLimits(opts)
	if err != nil {
		return err
	}
	manifest, err := readBackupManifestWithLimit(backupDir, limits.jsonBytes)
	if err != nil {
		return err
	}
	if err := validateRestoreJSONBudgetWithLimit(backupDir, manifest, limits.jsonBytes); err != nil {
		return err
	}
	if len(manifest.Containers) == 0 {
		return fmt.Errorf("manifest does not contain any containers")
	}
	if opts.Name != "" && len(manifest.Containers) != 1 {
		return fmt.Errorf("--name 只支持恢复单个备份")
	}
	if err := runcontrol.CheckItems(ctx, "restore-container", len(manifest.Containers)); err != nil {
		return err
	}
	if err := validateRestoreManifestArtifacts(backupDir, manifest, opts); err != nil {
		return err
	}
	svc, err := newBackupDockerService()
	if err != nil {
		return err
	}
	prepared := &preparedRestoreBackup{source: backupDir, dir: backupDir, manifest: manifest, svc: svc}
	if !opts.DryRun {
		prepared.targets, prepared.existingTargets, err = preflightRestoreDockerTargets(ctx, svc, backupDir, manifest, opts)
		if err != nil {
			return err
		}
		if err := validatePreparedRestoreSet(ctx, []*preparedRestoreBackup{prepared}, opts); err != nil {
			return err
		}
	}
	return executePreparedRestore(ctx, prepared, opts)
}

func executePreparedRestore(ctx context.Context, prepared *preparedRestoreBackup, opts RestoreOptions) error {
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	if opts.DryRun && prepared.signatureStatus != "" {
		fmt.Fprintf(opts.Output, "恢复来源验证: %s\n", prepared.signatureStatus)
	}
	if opts.DryRun {
		fmt.Fprintf(opts.Output, "恢复 dry-run 计划: %s\n", prepared.source)
		fmt.Fprintf(opts.Output, "  容器数量: %d\n", len(prepared.manifest.Containers))
		fmt.Fprintf(opts.Output, "  checksum: %s\n", checksumPlanText(opts.SkipChecksum, filepath.Join(prepared.dir, backupChecksumName)))
	}
	for _, entry := range prepared.manifest.Containers {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		if err := restoreBackupContainerEntry(ctx, prepared.svc, prepared.dir, entry, prepared.existingTargets, opts); err != nil {
			return err
		}
	}
	log.Printf("Restore summary: containers=%d source=%s", len(prepared.manifest.Containers), prepared.source)
	return nil
}

func restoreBackupContainerEntry(ctx context.Context, svc backupDockerService, backupDir string, entry BackupContainerManifest, expectedTargets map[string]string, opts RestoreOptions) error {
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
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
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	targetName, err := restoreEntryTargetName(entry, inspect, opts)
	if err != nil {
		return err
	}
	unsafeConfig, err := unsafeRestoreEntryConfig(entryDir, entry, inspect)
	if err != nil {
		return err
	}
	if !opts.DryRun && len(unsafeConfig) > 0 && !opts.AllowUnsafeHostConfig {
		return fmt.Errorf("container %s contains unsafe restore configuration: %s; inspect the dry-run plan and use --allow-dangerous-config only for a trusted backup", targetName, strings.Join(unsafeConfig, "; "))
	}

	exists, err := svc.ContainerExists(ctx, targetName)
	if err != nil {
		return err
	}
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	if exists && !opts.Replace {
		if !opts.DryRun {
			return fmt.Errorf("container %s already exists; use --replace to overwrite", targetName)
		}
	}
	if expectedID, tracked := expectedTargets[targetName]; tracked {
		if expectedID == "" && exists {
			return fmt.Errorf("container %s appeared after restore preflight; refusing to replace an unreviewed target", targetName)
		}
		if expectedID != "" && !exists {
			return fmt.Errorf("container %s disappeared after restore preflight", targetName)
		}
		if exists {
			actual, err := svc.InspectContainer(ctx, targetName)
			if err != nil {
				return fmt.Errorf("reinspect container %s after restore preflight: %w", targetName, err)
			}
			if actualID := restoreContainerIdentity(actual, targetName); actualID != expectedID {
				return fmt.Errorf("container %s changed after restore preflight: expected %s, found %s", targetName, expectedID, actualID)
			}
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
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		imagePath, err := backupFilePath(entryDir, entry.ImageArchive)
		if err != nil {
			return err
		}
		if err := svc.LoadImage(ctx, imagePath, opts.Output); err != nil {
			return fmt.Errorf("load image archive %s: %w", imagePath, err)
		}
		imageRef := inspect.Config.Image
		imageExists, err := svc.ImageExists(ctx, imageRef)
		if err != nil {
			return fmt.Errorf("verify loaded image %s: %w", imageRef, err)
		}
		if !imageExists {
			return fmt.Errorf("image archive %s did not load required image %s", imagePath, imageRef)
		}
	}

	for _, ref := range entry.Networks {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		netMeta, err := readNetworkInspect(entryDir, ref)
		if err != nil {
			return err
		}
		if err := svc.CreateNetwork(ctx, netMeta); err != nil {
			return fmt.Errorf("restore network %s: %w", ref.Name, err)
		}
	}

	for _, ref := range entry.Volumes {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		volMeta, err := readVolumeInspect(entryDir, ref)
		if err != nil {
			return err
		}
		if err := svc.CreateVolume(ctx, volMeta); err != nil {
			return fmt.Errorf("restore volume %s: %w", ref.Name, err)
		}
	}

	id, err := createRestoredContainer(ctx, svc, inspect, targetName, exists, expectedTargets[targetName], opts)
	if err != nil {
		return err
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
	unsafeConfig, err := unsafeRestoreEntryConfig(entryDir, entry, inspect)
	if err != nil {
		return plan, err
	}
	plan.UnsafeConfig = unsafeConfig
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
	if len(plan.UnsafeConfig) > 0 {
		plan.Conflicts = append(plan.Conflicts, "检测到高风险恢复配置；实际恢复默认阻止，只有确认来源后才可同时使用 --confirm 和 --allow-dangerous-config")
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
		fmt.Fprintln(w, "    目标状态: 已存在，将临时改名保留；新容器创建并启动成功后才删除旧容器")
	case plan.Exists:
		fmt.Fprintln(w, "    目标状态: 已存在，存在覆盖冲突")
	default:
		fmt.Fprintln(w, "    目标状态: 不存在，将创建")
	}
	if len(plan.UnsafeConfig) > 0 {
		fmt.Fprintln(w, "    高风险恢复配置（默认阻止实际恢复）:")
		for _, risk := range plan.UnsafeConfig {
			fmt.Fprintf(w, "      - %s\n", risk)
		}
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

func restorePortBindings(inspect container.InspectResponse) []string {
	if inspect.HostConfig == nil || len(inspect.HostConfig.PortBindings) == 0 {
		return nil
	}
	var ports []string
	for port, bindings := range inspect.HostConfig.PortBindings {
		portText := port.String()
		if len(bindings) == 0 {
			ports = append(ports, portText)
			continue
		}
		for _, binding := range bindings {
			host := "0.0.0.0"
			if binding.HostIP.IsValid() {
				host = binding.HostIP.String()
			}
			if host == "" {
				host = "0.0.0.0"
			}
			if binding.HostPort == "" {
				ports = append(ports, fmt.Sprintf("%s->%s", host, portText))
				continue
			}
			ports = append(ports, fmt.Sprintf("%s:%s->%s", host, binding.HostPort, portText))
		}
	}
	sort.Strings(ports)
	return ports
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
