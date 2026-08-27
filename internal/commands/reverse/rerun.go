package reverse

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

	"docker-manager/internal/audit"
	"docker-manager/internal/commandflags"
	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	"docker-manager/internal/runcontrol"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"

	"github.com/spf13/cobra"
)

const (
	defaultRerunReadyTimeout = 30 * time.Second
	rerunRollbackTimeout     = 30 * time.Second
	rerunStatePollInterval   = 100 * time.Millisecond
	rerunStopRecoveryMax     = 2 * time.Minute
	rerunStopRecoveryMargin  = 5 * time.Second
	rerunFinalInspectTimeout = 5 * time.Second
	rerunCandidateOwnerLabel = "com.docker-manager.rerun-owner"
)

type RerunCommandDefaults struct {
	ReadyTimeout time.Duration
}

func NewRerunCommand() *cobra.Command {
	return NewRerunCommandWithDefaults(nil)
}

func NewRerunCommandWithDefaults(defaults func() RerunCommandDefaults) *cobra.Command {
	var (
		dryRun       bool
		confirm      bool
		running      bool
		filters      []string
		readyTimeout = defaultRerunReadyTimeout
	)

	cmd := &cobra.Command{
		Use:   "rerun [container-filter...]",
		Short: "基于 Docker inspect 停止、删除并重建容器",
		RunE: func(cmd *cobra.Command, args []string) error {
			effectiveReadyTimeout := readyTimeout
			if !cmd.Flags().Changed("ready-timeout") && defaults != nil {
				if value := defaults().ReadyTimeout; value > 0 {
					effectiveReadyTimeout = value
				}
			}
			if effectiveReadyTimeout <= 0 {
				return fmt.Errorf("--ready-timeout 必须大于 0")
			}
			if len(args) == 0 && len(filters) == 0 && !running {
				return fmt.Errorf("rerun 是破坏性操作，必须提供容器名称/筛选条件，或显式使用 --running")
			}
			if !dryRun && !confirm {
				return fmt.Errorf("%s；如确认执行，请添加 --confirm；如仅审计，请使用 --dry-run", destructiveDockerMessage("rerun 会停止、删除并重建容器"))
			}
			targetFilters := append(append([]string(nil), filters...), args...)
			ctx := cmd.Context()
			targets, err := resolveReverseContainerTargetsContext(ctx, targetFilters, running)
			if err != nil {
				return err
			}
			backupDir := inspectBackupDir(time.Now())
			if !dryRun {
				if err := authorizeRerunMutations(ctx, targets, backupDir, confirm); err != nil {
					return fmt.Errorf("审计授权失败，未执行 rerun: %w", err)
				}
			}
			if !dryRun {
				printDestructiveDockerTarget(cmd.OutOrStdout())
			}
			return rerunContainers(ctx, targets, rerunOptions{
				DryRun:       dryRun,
				ReadyTimeout: effectiveReadyTimeout,
				Output:       cmd.OutOrStdout(),
				BackupDir:    backupDir,
			})
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "仅打印将要执行的重建动作，不修改 Docker")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "确认执行停止、删除并重建容器操作")
	cmd.Flags().DurationVar(&readyTimeout, "ready-timeout", defaultRerunReadyTimeout, "候选容器等待 running/healthy 的最长时间")
	commandflags.AddContainerFilterFlags(cmd, &running, &filters, "仅筛选正在运行的容器")
	return cmd
}

func authorizeRerunMutations(ctx context.Context, targets []string, backupDir string, confirm bool) error {
	session := audit.FromContext(ctx)
	if session == nil {
		return nil
	}
	fileCandidates := make([]audit.CandidateInput, 0, len(targets))
	dockerCandidates := make([]audit.CandidateInput, 0, len(targets))
	for _, target := range targets {
		backupPath := inspectBackupPath(backupDir, target)
		fileCandidates = append(fileCandidates, audit.CandidateInput{Kind: "file", Action: "write", Identifier: backupPath, Display: backupPath})
		dockerCandidates = append(dockerCandidates, audit.CandidateInput{Kind: "container", Action: "rerun", Identifier: target, Display: target})
	}
	if _, err := session.AuthorizeMutation(ctx, audit.MutationRequest{
		Scope:        audit.MutationFilesystem,
		Confirmation: audit.Confirmation{Provided: true, Mechanism: "rerun-inspect-backup"},
		Candidates:   fileCandidates,
	}); err != nil {
		return err
	}
	_, err := session.AuthorizeMutation(ctx, audit.MutationRequest{
		Scope:        audit.MutationDockerPersistent,
		Confirmation: audit.Confirmation{Required: true, Provided: confirm, Mechanism: "--confirm"},
		Candidates:   dockerCandidates,
	})
	return err
}

