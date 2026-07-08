package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"docker-manager/internal/docker"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
)

func buildRestorePlanReport(ctx context.Context, backupPath string, opts RestoreOptions) (RestorePlanReport, error) {
	ctx = backupContext(ctx)
	if err := checkBackupContext(ctx); err != nil {
		return RestorePlanReport{}, err
	}
	resolvedDir, cleanup, err := resolveRestoreBackupDirWithOptions(ctx, backupPath, opts)
	if err != nil {
		return RestorePlanReport{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	checksumText := checksumPlanText(opts.SkipChecksum, filepath.Join(resolvedDir, backupChecksumName))
	if !opts.SkipChecksum {
		if _, err := verifyBackupChecksumsWithContext(ctx, resolvedDir); err != nil {
			return RestorePlanReport{}, fmt.Errorf("verify checksums: %w", err)
		}
	}
	svc, err := newBackupDockerService()
	if err != nil {
		return RestorePlanReport{}, err
	}
	return buildRestorePlanReportFromDir(ctx, svc, resolvedDir, backupPath, checksumText, opts)
}

func buildRestorePlanReportFromDir(ctx context.Context, svc backupDockerService, backupDir, source string, checksumText string, opts RestoreOptions) (RestorePlanReport, error) {
	manifest, err := readBackupManifest(backupDir)
	if err != nil {
		return RestorePlanReport{}, err
	}
	if len(manifest.Containers) == 0 {
		return RestorePlanReport{}, fmt.Errorf("manifest does not contain any containers")
	}
	if opts.Name != "" && len(manifest.Containers) != 1 {
		return RestorePlanReport{}, fmt.Errorf("--name 只支持恢复单个备份")
	}
	report := RestorePlanReport{
		Source:         source,
		DockerEndpoint: docker.Endpoint(),
		Checksum:       checksumText,
		ContainerCount: len(manifest.Containers),
		Options: RestorePlanOptions{
			Replace: opts.Replace,
			NoStart: opts.NoStart,
			Name:    opts.Name,
		},
	}
	ports, portWarnings := currentRestorePortBindings(ctx, svc)
	report.Warnings = append(report.Warnings, portWarnings...)
	for _, entry := range manifest.Containers {
		if err := checkBackupContext(ctx); err != nil {
			return report, err
		}
		plan, err := buildRestoreContainerPlan(ctx, svc, backupDir, entry, opts, ports)
		if err != nil {
			return report, err
		}
		report.Containers = append(report.Containers, plan)
		addRestorePlanSummary(&report.Summary, plan)
	}
	report.Summary.Warnings = len(report.Warnings)
	return report, nil
}

func buildRestoreContainerPlan(ctx context.Context, svc backupDockerService, backupDir string, entry BackupContainerManifest, opts RestoreOptions, existingPorts map[string]string) (RestoreContainerPlan, error) {
	entryDir := backupDir
	if entry.Path != "" {
		var err error
		entryDir, err = backupFilePath(backupDir, entry.Path)
		if err != nil {
			return RestoreContainerPlan{}, err
		}
	}
	inspect, err := readContainerInspect(entryDir, entry)
	if err != nil {
		return RestoreContainerPlan{}, err
	}
	targetName := opts.Name
	if targetName == "" {
		targetName = entry.ContainerName
	}
	if targetName == "" {
		targetName = normalizeContainerName(inspect.Name)
	}
	if targetName == "" {
		return RestoreContainerPlan{}, fmt.Errorf("backup does not contain a container name; use --name")
	}
	exists, err := svc.ContainerExists(ctx, targetName)
	if err != nil {
		return RestoreContainerPlan{}, err
	}
	plan := RestoreContainerPlan{
		ContainerName: targetName,
		SourceName:    entry.ContainerName,
		EntryDir:      entryDir,
		Ports:         restorePortBindings(inspect),
		Container:     restoreTargetPlan(exists, opts),
	}
	plan.Image = restoreImagePlan(ctx, svc, entryDir, entry, inspect)
	plan.Networks = restoreNetworkPlans(ctx, svc, entryDir, entry.Networks)
	plan.Volumes = restoreVolumePlans(ctx, svc, entryDir, entry.Volumes)
	plan.PortConflicts = restorePortConflicts(inspect, existingPorts, targetName)
	plan.Actions = restoreContainerActions(plan, opts)
	if exists && !opts.Replace {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("目标容器 %s 已存在；实际恢复需要 --replace 或改用 --name", targetName))
	}
	if len(plan.PortConflicts) > 0 {
		plan.Warnings = append(plan.Warnings, "存在端口冲突，实际恢复前需要释放端口或调整容器配置")
	}
	return plan, nil
}

func restoreImagePlan(ctx context.Context, svc backupDockerService, entryDir string, entry BackupContainerManifest, inspect container.InspectResponse) RestoreImagePlan {
	imageRef := entry.Image
	if imageRef == "" && inspect.Config != nil {
		imageRef = inspect.Config.Image
	}
	plan := RestoreImagePlan{Ref: imageRef, Archive: entry.ImageArchive, Action: "skip"}
	if entry.ImageArchive != "" {
		imagePath, err := backupFilePath(entryDir, entry.ImageArchive)
		if err != nil {
			plan.Error = err.Error()
			plan.Action = "error"
			return plan
		}
		plan.ArchivePath = imagePath
		plan.Action = "load-archive"
		if _, err := os.Stat(imagePath); err != nil {
			plan.Error = err.Error()
			plan.Action = "error"
			return plan
		}
	}
	if imageRef == "" {
		return plan
	}
	exists, err := svc.ImageExists(ctx, imageRef)
	if err != nil {
		plan.Error = err.Error()
		return plan
	}
	plan.Exists = exists
	if exists && entry.ImageArchive == "" {
		plan.Action = "reuse"
	}
	if exists && entry.ImageArchive != "" {
		plan.Action = "load-archive"
	}
	if !exists && entry.ImageArchive == "" {
		plan.Action = "missing"
	}
	return plan
}

func restoreNetworkPlans(ctx context.Context, svc backupDockerService, entryDir string, refs []BackupResourceRef) []RestoreResourcePlan {
	plans := make([]RestoreResourcePlan, 0, len(refs))
	for _, ref := range refs {
		plan := RestoreResourcePlan{Name: ref.Name, File: ref.File, Action: "create"}
		expected, err := readNetworkInspect(entryDir, ref)
		if err != nil {
			plan.Action = "error"
			plan.Error = err.Error()
			plans = append(plans, plan)
			continue
		}
		actual, err := svc.InspectNetwork(ctx, ref.Name)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				plans = append(plans, plan)
				continue
			}
			plan.Action = "error"
			plan.Error = err.Error()
			plans = append(plans, plan)
			continue
		}
		plan.Exists = true
		plan.Action = "reuse"
		plan.Differences = compareRestoreNetwork(expected, actual)
		if len(plan.Differences) > 0 {
			plan.Different = true
			plan.Action = "reuse-different"
		}
		plans = append(plans, plan)
	}
	return plans
}

