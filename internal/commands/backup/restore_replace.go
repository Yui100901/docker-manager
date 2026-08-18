package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
)

const restoreRollbackTimeout = 30 * time.Second

type stagedRestoreReplacement struct {
	oldID      string
	targetName string
	backupName string
	wasRunning bool
}

func createRestoredContainer(ctx context.Context, svc backupDockerService, inspect container.InspectResponse, targetName string, exists bool, expectedExistingID string, opts RestoreOptions) (string, error) {
	var staged *stagedRestoreReplacement
	var err error
	createName := targetName
	if exists {
		if !opts.Replace {
			return "", fmt.Errorf("container %s already exists; use --replace to overwrite", targetName)
		}
		staged, err = stageRestoreReplacement(ctx, svc, targetName, expectedExistingID)
		if err != nil {
			return "", err
		}
		createName = restoreCandidateName(targetName)
	}

	id, err := svc.CreateContainer(ctx, inspect, createName)
	if err != nil {
		var reconcileErr error
		if id == "" && staged != nil {
			id, reconcileErr = reconcileRestoreCreateError(svc, createName)
		}
		rollbackErr := rollbackRestoredContainer(svc, id, staged)
		return "", errors.Join(fmt.Errorf("create container %s: %w", targetName, err), reconcileErr, rollbackErr)
	}
	if err := checkBackupContext(ctx); err != nil {
		rollbackErr := rollbackRestoredContainer(svc, id, staged)
		return "", errors.Join(err, rollbackErr)
	}
	if !opts.NoStart {
		if err := svc.StartContainer(ctx, id); err != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, staged)
			return "", errors.Join(fmt.Errorf("start container %s: %w", targetName, err), rollbackErr)
		}
		if err := checkBackupContext(ctx); err != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, staged)
			return "", errors.Join(err, rollbackErr)
		}
		if staged != nil {
			if err := svc.WaitContainerReady(ctx, id, restoreRequiresHealthy(inspect)); err != nil {
				rollbackErr := rollbackRestoredContainer(svc, id, staged)
				return "", errors.Join(fmt.Errorf("container %s did not become ready: %w", targetName, err), rollbackErr)
			}
		}
	}
	if staged != nil {
		if err := checkBackupContext(ctx); err != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, staged)
			return "", errors.Join(err, rollbackErr)
		}
		if err := svc.RenameContainer(ctx, id, targetName); err != nil {
			renamed, reconcileErr := restoreContainerHasName(svc, id, targetName)
			if !renamed {
				rollbackErr := rollbackRestoredContainer(svc, id, staged)
				return "", errors.Join(fmt.Errorf("rename restored container %s to %s: %w", createName, targetName, err), reconcileErr, rollbackErr)
			}
			log.Printf("Restored container rename returned an error but the target name is committed: container=%s error=%v", targetName, err)
		}
		if err := checkBackupContext(ctx); err != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, staged)
			return "", errors.Join(err, rollbackErr)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
		defer cancel()
		if err := svc.RemoveContainer(cleanupCtx, staged.oldID); err != nil {
			_, inspectErr := svc.InspectContainer(cleanupCtx, staged.oldID)
			switch {
			case cerrdefs.IsNotFound(inspectErr):
				log.Printf("Replaced container removal returned an error but the old container is gone: container=%s error=%v", staged.targetName, err)
			case inspectErr == nil:
				return "", fmt.Errorf("remove replaced container %s (%s): %w; the old rollback container remains and the healthy new container was retained as %s", staged.targetName, staged.backupName, err, staged.targetName)
			default:
				return "", fmt.Errorf("remove replaced container %s (%s) returned %v and its state could not be verified (%v); the rollback container may remain and the healthy new container was retained as %s", staged.targetName, staged.backupName, err, inspectErr, staged.targetName)
			}
		}
	}
	return id, nil
}

func reconcileRestoreCreateError(svc backupDockerService, candidateName string) (string, error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
	defer cancel()
	actual, err := svc.InspectContainer(cleanupCtx, candidateName)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect candidate %s after create error: %w", candidateName, err)
	}
	if actual.ID == "" && normalizeContainerName(actual.Name) != candidateName {
		return "", nil
	}
	if actual.ID != "" {
		return actual.ID, nil
	}
	return candidateName, nil
}

