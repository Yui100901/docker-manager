package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
)

const (
	restoreRollbackTimeout     = 30 * time.Second
	restoreStatePollInterval   = 100 * time.Millisecond
	restoreCandidateAttempts   = 8
	restoreStopRecoveryMax     = 2 * time.Minute
	restoreStopRecoveryMargin  = 5 * time.Second
	restoreFinalInspectTimeout = 5 * time.Second
	restoreCandidateOwnerLabel = "com.docker-manager.restore-owner"
)

type stagedRestoreReplacement struct {
	oldID      string
	targetName string
	backupName string
	wasRunning bool
}

func createRestoredContainer(ctx context.Context, svc backupDockerService, inspect container.InspectResponse, targetName string, exists bool, expectedExistingID string, opts RestoreOptions) (string, error) {
	var staged *stagedRestoreReplacement
	var err error
	candidateOwner, err := newRestoreCandidateOwner()
	if err != nil {
		return "", err
	}
	createName, err := unusedRestoreCandidateName(ctx, svc, targetName)
	if err != nil {
		return "", err
	}
	if exists {
		if !opts.Replace {
			return "", fmt.Errorf("container %s already exists; use --replace to overwrite", targetName)
		}
		staged, err = stageRestoreReplacement(ctx, svc, targetName, expectedExistingID)
		if err != nil {
			return "", err
		}
	}

	candidateInspect := withRestoreCandidateOwner(inspect, candidateOwner)
	id, err := svc.CreateContainer(ctx, candidateInspect, createName)
	if err != nil {
		var reconcileErr error
		if id == "" {
			id, reconcileErr = reconcileRestoreCreateError(svc, createName, candidateOwner)
		}
		rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, staged)
		return "", errors.Join(fmt.Errorf("create container %s: %w", targetName, err), reconcileErr, rollbackErr)
	}
	if err := checkBackupContext(ctx); err != nil {
		rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, staged)
		return "", errors.Join(err, rollbackErr)
	}
	// A replacement candidate must be proven ready before it claims the target
	// name. A new, non-replacement container is committed first and then started
	// so one-shot and auto-remove workloads are not mistaken for failed restores.
	if staged != nil && !opts.NoStart {
		if err := svc.StartContainer(ctx, id); err != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, staged)
			return "", errors.Join(fmt.Errorf("start container %s: %w", targetName, err), rollbackErr)
		}
		if err := checkBackupContext(ctx); err != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, staged)
			return "", errors.Join(err, rollbackErr)
		}
		readyTimeout := opts.ReadyTimeout
		if readyTimeout <= 0 {
			readyTimeout = defaultRestoreReadyTimeout
		}
		readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
		err := svc.WaitContainerReady(readyCtx, id, restoreRequiresHealthy(inspect))
		cancel()
		if err != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, staged)
			return "", errors.Join(fmt.Errorf("container %s did not become ready: %w", targetName, err), rollbackErr)
		}
	}
	if err := checkBackupContext(ctx); err != nil {
		rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, staged)
		return "", errors.Join(err, rollbackErr)
	}
	if staged == nil {
		occupied, existsErr := svc.ContainerExists(ctx, targetName)
		if existsErr != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, nil)
			return "", errors.Join(fmt.Errorf("recheck restore target %s before commit: %w", targetName, existsErr), rollbackErr)
		}
		if occupied {
			rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, nil)
			return "", errors.Join(fmt.Errorf("container %s appeared while the restore candidate was being prepared; refusing to replace it", targetName), rollbackErr)
		}
	}
	if err := svc.RenameContainer(ctx, id, targetName); err != nil {
		renamed, reconcileErr := restoreContainerHasName(svc, id, targetName)
		if !renamed {
			rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, staged)
			return "", errors.Join(fmt.Errorf("rename restored container %s to %s: %w", createName, targetName, err), reconcileErr, rollbackErr)
		}
		log.Printf("Restored container rename returned an error but the target name is committed: container=%s error=%v", targetName, err)
	}
	if err := checkBackupContext(ctx); err != nil {
		rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, staged)
		return "", errors.Join(err, rollbackErr)
	}
	if staged == nil && !opts.NoStart {
		if err := svc.StartContainer(ctx, id); err != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, nil)
			return "", errors.Join(fmt.Errorf("start container %s: %w", targetName, err), rollbackErr)
		}
		if err := checkBackupContext(ctx); err != nil {
			rollbackErr := rollbackRestoredContainer(svc, id, candidateOwner, nil)
			return "", errors.Join(err, rollbackErr)
		}
	}
	if staged != nil {
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

func reconcileRestoreCreateError(svc backupDockerService, candidateName, candidateOwner string) (string, error) {
	id, exists, err := inspectOwnedRestoreCandidate(svc, candidateName, candidateOwner)
	if err != nil {
		return "", fmt.Errorf("reconcile candidate %s after create error: %w", candidateName, err)
	}
	if !exists {
		return "", nil
	}
	return id, nil
}

func restoreContainerHasName(svc backupDockerService, id, targetName string) (bool, error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreFinalInspectTimeout)
	defer cancel()
	actual, err := svc.InspectContainer(cleanupCtx, id)
	if err != nil {
		return false, fmt.Errorf("inspect container %s after rename error: %w", id, err)
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
			recoveryErr := recoverRestoreContainerAfterStopError(svc, oldID, restoreStopRecoveryTimeout(previous), restoreStopResultUncertain(err))
			return nil, errors.Join(fmt.Errorf("stop existing container %s: %w", targetName, err), recoveryErr)
		}
	}
	if err := svc.RenameContainer(ctx, oldID, staged.backupName); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
		actual, inspectErr := svc.InspectContainer(cleanupCtx, oldID)
		cancel()
		if inspectErr == nil && normalizeContainerName(actual.Name) == targetName {
			var restartErr error
			if staged.wasRunning {
				restartErr = restartRestoreContainer(svc, oldID, targetName)
			}
			return nil, errors.Join(fmt.Errorf("temporarily rename existing container %s: %w", targetName, err), restartErr)
		}
		rollbackErr := rollbackRestoredContainer(svc, "", "", staged)
		return nil, errors.Join(fmt.Errorf("temporarily rename existing container %s: %w", targetName, err), inspectErr, rollbackErr)
	}
	return staged, nil
}