func destructiveDockerMessage(action string) string {
	if docker.IsRemoteEndpoint() {
		return fmt.Sprintf("%s；目标 Docker: %s", action, docker.Endpoint())
	}
	return action
}

func printDestructiveDockerTarget(w io.Writer) {
	if docker.IsRemoteEndpoint() {
		fmt.Fprintf(w, "Target Docker: %s\n", docker.Endpoint())
	}
}

type rerunOptions struct {
	DryRun       bool
	ReadyTimeout time.Duration
	Output       io.Writer
	Service      rerunDockerService
	BackupDir    string
}

type rerunDockerService interface {
	InspectContext(ctx context.Context, containerID string) (container.InspectResponse, error)
	StopContext(ctx context.Context, containerID string) error
	RenameContext(ctx context.Context, containerID, newName string) error
	CreateFromInspectContext(ctx context.Context, inspect container.InspectResponse, name string) (string, error)
	StartContext(ctx context.Context, containerID string) error
	WaitReadyContext(ctx context.Context, containerID string, requireHealthy bool) error
	RemoveContext(ctx context.Context, containerID string, force, removeVolumes bool) error
}

func rerunContainers(ctx context.Context, names []string, opts rerunOptions) error {
	output := opts.Output
	if output == nil {
		output = io.Discard
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := runcontrol.CheckItems(ctx, "container", len(names)); err != nil {
		return err
	}
	service := opts.Service
	if service == nil {
		if err := ensureContainerManager(); err != nil {
			return err
		}
		service = containerManager
	}
	var firstErr error
	backupDir := opts.BackupDir
	if backupDir == "" {
		backupDir = inspectBackupDir(time.Now())
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if opts.DryRun {
			fmt.Fprintf(output, "Dry run: backup inspect for %s to %s\n", name, inspectBackupPath(backupDir, name))
			fmt.Fprintf(output, "Dry run: stage %s under a rollback name, validate a candidate, then commit or roll back\n", name)
			continue
		}
		backupPath, err := backupContainerInspectContext(ctx, name, backupDir)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("备份容器 %s inspect 失败: %w", name, err)
			}
			continue
		}
		fmt.Fprintf(output, "Backup inspect %s to %s\n", name, backupPath)

		containerID, err := safelyRerunContainer(ctx, service, name, opts.ReadyTimeout)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("重建容器 %s 失败: %w", name, err)
			}
			continue
		}
		fmt.Fprintf(output, "Recreate container %s id %s\n", name, containerID)
	}
	return firstErr
}

type stagedRerunContainer struct {
	oldID        string
	targetName   string
	rollbackName string
	wasRunning   bool
}