func restoreVolumePlans(ctx context.Context, svc backupDockerService, entryDir string, refs []BackupResourceRef) []RestoreResourcePlan {
	plans := make([]RestoreResourcePlan, 0, len(refs))
	for _, ref := range refs {
		plan := RestoreResourcePlan{Name: ref.Name, File: ref.File, Action: "create"}
		expected, err := readVolumeInspect(entryDir, ref)
		if err != nil {
			plan.Action = "error"
			plan.Error = err.Error()
			plans = append(plans, plan)
			continue
		}
		actual, err := svc.InspectVolume(ctx, ref.Name)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				plans = append(plans, plan)
				continue
			}
			plan.Action = "error"
			plan.Error = err.Error()
			plans = append(plans, plan)
			continue
		}
		plan.Exists = true
		plan.Action = "reuse"
		plan.Differences = compareRestoreVolume(expected, actual)
		if len(plan.Differences) > 0 {
			plan.Different = true
			plan.Action = "reuse-different"
		}
		plans = append(plans, plan)
	}
	return plans
}

func restoreTargetPlan(exists bool, opts RestoreOptions) RestoreTargetPlan {
	switch {
	case exists && opts.Replace:
		return RestoreTargetPlan{Exists: true, Action: "replace"}
	case exists:
		return RestoreTargetPlan{Exists: true, Action: "conflict"}
	default:
		return RestoreTargetPlan{Exists: false, Action: "create"}
	}
}

func restoreContainerActions(plan RestoreContainerPlan, opts RestoreOptions) []string {
	var actions []string
	if plan.Image.Action == "load-archive" {
		actions = append(actions, "load-image")
	}
	for _, net := range plan.Networks {
		if net.Action == "create" {
			actions = append(actions, "create-network:"+net.Name)
		}
	}
	for _, vol := range plan.Volumes {
		if vol.Action == "create" {
			actions = append(actions, "create-volume:"+vol.Name)
		}
	}
	if plan.Container.Action == "replace" {
		actions = append(actions, "remove-container:"+plan.ContainerName)
	}
	actions = append(actions, "create-container:"+plan.ContainerName)
	if !opts.NoStart {
		actions = append(actions, "start-container:"+plan.ContainerName)
	}
	return actions
}

func currentRestorePortBindings(ctx context.Context, svc backupDockerService) (map[string]string, []string) {
	containers, err := svc.ListContainers(ctx, true)
	if err != nil {
		return nil, []string{fmt.Sprintf("无法列出现有容器检查端口冲突: %v", err)}
	}
	result := map[string]string{}
	var warnings []string
	for _, summary := range containers {
		ref := summary.ID
		if ref == "" {
			ref = firstContainerName(summary.Names)
		}
		if ref == "" {
			continue
		}
		inspect, err := svc.InspectContainer(ctx, ref)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("无法 inspect 容器 %s 检查端口冲突: %v", ref, err))
			continue
		}
		name := normalizeContainerName(inspect.Name)
		if name == "" {
			name = firstContainerName(summary.Names)
		}
		if name == "" {
			name = restoreShortID(summary.ID)
		}
		for _, key := range restorePortKeys(inspect) {
			result[key] = name
		}
	}
	return result, warnings
}