func rollbackRestoredContainer(svc backupDockerService, newID, candidateOwner string, staged *stagedRestoreReplacement) error {
	var rollbackErrors []error
	if newID != "" {
		if err := removeRestoreContainerForRollback(svc, newID, candidateOwner); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
	}
	if staged == nil {
		return errors.Join(rollbackErrors...)
	}
	if err := renameRestoreContainerForRollback(svc, staged.oldID, staged.backupName, staged.targetName); err != nil {
		rollbackErrors = append(rollbackErrors, err)
	}
	if staged.wasRunning {
		if err := restartRestoreContainer(svc, staged.oldID, staged.targetName); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
	}
	return errors.Join(rollbackErrors...)
}

func removeRestoreContainerForRollback(svc backupDockerService, candidateRef, candidateOwner string) error {
	id, exists, ownershipErr := inspectOwnedRestoreCandidate(svc, candidateRef, candidateOwner)
	if ownershipErr != nil {
		return fmt.Errorf("verify restore candidate ownership before rollback: %w", ownershipErr)
	}
	if !exists {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
	err := svc.RemoveContainer(cleanupCtx, id)
	cancel()
	if err == nil {
		return nil
	}

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), restoreFinalInspectTimeout)
	_, inspectErr := svc.InspectContainer(verifyCtx, id)
	verifyCancel()
	switch {
	case cerrdefs.IsNotFound(inspectErr):
		log.Printf("Rollback removal returned an error but the new container is gone: container=%s error=%v", id, err)
		return nil
	case inspectErr == nil:
		return fmt.Errorf("rollback remove new container %s: %w; the container still exists", id, err)
	default:
		return errors.Join(
			fmt.Errorf("rollback remove new container %s: %w", id, err),
			fmt.Errorf("inspect new container %s after rollback removal error: %w", id, inspectErr),
		)
	}
}

func inspectOwnedRestoreCandidate(svc backupDockerService, candidateRef, candidateOwner string) (string, bool, error) {
	if candidateOwner == "" {
		return "", false, errors.New("restore candidate ownership marker is empty; refusing cleanup")
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
	actual, err := svc.InspectContainer(cleanupCtx, candidateRef)
	cancel()
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect restore candidate %s: %w", candidateRef, err)
	}
	if actual.ID == "" && normalizeContainerName(actual.Name) == "" {
		return "", false, nil
	}
	if actual.Config == nil || actual.Config.Labels[restoreCandidateOwnerLabel] != candidateOwner {
		return "", true, fmt.Errorf("container %s does not carry this restore transaction's ownership label; refusing cleanup", candidateRef)
	}
	if actual.ID == "" {
		return "", true, fmt.Errorf("owned restore candidate %s has no stable container ID; refusing cleanup", candidateRef)
	}
	return actual.ID, true, nil
}

func withRestoreCandidateOwner(inspect container.InspectResponse, owner string) container.InspectResponse {
	config := &container.Config{}
	if inspect.Config != nil {
		*config = *inspect.Config
	}
	config.Labels = make(map[string]string, len(config.Labels)+1)
	if inspect.Config != nil {
		for key, value := range inspect.Config.Labels {
			config.Labels[key] = value
		}
	}
	config.Labels[restoreCandidateOwnerLabel] = owner
	inspect.Config = config
	return inspect
}