func safelyRerunContainer(ctx context.Context, svc rerunDockerService, name string, readyTimeout time.Duration) (string, error) {
	if readyTimeout <= 0 {
		readyTimeout = defaultRerunReadyTimeout
	}
	inspect, err := svc.InspectContext(ctx, name)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	targetName := strings.TrimPrefix(strings.TrimSpace(inspect.Name), "/")
	if targetName == "" {
		targetName = strings.TrimPrefix(strings.TrimSpace(name), "/")
	}
	if targetName == "" {
		return "", errors.New("container has no usable name")
	}
	if inspect.HostConfig != nil && inspect.HostConfig.AutoRemove {
		return "", fmt.Errorf("cannot safely rerun auto-remove container %s because stopping it would delete the rollback copy", targetName)
	}
	oldID := inspect.ID
	if oldID == "" {
		oldID = name
	}
	candidateOwner, err := newRerunCandidateOwner()
	if err != nil {
		return "", err
	}
	staged := &stagedRerunContainer{
		oldID:        oldID,
		targetName:   targetName,
		rollbackName: rerunTemporaryName(targetName, "rollback"),
		wasRunning:   inspect.State != nil && inspect.State.Running,
	}

	if staged.wasRunning {
		if err := svc.StopContext(ctx, oldID); err != nil {
			var recoveryErr error
			if rerunStopResultUncertain(err) {
				recoveryErr = recoverRunningRerunContainer(svc, oldID, rerunStopRecoveryTimeout(inspect))
			} else {
				recoveryErr = restoreRunningRerunContainer(svc, oldID)
			}
			return "", errors.Join(fmt.Errorf("stop existing container %s: %w", targetName, err), recoveryErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", errors.Join(err, rollbackRerunContainer(svc, "", candidateOwner, staged))
	}
	if err := svc.RenameContext(ctx, oldID, staged.rollbackName); err != nil {
		current, inspectErr := inspectRerunContainerForCleanup(svc, oldID)
		if inspectErr != nil || normalizeRerunName(current.Name) != staged.rollbackName {
			return "", errors.Join(fmt.Errorf("stage existing container %s: %w", targetName, err), inspectErr, rollbackRerunContainer(svc, "", candidateOwner, staged))
		}
		log.Printf("rerun staging rename returned an error but was committed: container=%s error=%v", targetName, err)
	}

	candidateName := rerunTemporaryName(targetName, "candidate")
	candidateInspect := withRerunCandidateOwner(inspect, candidateOwner)
	candidateID, err := svc.CreateFromInspectContext(ctx, candidateInspect, candidateName)
	if err != nil {
		candidateRef := candidateID
		if candidateRef == "" {
			candidateRef = candidateName
		}
		return "", errors.Join(fmt.Errorf("create rerun candidate %s: %w", targetName, err), rollbackRerunContainer(svc, candidateRef, candidateOwner, staged))
	}
	if candidateID == "" {
		candidateID = candidateName
	}
	if err := ctx.Err(); err != nil {
		return "", errors.Join(err, rollbackRerunContainer(svc, candidateID, candidateOwner, staged))
	}
	if staged.wasRunning {
		if err := svc.StartContext(ctx, candidateID); err != nil {
			return "", errors.Join(fmt.Errorf("start rerun candidate %s: %w", targetName, err), rollbackRerunContainer(svc, candidateID, candidateOwner, staged))
		}
		readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
		readyErr := svc.WaitReadyContext(readyCtx, candidateID, rerunRequiresHealthy(inspect))
		cancel()
		if readyErr != nil {
			return "", errors.Join(fmt.Errorf("rerun candidate %s did not become ready: %w", targetName, readyErr), rollbackRerunContainer(svc, candidateID, candidateOwner, staged))
		}
	}
	if err := ctx.Err(); err != nil {
		return "", errors.Join(err, rollbackRerunContainer(svc, candidateID, candidateOwner, staged))
	}
	if err := svc.RenameContext(ctx, candidateID, targetName); err != nil {
		current, inspectErr := inspectRerunContainerForCleanup(svc, candidateID)
		if inspectErr != nil || normalizeRerunName(current.Name) != targetName {
			return "", errors.Join(fmt.Errorf("commit rerun candidate %s: %w", targetName, err), inspectErr, rollbackRerunContainer(svc, candidateID, candidateOwner, staged))
		}
		log.Printf("rerun candidate rename returned an error but was committed: container=%s error=%v", targetName, err)
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), rerunRollbackTimeout)
	defer cancel()
	if err := svc.RemoveContext(cleanupCtx, oldID, false, false); err != nil {
		_, inspectErr := svc.InspectContext(cleanupCtx, oldID)
		switch {
		case cerrdefs.IsNotFound(inspectErr):
			log.Printf("rerun old-container removal returned an error but the rollback copy is gone: container=%s error=%v", targetName, err)
		case inspectErr == nil:
			return candidateID, fmt.Errorf("remove rerun rollback container %s (%s): %w; healthy new container retained as %s", targetName, staged.rollbackName, err, targetName)
		default:
			return candidateID, fmt.Errorf("remove rerun rollback container %s (%s) returned %v and verification failed (%v); healthy new container retained as %s", targetName, staged.rollbackName, err, inspectErr, targetName)
		}
	}
	return candidateID, nil
}

func rollbackRerunContainer(svc rerunDockerService, candidateID, candidateOwner string, staged *stagedRerunContainer) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), rerunRollbackTimeout)
	defer cancel()
	var rollbackErrors []error
	if candidateID != "" {
		if err := removeRerunContainerForRollback(cleanupCtx, svc, candidateID, candidateOwner); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
	}
	if staged == nil {
		return errors.Join(rollbackErrors...)
	}
	current, inspectErr := svc.InspectContext(cleanupCtx, staged.oldID)
	if inspectErr != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("inspect rerun rollback container %s: %w", staged.oldID, inspectErr))
		return errors.Join(rollbackErrors...)
	}
	if normalizeRerunName(current.Name) != staged.targetName {
		if err := svc.RenameContext(cleanupCtx, staged.oldID, staged.targetName); err != nil {
			rechecked, inspectErr := svc.InspectContext(cleanupCtx, staged.oldID)
			if inspectErr != nil || normalizeRerunName(rechecked.Name) != staged.targetName {
				rollbackErrors = append(rollbackErrors, errors.Join(fmt.Errorf("restore rerun container name %s: %w", staged.targetName, err), inspectErr))
			}
		}
	}
	if staged.wasRunning && (current.State == nil || !current.State.Running) {
		if err := svc.StartContext(cleanupCtx, staged.oldID); err != nil {
			rechecked, inspectErr := svc.InspectContext(cleanupCtx, staged.oldID)
			if inspectErr != nil || rechecked.State == nil || !rechecked.State.Running {
				rollbackErrors = append(rollbackErrors, errors.Join(fmt.Errorf("restart original rerun container %s: %w", staged.targetName, err), inspectErr))
			}
		}
	}
	return errors.Join(rollbackErrors...)
}

