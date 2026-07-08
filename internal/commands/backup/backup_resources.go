package backup

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/moby/moby/api/types/container"
)

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
		if err := checkBackupContext(ctx); err != nil {
			return nil, err
		}
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
		if err := checkBackupContext(ctx); err != nil {
			return nil, err
		}
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
		if err := checkBackupContext(ctx); err != nil {
			return nil, err
		}
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
		if err := checkBackupContext(ctx); err != nil {
			return nil, err
		}
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