func newRestoreCandidateOwner() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate restore candidate ownership marker: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func renameRestoreContainerForRollback(svc backupDockerService, id, previousName, targetName string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
	err := svc.RenameContainer(cleanupCtx, id, targetName)
	cancel()
	if err == nil {
		return nil
	}

	renamed, inspectErr := restoreContainerHasName(svc, id, targetName)
	if renamed {
		log.Printf("Rollback rename returned an error but the original container name is restored: container=%s error=%v", targetName, err)
		return nil
	}
	baseErr := fmt.Errorf("rollback rename %s to %s: %w", previousName, targetName, err)
	if inspectErr != nil {
		return errors.Join(baseErr, inspectErr)
	}
	return errors.Join(baseErr, fmt.Errorf("original container %s did not regain target name %s", id, targetName))
}

func restartRestoreContainer(svc backupDockerService, id, targetName string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreRollbackTimeout)
	err := svc.StartContainer(cleanupCtx, id)
	cancel()
	if err == nil {
		return nil
	}

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), restoreFinalInspectTimeout)
	actual, inspectErr := svc.InspectContainer(verifyCtx, id)
	verifyCancel()
	if inspectErr == nil && actual.State != nil && actual.State.Running {
		log.Printf("Rollback restart returned an error but the original container is running: container=%s error=%v", targetName, err)
		return nil
	}
	baseErr := fmt.Errorf("rollback restart original container %s: %w", targetName, err)
	if inspectErr != nil {
		return errors.Join(baseErr, fmt.Errorf("inspect original container %s after rollback restart error: %w", id, inspectErr))
	}
	return errors.Join(baseErr, fmt.Errorf("original container %s is not running after rollback restart", targetName))
}

func restoreRollbackName(targetName string) string {
	return restoreTemporaryName(targetName, "rollback")
}

func restoreCandidateName(targetName string) string {
	return restoreTemporaryName(targetName, "candidate")
}

func unusedRestoreCandidateName(ctx context.Context, svc backupDockerService, targetName string) (string, error) {
	for attempt := 0; attempt < restoreCandidateAttempts; attempt++ {
		if err := checkBackupContext(ctx); err != nil {
			return "", err
		}
		name := restoreCandidateName(targetName)
		exists, err := svc.ContainerExists(ctx, name)
		if err != nil {
			return "", fmt.Errorf("check restore candidate name %s: %w", name, err)
		}
		if !exists {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not allocate an unused restore candidate name for %s", targetName)
}

func restoreStopRecoveryTimeout(inspect container.InspectResponse) time.Duration {
	timeout := restoreRollbackTimeout
	if inspect.Config == nil || inspect.Config.StopTimeout == nil || *inspect.Config.StopTimeout <= 0 {
		return timeout
	}
	candidate := time.Duration(*inspect.Config.StopTimeout)*time.Second + restoreStopRecoveryMargin
	if candidate > timeout {
		timeout = candidate
	}
	if timeout > restoreStopRecoveryMax {
		return restoreStopRecoveryMax
	}
	return timeout
}

func restoreStopResultUncertain(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func recoverRestoreContainerAfterStopError(svc backupDockerService, id string, timeout time.Duration, uncertain bool) error {
	if uncertain {
		return recoverRunningRestoreContainer(svc, id, timeout)
	}

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), restoreFinalInspectTimeout)
	current, inspectErr := svc.InspectContainer(verifyCtx, id)
	verifyCancel()
	if inspectErr != nil {
		return fmt.Errorf("inspect original restore container %s after stop error: %w", id, inspectErr)
	}
	if current.State != nil && current.State.Running {
		return nil
	}
	return restartRestoreContainer(svc, id, id)
}

func recoverRunningRestoreContainer(svc backupDockerService, id string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = restoreRollbackTimeout
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(restoreStatePollInterval)
	defer ticker.Stop()

	sawStopped := false
	var lastErr error
	for {
		current, err := svc.InspectContainer(cleanupCtx, id)
		if err != nil {
			lastErr = fmt.Errorf("inspect original restore container %s while reconciling stop: %w", id, err)
		} else if current.State != nil && current.State.Running {
			if sawStopped {
				return nil
			}
			lastErr = nil
		} else {
			sawStopped = true
			if startErr := svc.StartContainer(cleanupCtx, id); startErr != nil {
				lastErr = fmt.Errorf("restart original restore container %s after uncertain stop: %w", id, startErr)
			} else {
				lastErr = nil
			}
		}

		select {
		case <-cleanupCtx.Done():
			verifyCtx, verifyCancel := context.WithTimeout(context.Background(), restoreFinalInspectTimeout)
			current, inspectErr := svc.InspectContainer(verifyCtx, id)
			verifyCancel()
			if inspectErr == nil && current.State != nil && current.State.Running {
				return nil
			}
			return errors.Join(lastErr, inspectErr, fmt.Errorf("restore original container %s to running state: %w", id, cleanupCtx.Err()))
		case <-ticker.C:
		}
	}
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