func removeRerunContainerForRollback(ctx context.Context, svc rerunDockerService, candidateRef, candidateOwner string) error {
	id, exists, ownershipErr := inspectOwnedRerunCandidate(ctx, svc, candidateRef, candidateOwner)
	if ownershipErr != nil {
		return fmt.Errorf("verify rerun candidate ownership before rollback: %w", ownershipErr)
	}
	if !exists {
		return nil
	}
	err := svc.RemoveContext(ctx, id, true, false)
	if err == nil || cerrdefs.IsNotFound(err) {
		return nil
	}
	_, inspectErr := svc.InspectContext(ctx, id)
	if cerrdefs.IsNotFound(inspectErr) {
		return nil
	}
	if inspectErr != nil {
		return errors.Join(fmt.Errorf("remove rerun candidate %s: %w", id, err), fmt.Errorf("verify rerun candidate removal %s: %w", id, inspectErr))
	}
	return fmt.Errorf("remove rerun candidate %s: %w; candidate still exists after verification", id, err)
}

func inspectOwnedRerunCandidate(ctx context.Context, svc rerunDockerService, candidateRef, candidateOwner string) (string, bool, error) {
	if candidateOwner == "" {
		return "", false, errors.New("rerun candidate ownership marker is empty; refusing cleanup")
	}
	actual, err := svc.InspectContext(ctx, candidateRef)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect rerun candidate %s: %w", candidateRef, err)
	}
	if actual.ID == "" && normalizeRerunName(actual.Name) == "" {
		return "", false, nil
	}
	if actual.Config == nil || actual.Config.Labels[rerunCandidateOwnerLabel] != candidateOwner {
		return "", true, fmt.Errorf("container %s does not carry this rerun transaction's ownership label; refusing cleanup", candidateRef)
	}
	if actual.ID == "" {
		return "", true, fmt.Errorf("owned rerun candidate %s has no stable container ID; refusing cleanup", candidateRef)
	}
	return actual.ID, true, nil
}

