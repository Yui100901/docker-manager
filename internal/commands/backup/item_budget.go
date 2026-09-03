package backup

import (
	"context"
	"sync"

	"docker-manager/internal/runcontrol"

	"github.com/moby/moby/api/types/container"
)

type backupRelatedItems struct {
	networks map[string]struct{}
	volumes  map[string]struct{}
}

type restoreItemBudget struct {
	mu       sync.Mutex
	networks map[string]struct{}
	volumes  map[string]struct{}
}

func newRestoreItemBudget() *restoreItemBudget {
	return &restoreItemBudget{
		networks: make(map[string]struct{}),
		volumes:  make(map[string]struct{}),
	}
}

func newBackupRelatedItems() backupRelatedItems {
	return backupRelatedItems{
		networks: make(map[string]struct{}),
		volumes:  make(map[string]struct{}),
	}
}

func (items *backupRelatedItems) addContainerInspect(inspect container.InspectResponse) {
	if items == nil {
		return
	}
	for _, name := range backupNetworkNames(inspect) {
		items.networks[name] = struct{}{}
	}
	for _, name := range namedVolumes(inspect) {
		items.volumes[name] = struct{}{}
	}
}

func (items *backupRelatedItems) addManifest(manifest BackupManifest) {
	if items == nil {
		return
	}
	for _, entry := range manifest.Containers {
		for _, ref := range entry.Networks {
			if !isBuiltinNetwork(ref.Name) {
				items.networks[ref.Name] = struct{}{}
			}
		}
		for _, ref := range entry.Volumes {
			items.volumes[ref.Name] = struct{}{}
		}
	}
}

func reserveBackupContainerItems(ctx context.Context, count int) error {
	return runcontrol.CheckItems(ctx, "backup-container", count)
}

func hasBackupItemController(ctx context.Context) bool {
	_, ok := runcontrol.FromContext(ctx)
	return ok
}

func reserveRestoreContainerItems(ctx context.Context, count int) error {
	return runcontrol.CheckItems(ctx, "restore-container", count)
}

func reserveBackupRelatedItems(ctx context.Context, items backupRelatedItems) error {
	if err := runcontrol.CheckItems(ctx, "backup-network", len(items.networks)); err != nil {
		return err
	}
	return runcontrol.CheckItems(ctx, "backup-volume", len(items.volumes))
}

func reserveRestoreManifestItems(ctx context.Context, manifest BackupManifest, budget *restoreItemBudget) error {
	if err := reserveRestoreContainerItems(ctx, len(manifest.Containers)); err != nil {
		return err
	}
	items := newBackupRelatedItems()
	items.addManifest(manifest)
	if budget == nil {
		budget = newRestoreItemBudget()
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()

	newNetworks := missingBackupItemNames(items.networks, budget.networks)
	if err := runcontrol.CheckItems(ctx, "restore-network", len(newNetworks)); err != nil {
		return err
	}
	addBackupItemNames(budget.networks, newNetworks)
	newVolumes := missingBackupItemNames(items.volumes, budget.volumes)
	if err := runcontrol.CheckItems(ctx, "restore-volume", len(newVolumes)); err != nil {
		return err
	}
	addBackupItemNames(budget.volumes, newVolumes)
	return nil
}

func missingBackupItemNames(items, reserved map[string]struct{}) []string {
	names := make([]string, 0, len(items))
	for name := range items {
		if _, exists := reserved[name]; !exists {
			names = append(names, name)
		}
	}
	return names
}

func addBackupItemNames(dst map[string]struct{}, names []string) {
	for _, name := range names {
		dst[name] = struct{}{}
	}
}