func restorePortConflicts(inspect container.InspectResponse, existing map[string]string, targetName string) []RestorePortConflict {
	if len(existing) == 0 {
		return nil
	}
	var conflicts []RestorePortConflict
	for _, key := range restorePortKeys(inspect) {
		containerName, ok := existing[key]
		if !ok || containerName == targetName {
			continue
		}
		conflicts = append(conflicts, RestorePortConflict{Port: key, Container: containerName})
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].Port == conflicts[j].Port {
			return conflicts[i].Container < conflicts[j].Container
		}
		return conflicts[i].Port < conflicts[j].Port
	})
	return conflicts
}

func restorePortKeys(inspect container.InspectResponse) []string {
	if inspect.HostConfig == nil {
		return nil
	}
	var keys []string
	for port, bindings := range inspect.HostConfig.PortBindings {
		proto := port.Proto()
		for _, binding := range bindings {
			host := "0.0.0.0"
			if binding.HostIP.IsValid() {
				host = binding.HostIP.String()
			}
			if binding.HostPort == "" {
				continue
			}
			keys = append(keys, fmt.Sprintf("%s:%s/%s", host, binding.HostPort, proto))
		}
	}
	sort.Strings(keys)
	return keys
}

func printRestorePlanReport(w io.Writer, report RestorePlanReport) {
	fmt.Fprintf(w, "恢复计划: %s\n", report.Source)
	if report.DockerEndpoint != "" {
		fmt.Fprintf(w, "目标 Docker: %s\n", report.DockerEndpoint)
	}
	fmt.Fprintf(w, "容器数量: %d checksum=%s\n", report.ContainerCount, report.Checksum)
	fmt.Fprintf(w, "摘要: 镜像导入=%d 已存在镜像=%d network创建=%d 已存在network=%d 差异network=%d volume创建=%d 已存在volume=%d 差异volume=%d 容器创建=%d 替换=%d 冲突=%d 端口冲突=%d\n\n",
		report.Summary.ImagesToLoad,
		report.Summary.ImagesPresent,
		report.Summary.NetworksToCreate,
		report.Summary.NetworksPresent,
		report.Summary.NetworksDifferent,
		report.Summary.VolumesToCreate,
		report.Summary.VolumesPresent,
		report.Summary.VolumesDifferent,
		report.Summary.ContainersToCreate,
		report.Summary.ContainersToReplace,
		report.Summary.ContainerConflicts,
		report.Summary.PortConflicts,
	)
	for _, warning := range report.Warnings {
		fmt.Fprintf(w, "警告: %s\n", warning)
	}
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w)
	}
	for _, plan := range report.Containers {
		printRestoreContainerPlan(w, plan)
	}
}

func printRestoreContainerPlan(w io.Writer, plan RestoreContainerPlan) {
	fmt.Fprintf(w, "- 容器: %s", plan.ContainerName)
	if plan.SourceName != "" && plan.SourceName != plan.ContainerName {
		fmt.Fprintf(w, " source=%s", plan.SourceName)
	}
	fmt.Fprintf(w, " action=%s backup=%s\n", plan.Container.Action, plan.EntryDir)
	if plan.Image.Ref != "" || plan.Image.Archive != "" {
		fmt.Fprintf(w, "  镜像: ref=%s archive=%s exists=%v action=%s\n", plan.Image.Ref, plan.Image.Archive, plan.Image.Exists, plan.Image.Action)
		if plan.Image.Error != "" {
			fmt.Fprintf(w, "    error=%s\n", plan.Image.Error)
		}
	}
	printRestoreResourcePlans(w, "network", plan.Networks)
	printRestoreResourcePlans(w, "volume", plan.Volumes)
	if len(plan.Ports) > 0 {
		fmt.Fprintf(w, "  端口: %s\n", strings.Join(plan.Ports, ", "))
	}
	for _, conflict := range plan.PortConflicts {
		fmt.Fprintf(w, "  端口冲突: %s 已被 %s 使用\n", conflict.Port, conflict.Container)
	}
	if len(plan.Actions) > 0 {
		fmt.Fprintf(w, "  动作: %s\n", strings.Join(plan.Actions, ", "))
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(w, "  预检提示: %s\n", warning)
	}
}

func printRestoreResourcePlans(w io.Writer, kind string, plans []RestoreResourcePlan) {
	for _, plan := range plans {
		fmt.Fprintf(w, "  %s: %s exists=%v action=%s", kind, plan.Name, plan.Exists, plan.Action)
		if plan.Error != "" {
			fmt.Fprintf(w, " error=%s", plan.Error)
		}
		fmt.Fprintln(w)
		for _, diff := range plan.Differences {
			fmt.Fprintf(w, "    diff: %s\n", diff)
		}
	}
}