func withRerunCandidateOwner(inspect container.InspectResponse, owner string) container.InspectResponse {
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
	config.Labels[rerunCandidateOwnerLabel] = owner
	inspect.Config = config
	return inspect
}

func newRerunCandidateOwner() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate rerun candidate ownership marker: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func inspectRerunContainerForCleanup(svc rerunDockerService, id string) (container.InspectResponse, error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), rerunRollbackTimeout)
	defer cancel()
	return svc.InspectContext(cleanupCtx, id)
}

func rerunStopRecoveryTimeout(inspect container.InspectResponse) time.Duration {
	timeout := rerunRollbackTimeout
	if inspect.Config == nil || inspect.Config.StopTimeout == nil || *inspect.Config.StopTimeout <= 0 {
		return timeout
	}
	candidate := time.Duration(*inspect.Config.StopTimeout)*time.Second + rerunStopRecoveryMargin
	if candidate > timeout {
		timeout = candidate
	}
	if timeout > rerunStopRecoveryMax {
		return rerunStopRecoveryMax
	}
	return timeout
}

func rerunStopResultUncertain(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func recoverRunningRerunContainer(svc rerunDockerService, id string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = rerunRollbackTimeout
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(rerunStatePollInterval)
	defer ticker.Stop()

	sawStopped := false
	var lastErr error
	for {
		current, err := svc.InspectContext(cleanupCtx, id)
		if err != nil {
			lastErr = fmt.Errorf("inspect original rerun container %s while reconciling stop: %w", id, err)
		} else if current.State != nil && current.State.Running {
			if sawStopped {
				return nil
			}
			lastErr = nil
		} else {
			sawStopped = true
			if startErr := svc.StartContext(cleanupCtx, id); startErr != nil {
				lastErr = fmt.Errorf("restart original rerun container %s after uncertain stop: %w", id, startErr)
			} else {
				lastErr = nil
			}
		}

		select {
		case <-cleanupCtx.Done():
			verifyCtx, verifyCancel := context.WithTimeout(context.Background(), rerunFinalInspectTimeout)
			current, inspectErr := svc.InspectContext(verifyCtx, id)
			verifyCancel()
			if inspectErr == nil && current.State != nil && current.State.Running {
				return nil
			}
			return errors.Join(lastErr, inspectErr, fmt.Errorf("restore original container %s to running state: %w", id, cleanupCtx.Err()))
		case <-ticker.C:
		}
	}
}

func restoreRunningRerunContainer(svc rerunDockerService, id string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), rerunRollbackTimeout)
	defer cancel()
	current, err := svc.InspectContext(cleanupCtx, id)
	if err != nil {
		return fmt.Errorf("inspect original rerun container %s after stop error: %w", id, err)
	}
	if current.State != nil && current.State.Running {
		return nil
	}
	startErr := svc.StartContext(cleanupCtx, id)
	rechecked, inspectErr := svc.InspectContext(cleanupCtx, id)
	if inspectErr == nil && rechecked.State != nil && rechecked.State.Running {
		return nil
	}
	if startErr == nil {
		startErr = errors.New("start returned successfully but the container is not running")
	}
	return errors.Join(fmt.Errorf("restart original rerun container %s after stop error: %w", id, startErr), inspectErr)
}

func rerunRequiresHealthy(inspect container.InspectResponse) bool {
	return inspect.Config != nil && inspect.Config.Healthcheck != nil && len(inspect.Config.Healthcheck.Test) > 0 &&
		!strings.EqualFold(strings.TrimSpace(inspect.Config.Healthcheck.Test[0]), "NONE")
}

func normalizeRerunName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func rerunTemporaryName(targetName, purpose string) string {
	name := normalizeRerunName(targetName)
	if len(name) > 180 {
		name = name[:180]
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("%s-dm-rerun-%s-%d", name, purpose, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-dm-rerun-%s-%s", name, purpose, hex.EncodeToString(random))
}