func restoreContainerHasName(svc backupDockerService, id, targetName string) (bool, error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
	defer cancel()
	actual, err := svc.InspectContainer(cleanupCtx, id)
	if err != nil {
		return false, fmt.Errorf("inspect restored container %s after rename error: %w", id, err)
	}
	return normalizeContainerName(actual.Name) == targetName, nil
}

func restoreRequiresHealthy(inspect container.InspectResponse) bool {
	if inspect.Config == nil || inspect.Config.Healthcheck == nil || len(inspect.Config.Healthcheck.Test) == 0 {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(inspect.Config.Healthcheck.Test[0]), "NONE")
}

func stageRestoreReplacement(ctx context.Context, svc backupDockerService, targetName, expectedExistingID string) (*stagedRestoreReplacement, error) {
	previous, err := svc.InspectContainer(ctx, targetName)
	if err != nil {
		return nil, fmt.Errorf("inspect existing container %s: %w", targetName, err)
	}
	if previous.HostConfig != nil && previous.HostConfig.AutoRemove {
		return nil, fmt.Errorf("cannot safely replace auto-remove container %s because stopping it would delete the rollback copy", targetName)
	}
	if actualID := restoreContainerIdentity(previous, targetName); expectedExistingID != "" && actualID != expectedExistingID {
		return nil, fmt.Errorf("container %s changed after restore preflight: expected %s, found %s", targetName, expectedExistingID, actualID)
	}
	oldID := previous.ID
	if oldID == "" {
		oldID = targetName
	}
	staged := &stagedRestoreReplacement{
		oldID:      oldID,
		targetName: targetName,
		backupName: restoreRollbackName(targetName),
		wasRunning: previous.State != nil && previous.State.Running,
	}
	if staged.wasRunning {
		if err := svc.StopContainer(ctx, oldID); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
			_, inspectErr := svc.InspectContainer(cleanupCtx, oldID)
			restartErr := svc.StartContainer(cleanupCtx, oldID)
			cancel()
			return nil, errors.Join(fmt.Errorf("stop existing container %s: %w", targetName, err), inspectErr, restartErr)
		}
	}
	if err := svc.RenameContainer(ctx, oldID, staged.backupName); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
		actual, inspectErr := svc.InspectContainer(cleanupCtx, oldID)
		cancel()
		if inspectErr == nil && normalizeContainerName(actual.Name) == targetName {
			var restartErr error
			if staged.wasRunning {
				cleanupCtx, cancel = context.WithTimeout(context.Background(), restoreRollbackTimeout)
				restartErr = svc.StartContainer(cleanupCtx, oldID)
				cancel()
			}
			return nil, errors.Join(fmt.Errorf("temporarily rename existing container %s: %w", targetName, err), restartErr)
		}
		rollbackErr := rollbackRestoredContainer(svc, "", staged)
		return nil, errors.Join(fmt.Errorf("temporarily rename existing container %s: %w", targetName, err), inspectErr, rollbackErr)
	}
	return staged, nil
}

func rollbackRestoredContainer(svc backupDockerService, newID string, staged *stagedRestoreReplacement) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
	defer cancel()
	var rollbackErrors []error
	if newID != "" {
		if err := svc.RemoveContainer(cleanupCtx, newID); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback remove new container %s: %w", newID, err))
		}
	}
	if staged == nil {
		return errors.Join(rollbackErrors...)
	}
	if err := svc.RenameContainer(cleanupCtx, staged.oldID, staged.targetName); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback rename %s to %s: %w", staged.backupName, staged.targetName, err))
	}
	if staged.wasRunning {
		if err := svc.StartContainer(cleanupCtx, staged.oldID); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback restart original container %s: %w", staged.targetName, err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func restoreRollbackName(targetName string) string {
	return restoreTemporaryName(targetName, "rollback")
}

func restoreCandidateName(targetName string) string {
	return restoreTemporaryName(targetName, "candidate")
}

func restoreTemporaryName(targetName, purpose string) string {
	name := strings.TrimPrefix(normalizeContainerName(targetName), "/")
	if name == "" {
		name = "container"
	}
	if len(name) > 180 {
		name = name[:180]
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("%s-dm-restore-%s-%d", name, purpose, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-dm-restore-%s-%s", name, purpose, hex.EncodeToString(random))
}
