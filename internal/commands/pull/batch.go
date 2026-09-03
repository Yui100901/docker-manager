package pull

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/audit"
	"docker-manager/internal/commandflags"
	"docker-manager/internal/parallel"
	"docker-manager/internal/runcontrol"
	"docker-manager/internal/sensitive"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	pullBatchStatusSuccess              = "success"
	pullBatchStatusFailed               = "failed"
	pullBatchStatusSkipped              = "skipped"
	pullBatchStateMaxBytes              = int64(64 << 20)
	pullBatchStateCommitProtocolVersion = 2
	pullBatchStateMarkerMaxBytes        = int64(4 << 10)
	pullBatchStateMarkerNameMaxBytes    = 1024
	pullBatchStateMarkerVersion         = 1
	pullBatchStateMarkerFileName        = ".dm-pull-state-untrusted.marker"
)

type PullBatchOptions struct {
	File                     string
	Images                   []string
	To                       string
	OutputDir                string
	Load                     bool
	DockerConfig             string
	PlainHTTP                bool
	DisableCredentialHelpers bool
	CredentialHelperTimeout  time.Duration
	AuthRealmAllowlist       []string
	RegistryPolicies         map[string]appconfig.RegistryPolicy
	Limits                   PullResourceLimits
	Concurrency              int
	Retries                  int
	SkipExisting             bool
	Resume                   bool
	StateFile                string
	ReportFile               string
	ProgressOutput           io.Writer
	platform                 targetPlatform
	policyOverrides          registryPolicyOverrides
	commandflags.FormatOptions
}

type PullBatchReport struct {
	GeneratedAt string            `json:"generated_at"`
	To          string            `json:"to"`
	OutputDir   string            `json:"output_dir,omitempty"`
	StateFile   string            `json:"state_file,omitempty"`
	Total       int               `json:"total"`
	Succeeded   int               `json:"succeeded"`
	Failed      int               `json:"failed"`
	Skipped     int               `json:"skipped"`
	Items       []PullBatchResult `json:"items"`
}

type PullBatchResult struct {
	Image       string `json:"image"`
	Target      string `json:"target,omitempty"`
	Status      string `json:"status"`
	Attempts    int    `json:"attempts,omitempty"`
	Message     string `json:"message,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	fingerprint *pullBatchResumeFingerprint
}

type pullBatchState struct {
	CommitProtocol int                           `json:"commit_protocol,omitempty"`
	UpdatedAt      string                        `json:"updated_at"`
	Items          map[string]pullBatchStateItem `json:"items"`
}

type pullBatchStateItem struct {
	Image       string                      `json:"image"`
	Target      string                      `json:"target,omitempty"`
	Status      string                      `json:"status"`
	Attempts    int                         `json:"attempts,omitempty"`
	Message     string                      `json:"message,omitempty"`
	StartedAt   string                      `json:"started_at,omitempty"`
	FinishedAt  string                      `json:"finished_at,omitempty"`
	Fingerprint *pullBatchResumeFingerprint `json:"fingerprint,omitempty"`
}

type pullBatchResumeFingerprint struct {
	Version       int    `json:"version"`
	ArchivePath   string `json:"archive_path"`
	ArchiveSize   int64  `json:"archive_size"`
	ArchiveDigest string `json:"archive_digest"`
	TargetOS      string `json:"target_os"`
	TargetArch    string `json:"target_arch"`
	DockerLoad    bool   `json:"docker_load"`
}

type pullBatchStateUntrustedMarker struct {
	Version      int    `json:"version"`
	StateNameHex string `json:"state_name_hex"`
	Transaction  string `json:"transaction"`
}

type pullBatchStateMarkerCommit struct {
	marker pullBatchStateUntrustedMarker
	info   os.FileInfo
}

type pullBatchPlanItem struct {
	Image       string
	Target      string
	ArchivePath string
}

type pullBatchPathClaim struct {
	Owner string
	Path  string
	Info  os.FileInfo
}

type pullBatchLifecycleLock struct {
	root       *os.Root
	anchor     *os.File
	anchorInfo os.FileInfo
	marker     *os.File
	markerInfo os.FileInfo
	markerName string
	directory  string
	parentInfo os.FileInfo
}

type pullBatchFunc func(image string, opts PullOptions) error
type pullBatchExistsFunc func(ctx context.Context, image, target string, opts PullOptions) (bool, error)

type pullBatchPersistence struct {
	writeState       func(context.Context, string, pullBatchState) error
	writeReport      func(context.Context, string, PullBatchReport) error
	writeStateMarker func(context.Context, string, pullBatchStateUntrustedMarker) (pullBatchStateMarkerCommit, error)
	clearStateMarker func(context.Context, string, pullBatchStateMarkerCommit) error
}

func runPullBatch(ctx context.Context, runner *PullRunner, opts PullBatchOptions) (PullBatchReport, error) {
	opts.platform = runner.platform
	return runPullBatchWithDeps(ctx, opts, runner.getImage, runner.targetManifestExists)
}

func runPullBatchWithDeps(ctx context.Context, opts PullBatchOptions, pull pullBatchFunc, exists pullBatchExistsFunc) (PullBatchReport, error) {
	return runPullBatchWithDepsAndPersistence(ctx, opts, pull, exists, pullBatchPersistence{
		writeState:  writePullBatchStateWhileLocked,
		writeReport: writePullBatchReportWhileLocked,
	})
}

func runPullBatchWithDepsAndPersistence(ctx context.Context, opts PullBatchOptions, pull pullBatchFunc, exists pullBatchExistsFunc, persistence pullBatchPersistence) (PullBatchReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if persistence.writeState == nil {
		persistence.writeState = writePullBatchStateWhileLocked
	}
	if persistence.writeReport == nil {
		persistence.writeReport = writePullBatchReportWhileLocked
	}
	if persistence.writeStateMarker == nil {
		persistence.writeStateMarker = writePullBatchStateUntrustedMarker
	}
	if persistence.clearStateMarker == nil {
		persistence.clearStateMarker = clearPullBatchStateUntrustedMarker
	}
	images, err := loadPullBatchImages(opts.Images, opts.File)
	if err != nil {
		return PullBatchReport{}, err
	}
	if len(images) == 0 {
		return PullBatchReport{}, fmt.Errorf("pull 需要至少一个镜像，可通过位置参数或 --file 指定")
	}
	// Reserve the complete de-duplicated batch before starting any network or
	// Docker work. This makes --max-items fail closed and avoids partial pulls.
	if err := runcontrol.CheckItems(ctx, "image", len(images)); err != nil {
		return PullBatchReport{}, err
	}
	if opts.To != "" && len(images) > 1 && isTaggedImageRef(opts.To) {
		return PullBatchReport{}, fmt.Errorf("--to 使用完整镜像名时只能同步单个镜像；批量同步请使用 registry 或 namespace 前缀")
	}
	if opts.SkipExisting && opts.To == "" {
		return PullBatchReport{}, fmt.Errorf("--skip-existing 需要配合 --to 使用")
	}
	if opts.Concurrency <= 0 {
		return PullBatchReport{}, fmt.Errorf("--concurrency 必须大于 0")
	}
	if opts.Retries < 0 {
		return PullBatchReport{}, fmt.Errorf("--retries 不能小于 0")
	}
	if err := validateAuthRealmAllowlist(opts.AuthRealmAllowlist); err != nil {
		return PullBatchReport{}, err
	}
	if err := validatePullResourceLimits(opts.Limits); err != nil {
		return PullBatchReport{}, err
	}
	plan, err := preparePullBatchPlan(&opts, images)
	if err != nil {
		return PullBatchReport{}, err
	}
	if err := authorizePullBatchFiles(ctx, opts.StateFile, opts.ReportFile); err != nil {
		return PullBatchReport{}, fmt.Errorf("审计授权失败，未写入批量状态或报告: %w", err)
	}
	lifecycleLocks, err := acquirePullBatchLifecycleLocks(
		ctx,
		opts.StateFile,
		opts.ReportFile,
		pullBatchArchiveLockScope(opts.OutputDir),
	)
	if err != nil {
		return PullBatchReport{}, err
	}
	defer releasePullBatchLifecycleLocks(lifecycleLocks)
	ctx = withPullBatchLifecycleLocks(ctx, lifecycleLocks)
	if err := validatePullBatchStateUntrustedMarker(opts.StateFile); err != nil {
		return PullBatchReport{}, fmt.Errorf("检查状态文件提交标记失败: %w", err)
	}
	progressOutput := opts.ProgressOutput
	if progressOutput == nil {
		progressOutput = io.Discard
	}

	state := pullBatchState{Items: map[string]pullBatchStateItem{}}
	if opts.Resume {
		loaded, err := readPullBatchState(opts.StateFile)
		if err != nil {
			return PullBatchReport{}, err
		}
		state = loaded
	}
	if state.Items == nil {
		state.Items = map[string]pullBatchStateItem{}
	}
	resumeState := pullBatchState{Items: copyPullBatchStateItems(state.Items)}

	report := PullBatchReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		To:          opts.To,
		OutputDir:   opts.OutputDir,
		StateFile:   opts.StateFile,
		Total:       len(images),
		Items:       make([]PullBatchResult, len(images)),
	}
	for index := range plan {
		report.Items[index] = PullBatchResult{
			Image:   plan[index].Image,
			Target:  plan[index].Target,
			Status:  pullBatchStatusFailed,
			Message: "批量项未执行",
		}
	}

	var mu sync.Mutex
	var statePersistenceErrs []error
	parallel.ForEachIndex(ctx, len(images), opts.Concurrency, func(ctx context.Context, idx int) {
		result := runPlannedPullBatchItem(ctx, plan[idx], opts, resumeState, pull, exists, progressOutput)
		mu.Lock()
		defer mu.Unlock()
		report.Items[idx] = result
		updatePullBatchReportCounts(&report)
		updatePullBatchStateItem(state, result)
		if err := persistPullBatchState(ctx, opts.StateFile, state, persistence.writeState, persistence.writeStateMarker, persistence.clearStateMarker); err != nil {
			statePersistenceErrs = append(statePersistenceErrs, fmt.Errorf("写入批量状态文件失败: %w", err))
			report.Items[idx].Status = pullBatchStatusFailed
			message := "写入状态文件失败: " + err.Error()
			if report.Items[idx].Message != "" {
				message = report.Items[idx].Message + "; " + message
			}
			report.Items[idx].Message = message
			updatePullBatchStateItem(state, report.Items[idx])
		}
	})

	contextErr := ctx.Err()
	if contextErr != nil {
		for index := range report.Items {
			if report.Items[index].StartedAt == "" {
				report.Items[index].Message = "批量项未执行: " + contextErr.Error()
			}
		}
	}
	updatePullBatchReportCounts(&report)
	var reportErr error
	if opts.ReportFile != "" && !errors.Is(contextErr, context.Canceled) {
		if err := persistence.writeReport(ctx, opts.ReportFile, report); err != nil {
			reportErr = fmt.Errorf("写入批量报告失败: %w", err)
		}
	}
	if afterReportErr := ctx.Err(); contextErr == nil {
		contextErr = afterReportErr
	}
	persistenceErr := errors.Join(statePersistenceErrs...)
	if contextErr != nil || persistenceErr != nil || reportErr != nil {
		return report, errors.Join(contextErr, persistenceErr, reportErr)
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("pull 批量完成但存在失败项: total=%d success=%d skipped=%d failed=%d", report.Total, report.Succeeded, report.Skipped, report.Failed)
	}
	return report, nil
}

func preparePullBatchPlan(opts *PullBatchOptions, images []string) ([]pullBatchPlanItem, error) {
	if opts == nil {
		return nil, errors.New("pull 批量选项不能为空")
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "."
	}
	outputDir, err := normalizePullBatchPath(opts.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("解析批量输出目录失败: %w", err)
	}
	opts.OutputDir = outputDir
	if opts.StateFile == "" {
		opts.StateFile = filepath.Join(outputDir, "pull-state.json")
	}
	opts.StateFile, err = normalizePullBatchPath(opts.StateFile)
	if err != nil {
		return nil, fmt.Errorf("解析批量状态文件路径失败: %w", err)
	}
	if opts.ReportFile != "" {
		opts.ReportFile, err = normalizePullBatchPath(opts.ReportFile)
		if err != nil {
			return nil, fmt.Errorf("解析批量报告文件路径失败: %w", err)
		}
	}
	claimedPaths := make(map[string]pullBatchPathClaim, len(images)+10)
	lifecycleDirectories := []string{outputDir, filepath.Dir(opts.StateFile)}
	if opts.ReportFile != "" {
		lifecycleDirectories = append(lifecycleDirectories, filepath.Dir(opts.ReportFile))
	}
	claimedLifecycleLocks := make([]string, 0, len(lifecycleDirectories))
	for _, directory := range lifecycleDirectories {
		lockPath := pullBatchLifecycleLockPath(directory)
		alreadyClaimed := false
		for _, claimedLock := range claimedLifecycleLocks {
			if pullBatchPathsEqual(claimedLock, lockPath) {
				alreadyClaimed = true
				break
			}
		}
		if alreadyClaimed {
			continue
		}
		claimedLifecycleLocks = append(claimedLifecycleLocks, lockPath)
		if err := claimPullBatchPath(claimedPaths, "输出目录生命周期锁", lockPath); err != nil {
			return nil, err
		}
	}
	if err := claimPullBatchPath(claimedPaths, "状态文件", opts.StateFile); err != nil {
		return nil, err
	}
	if err := claimPullBatchPath(claimedPaths, "状态文件锁", pullAtomicJSONLockPath(opts.StateFile)); err != nil {
		return nil, err
	}
	stateUntrustedMarker := pullBatchStateUntrustedMarkerPath(opts.StateFile)
	if err := claimPullBatchPath(claimedPaths, "状态文件不可信标记", stateUntrustedMarker); err != nil {
		return nil, err
	}
	if err := claimPullBatchPath(claimedPaths, "状态文件不可信标记锁", pullAtomicJSONLockPath(stateUntrustedMarker)); err != nil {
		return nil, err
	}
	if opts.ReportFile != "" {
		if err := claimPullBatchPath(claimedPaths, "报告文件", opts.ReportFile); err != nil {
			return nil, err
		}
		if err := claimPullBatchPath(claimedPaths, "报告文件锁", pullAtomicJSONLockPath(opts.ReportFile)); err != nil {
			return nil, err
		}
	}

	plan := make([]pullBatchPlanItem, len(images))
	targetOwners := make(map[string]string, len(images))
	for index, imageName := range images {
		info, parseErr := parseImageInfo(imageName)
		if parseErr != nil {
			return nil, fmt.Errorf("镜像名称 %q 解析失败: %w", imageName, parseErr)
		}
		archivePath, resolveErr := resolveOutputFile(info, PullOptions{OutputDir: outputDir})
		if resolveErr != nil {
			return nil, fmt.Errorf("解析镜像 %q 的归档路径失败: %w", imageName, resolveErr)
		}
		archivePath, resolveErr = normalizePullBatchPath(archivePath)
		if resolveErr != nil {
			return nil, fmt.Errorf("解析镜像 %q 的归档绝对路径失败: %w", imageName, resolveErr)
		}
		if err := claimPullBatchPath(claimedPaths, fmt.Sprintf("镜像 %q 的归档", imageName), archivePath); err != nil {
			return nil, err
		}

		target := ""
		if opts.To != "" {
			target, resolveErr = resolvePushTarget(info, opts.To)
			if resolveErr != nil {
				return nil, fmt.Errorf("解析镜像 %q 的 --to 目标失败: %w", imageName, resolveErr)
			}
			if previous, exists := targetOwners[target]; exists {
				return nil, fmt.Errorf("批量 --to 目标冲突: 镜像 %q 与 %q 均解析为 %q", previous, imageName, target)
			}
			targetOwners[target] = imageName
		}
		plan[index] = pullBatchPlanItem{Image: imageName, Target: target, ArchivePath: archivePath}
	}
	if err := rejectPullInternalOutputName(opts.StateFile); err != nil {
		return nil, fmt.Errorf("批量状态文件路径无效: %w", err)
	}
	if opts.ReportFile != "" {
		if err := rejectPullInternalOutputName(opts.ReportFile); err != nil {
			return nil, fmt.Errorf("批量报告文件路径无效: %w", err)
		}
	}
	for _, item := range plan {
		if err := rejectPullInternalOutputName(item.ArchivePath); err != nil {
			return nil, fmt.Errorf("镜像 %q 的归档路径无效: %w", item.Image, err)
		}
	}

	// This validation is read-only. It makes unsafe existing destinations fail
	// before any registry request, Docker mutation, or metadata/archive write.
	if err := validatePullBatchPathClaims(claimedPaths); err != nil {
		return nil, err
	}
	return plan, nil
}

func normalizePullBatchPath(path string) (string, error) {
	if err := validatePullBatchPlatformPath(path); err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func pullBatchArchiveLockScope(outputDir string) string {
	return filepath.Join(outputDir, ".dm-pull-batch-archive-scope")
}

func rejectPullInternalOutputName(path string) error {
	if pullInternalOutputNameReserved(filepath.Base(path)) {
		return fmt.Errorf("输出文件名使用了 pull 内部保留命名空间: %s", filepath.Base(path))
	}
	return rejectPullInternalOutputAlias(path)
}

func pullInternalOutputNameReserved(name string) bool {
	name = strings.ToLower(name)
	return strings.HasPrefix(name, ".dm-pull-") || strings.HasPrefix(name, ".docker-manager-pull-")
}

func pullBatchLifecycleLockPath(directory string) string {
	return pullAtomicJSONLockPath(pullBatchArchiveLockScope(directory))
}

func claimPullBatchPath(claimed map[string]pullBatchPathClaim, owner, path string) error {
	for _, previous := range claimed {
		switch {
		case pullBatchPathsEqual(previous.Path, path):
			return fmt.Errorf("批量输出路径冲突: %s 与 %s 均指向 %s", previous.Owner, owner, path)
		case pullBatchPathContains(previous.Path, path):
			return fmt.Errorf("批量输出路径拓扑冲突: %s %s 是 %s %s 的祖先路径", previous.Owner, previous.Path, owner, path)
		case pullBatchPathContains(path, previous.Path):
			return fmt.Errorf("批量输出路径拓扑冲突: %s %s 是 %s %s 的祖先路径", owner, path, previous.Owner, previous.Path)
		}
	}
	claimed[filepath.Clean(path)] = pullBatchPathClaim{Owner: owner, Path: path}
	return nil
}

func validatePullBatchPathClaims(claimed map[string]pullBatchPathClaim) error {
	if err := validatePullBatchPlatformPathClaims(claimed); err != nil {
		return err
	}
	claims := make([]pullBatchPathClaim, 0, len(claimed))
	for key, claim := range claimed {
		if err := rejectPullArchiveOutputLinks(claim.Path); err != nil {
			return fmt.Errorf("批量输出路径不安全: %w", err)
		}
		info, err := pullPathLstatNoFollow(claim.Path)
		if err == nil {
			claim.Info = info
			claimed[key] = claim
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("检查批量输出路径 %s 失败: %w", claim.Path, err)
		}
		claims = append(claims, claim)
	}
	for left := 0; left < len(claims); left++ {
		if claims[left].Info == nil {
			continue
		}
		for right := left + 1; right < len(claims); right++ {
			if claims[right].Info != nil && os.SameFile(claims[left].Info, claims[right].Info) {
				return fmt.Errorf("批量输出文件身份冲突: %s 与 %s 是同一文件 (%s, %s)",
					claims[left].Owner, claims[right].Owner, claims[left].Path, claims[right].Path)
			}
		}
	}
	return nil
}

func acquirePullBatchLifecycleLocks(ctx context.Context, paths ...string) ([]pullBatchLifecycleLock, error) {
	directories := make([]string, 0, len(paths))
	seen := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		normalized, err := normalizePullBatchPath(path)
		if err != nil {
			return nil, fmt.Errorf("解析批量元数据锁路径失败: %w", err)
		}
		directory := filepath.Dir(normalized)
		alreadySeen := false
		for _, seenDirectory := range seen {
			if pullBatchPathsEqual(seenDirectory, directory) {
				alreadySeen = true
				break
			}
		}
		if alreadySeen {
			continue
		}
		seen = append(seen, directory)
		directories = append(directories, directory)
	}
	sort.Slice(directories, func(left, right int) bool {
		return pullBatchPathLess(directories[left], directories[right])
	})

	locks := make([]pullBatchLifecycleLock, 0, len(directories))
	cleanup := func() { releasePullBatchLifecycleLocks(locks) }
	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			cleanup()
			return nil, err
		}
		scopePath := pullBatchArchiveLockScope(directory)
		root, outputName, _, parentInfo, err := openPullArchiveOutput(scopePath)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("准备 pull 输出目录生命周期锁失败: %w", err)
		}
		lockName := pullAtomicJSONLockName(outputName)
		anchor, anchorInfo, err := openPullBatchLifecycleAnchor(root, lockName)
		if err != nil {
			_ = root.Close()
			cleanup()
			return nil, fmt.Errorf("打开 pull 输出目录生命周期锁失败: %w", err)
		}
		acquired, lockErr := tryLockPullAtomicJSONFile(anchor)
		if lockErr == nil && acquired {
			var marker *os.File
			var markerInfo os.FileInfo
			marker, markerInfo, lockErr = attachPullBatchLifecycleMarker(root, lockName, anchor, anchorInfo)
			if lockErr == nil {
				candidate := pullBatchLifecycleLock{
					root:       root,
					anchor:     anchor,
					anchorInfo: anchorInfo,
					marker:     marker,
					markerInfo: markerInfo,
					markerName: lockName,
					directory:  directory,
					parentInfo: parentInfo,
				}
				lockErr = verifyPullBatchLifecycleLock(&candidate)
				if lockErr == nil {
					locks = append(locks, candidate)
					continue
				}
			}
			if marker != nil && marker != anchor {
				_ = marker.Close()
			}
		}
		if acquired {
			_ = unlockPullAtomicJSONFile(anchor)
		}
		closeErr := errors.Join(anchor.Close(), root.Close())
		cleanup()
		if lockErr != nil || closeErr != nil {
			return nil, fmt.Errorf("获取 pull 输出目录生命周期锁失败: %w", errors.Join(lockErr, closeErr))
		}
		return nil, fmt.Errorf("pull 输出目录正在被另一进程使用，未执行任何镜像操作: %s", directory)
	}
	return locks, nil
}

func releasePullBatchLifecycleLocks(locks []pullBatchLifecycleLock) {
	for index := len(locks) - 1; index >= 0; index-- {
		lock := &locks[index]
		_ = unlockPullAtomicJSONFile(lock.anchor)
		if lock.marker != nil && lock.marker != lock.anchor {
			_ = lock.marker.Close()
		}
		_ = lock.anchor.Close()
		_ = lock.root.Close()
	}
}

type pullBatchLifecycleContextKey struct{}

func withPullBatchLifecycleLocks(ctx context.Context, locks []pullBatchLifecycleLock) context.Context {
	if len(locks) == 0 {
		return ctx
	}
	existing, _ := ctx.Value(pullBatchLifecycleContextKey{}).([]pullBatchLifecycleLock)
	combined := make([]pullBatchLifecycleLock, 0, len(existing)+len(locks))
	combined = append(combined, existing...)
	combined = append(combined, locks...)
	return context.WithValue(ctx, pullBatchLifecycleContextKey{}, combined)
}

func verifyPullBatchLifecyclePath(ctx context.Context, outputPath string) error {
	normalized, err := normalizePullBatchPath(outputPath)
	if err != nil {
		return err
	}
	directory := filepath.Dir(normalized)
	locks, _ := ctx.Value(pullBatchLifecycleContextKey{}).([]pullBatchLifecycleLock)
	for index := range locks {
		if !pullBatchPathsEqual(locks[index].directory, directory) {
			continue
		}
		return verifyPullBatchLifecycleLock(&locks[index])
	}
	return fmt.Errorf("pull 输出目录生命周期锁未持有: %s", filepath.Dir(normalized))
}

func holdPullArchiveLifecycle(ctx context.Context, outputPath string) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := normalizePullBatchPath(outputPath)
	if err != nil {
		return ctx, nil, err
	}
	if err := rejectPullInternalOutputName(normalized); err != nil {
		return ctx, nil, err
	}
	directory := filepath.Dir(normalized)
	if pullBatchPathsEqual(normalized, pullBatchLifecycleLockPath(directory)) {
		return ctx, nil, fmt.Errorf("归档输出与 pull 生命周期锁路径冲突: %s", normalized)
	}
	existing, _ := ctx.Value(pullBatchLifecycleContextKey{}).([]pullBatchLifecycleLock)
	for index := range existing {
		if !pullBatchPathsEqual(existing[index].directory, directory) {
			continue
		}
		if err := verifyPullBatchLifecycleLock(&existing[index]); err != nil {
			return ctx, nil, err
		}
		if err := rejectPullArchiveLifecycleAlias(normalized, &existing[index]); err != nil {
			return ctx, nil, err
		}
		return ctx, func() {}, nil
	}
	locks, err := acquirePullBatchLifecycleLocks(ctx, normalized)
	if err != nil {
		return ctx, nil, err
	}
	lockedContext := withPullBatchLifecycleLocks(ctx, locks)
	if err := rejectPullArchiveLifecycleAlias(normalized, &locks[0]); err != nil {
		releasePullBatchLifecycleLocks(locks)
		return ctx, nil, err
	}
	return lockedContext, func() { releasePullBatchLifecycleLocks(locks) }, nil
}

func rejectPullArchiveLifecycleAlias(outputPath string, lock *pullBatchLifecycleLock) error {
	if lock == nil || lock.root == nil || !pullBatchPathsEqual(filepath.Dir(outputPath), lock.directory) {
		return fmt.Errorf("pull 生命周期锁与归档输出目录不匹配: %s", outputPath)
	}
	outputName := filepath.Base(outputPath)
	if !filepath.IsLocal(outputName) {
		return fmt.Errorf("归档输出文件名无效: %s", outputPath)
	}
	outputInfo, outputErr := lock.root.Lstat(outputName)
	if os.IsNotExist(outputErr) {
		return nil
	}
	if outputErr != nil {
		return outputErr
	}
	markerInfo, markerErr := lock.root.Lstat(lock.markerName)
	if markerErr != nil {
		return markerErr
	}
	if lock == nil || lock.markerInfo == nil || !safePullAtomicJSONLockIdentity(lock.markerInfo, markerInfo) {
		return fmt.Errorf("pull 生命周期锁标记在归档别名检查期间发生变化")
	}
	if os.SameFile(outputInfo, markerInfo) {
		return fmt.Errorf("归档输出与 pull 生命周期锁文件身份冲突: %s", outputPath)
	}
	return nil
}

func authorizePullBatchFiles(ctx context.Context, stateFile, reportFile string) error {
	session := audit.FromContext(ctx)
	if session == nil {
		return nil
	}
	candidates := make([]audit.CandidateInput, 0, 2)
	if stateFile != "" {
		path := filepath.Clean(stateFile)
		candidates = append(candidates, audit.CandidateInput{
			Kind:       "pull-state",
			Action:     "write",
			Identifier: path,
			Display:    path,
		})
	}
	if reportFile != "" {
		path := filepath.Clean(reportFile)
		candidates = append(candidates, audit.CandidateInput{
			Kind:       "pull-report",
			Action:     "write",
			Identifier: path,
			Display:    path,
		})
	}
	if len(candidates) == 0 {
		return nil
	}
	_, err := session.AuthorizeMutation(ctx, audit.MutationRequest{
		Scope:        audit.MutationFilesystem,
		Confirmation: audit.Confirmation{Provided: true, Mechanism: "pull-batch-files"},
		Candidates:   candidates,
	})
	return err
}

func updatePullBatchStateItem(state pullBatchState, result PullBatchResult) {
	if result.Status == pullBatchStatusSkipped {
		if existing, ok := state.Items[result.Image]; ok && existing.Status == pullBatchStatusSuccess && existing.Target == result.Target {
			return
		}
	}
	var fingerprint *pullBatchResumeFingerprint
	if result.Status == pullBatchStatusSuccess {
		fingerprint = clonePullBatchResumeFingerprint(result.fingerprint)
	}
	state.Items[result.Image] = pullBatchStateItem{
		Image:       result.Image,
		Target:      result.Target,
		Status:      result.Status,
		Attempts:    result.Attempts,
		Message:     result.Message,
		StartedAt:   result.StartedAt,
		FinishedAt:  result.FinishedAt,
		Fingerprint: fingerprint,
	}
}

func runPullBatchItem(ctx context.Context, imageName string, opts PullBatchOptions, state pullBatchState, pull pullBatchFunc, exists pullBatchExistsFunc, progressOutput io.Writer) PullBatchResult {
	plan := pullBatchPlanItem{Image: imageName}
	info, err := parseImageInfo(imageName)
	if err != nil {
		now := time.Now().Format(time.RFC3339)
		return PullBatchResult{Image: imageName, Status: pullBatchStatusFailed, Message: "镜像名称解析失败: " + err.Error(), StartedAt: now, FinishedAt: now}
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "."
	}
	plan.ArchivePath, err = resolveOutputFile(info, PullOptions{OutputDir: opts.OutputDir})
	if err != nil {
		now := time.Now().Format(time.RFC3339)
		return PullBatchResult{Image: imageName, Status: pullBatchStatusFailed, Message: "解析归档路径失败: " + err.Error(), StartedAt: now, FinishedAt: now}
	}
	if opts.To != "" {
		plan.Target, err = resolvePushTarget(info, opts.To)
		if err != nil {
			now := time.Now().Format(time.RFC3339)
			return PullBatchResult{Image: imageName, Status: pullBatchStatusFailed, Message: err.Error(), StartedAt: now, FinishedAt: now}
		}
	}
	return runPlannedPullBatchItem(ctx, plan, opts, state, pull, exists, progressOutput)
}

func runPlannedPullBatchItem(ctx context.Context, plan pullBatchPlanItem, opts PullBatchOptions, state pullBatchState, pull pullBatchFunc, exists pullBatchExistsFunc, progressOutput io.Writer) PullBatchResult {
	startedAt := time.Now().Format(time.RFC3339)
	result := PullBatchResult{Image: plan.Image, Target: plan.Target, Status: pullBatchStatusFailed, StartedAt: startedAt}
	if opts.Resume {
		if item, ok := state.Items[plan.Image]; ok && item.Status == pullBatchStatusSuccess && item.Target == plan.Target {
			matches, err := pullBatchResumeFingerprintMatches(ctx, item, plan, opts)
			if err != nil {
				result.Message = "验证断点归档失败: " + err.Error()
				result.FinishedAt = time.Now().Format(time.RFC3339)
				return result
			}
			if matches {
				result.Status = pullBatchStatusSkipped
				result.Message = "状态文件与归档指纹均已成功，跳过"
				result.FinishedAt = time.Now().Format(time.RFC3339)
				return result
			}
		}
	}
	pullOpts := PullOptions{
		Context:                  ctx,
		Output:                   plan.ArchivePath,
		OutputDir:                opts.OutputDir,
		Load:                     opts.Load,
		To:                       opts.To,
		DockerConfig:             opts.DockerConfig,
		PlainHTTP:                opts.PlainHTTP,
		DisableCredentialHelpers: opts.DisableCredentialHelpers,
		CredentialHelperTimeout:  opts.CredentialHelperTimeout,
		AuthRealmAllowlist:       append([]string(nil), opts.AuthRealmAllowlist...),
		RegistryPolicies:         cloneRegistryPolicies(opts.RegistryPolicies),
		Limits:                   effectivePullResourceLimits(opts.Limits),
		ProgressOutput:           progressOutput,
		policyOverrides:          opts.policyOverrides,
	}
	itemCtx, cancel := context.WithTimeout(ctx, pullOpts.Limits.TotalTimeout)
	defer cancel()
	pullOpts.Context = itemCtx
	if opts.SkipExisting {
		found, err := exists(itemCtx, plan.Image, plan.Target, pullOpts)
		if err != nil {
			result.Message = "检查目标 manifest 失败: " + err.Error()
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
		if found {
			result.Status = pullBatchStatusSkipped
			result.Message = "目标 registry 已存在，跳过"
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
	}

	var lastErr error
	maxAttempts := opts.Retries + 1
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result.Attempts = attempt
		if err := itemCtx.Err(); err != nil {
			result.Message = err.Error()
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
		if err := pull(plan.Image, pullOpts); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				result.Message = err.Error()
				result.FinishedAt = time.Now().Format(time.RFC3339)
				return result
			}
			lastErr = err
			continue
		}
		fingerprint, err := buildPullBatchResumeFingerprint(itemCtx, plan, opts)
		if err != nil {
			result.Message = "拉取回调成功但归档完整性校验失败: " + err.Error()
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
		result.Status = pullBatchStatusSuccess
		result.fingerprint = fingerprint
		result.Message = "同步成功"
		result.FinishedAt = time.Now().Format(time.RFC3339)
		return result
	}
	if lastErr != nil {
		result.Message = lastErr.Error()
	}
	result.FinishedAt = time.Now().Format(time.RFC3339)
	return result
}

func (r *PullRunner) targetManifestExists(ctx context.Context, imageName, target string, opts PullOptions) (bool, error) {
	info, err := parseImageInfo(target)
	if err != nil {
		return false, err
	}
	targetRunner, targetOpts, err := r.bindRegistryPolicy(info.Registry, registryCredentialPush, opts)
	if err != nil {
		return false, fmt.Errorf("应用目标 registry %s 策略失败: %w", info.Registry, err)
	}
	targetOpts.PlainHTTP = pushTargetUsesPlainHTTP(targetOpts)
	headers := map[string]string{
		"Accept": strings.Join([]string{
			dockerManifestV2,
			dockerManifestListV2,
			ocispec.MediaTypeImageManifest,
			ocispec.MediaTypeImageIndex,
		}, ", "),
	}
	limit := effectivePullResourceLimits(opts.Limits).ManifestBytes
	_, _, err = targetRunner.fetchRegistryBytesOnceLimit(ctx, registryAPIURL(targetOpts, info, "manifests", getReference(info)), headers, nil, info, targetOpts, nil, limit)
	if err == nil {
		return true, nil
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func resolvePullBatchTarget(imageName, to string) (string, error) {
	info, err := parseImageInfo(imageName)
	if err != nil {
		return "", fmt.Errorf("镜像名称解析失败: %w", err)
	}
	return resolvePushTarget(info, to)
}

func loadPullBatchImages(args []string, file string) ([]string, error) {
	var values []string
	values = append(values, args...)
	if file != "" {
		fromFile, err := readPullBatchImageFile(file)
		if err != nil {
			return nil, err
		}
		values = append(values, fromFile...)
	}
	return uniquePullBatchImages(values), nil
}

func readPullBatchImageFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取镜像列表失败: %w", err)
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "警告: 关闭镜像列表文件 %s 失败: %v\n", path, cerr)
		}
	}()
	var images []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		images = append(images, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取镜像列表失败: %w", err)
	}
	return images, nil
}

func uniquePullBatchImages(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.HasPrefix(value, "#") || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func readPullBatchState(path string) (pullBatchState, error) {
	state := pullBatchState{Items: map[string]pullBatchStateItem{}}
	normalizedPath, err := normalizePullBatchPath(path)
	if err != nil {
		return state, fmt.Errorf("解析状态文件路径失败: %w", err)
	}
	root, outputName, outputPath, _, err := openPullArchiveOutput(normalizedPath)
	if err != nil {
		return state, fmt.Errorf("安全打开状态文件父目录失败: %w", err)
	}
	defer root.Close()
	marker, _, err := readPullBatchStateUntrustedMarker(root, outputName)
	if err != nil {
		return state, fmt.Errorf("检查状态文件提交标记失败: %w", err)
	}
	commitUntrusted := marker != nil
	initial, exists, err := snapshotPullJSONOutput(root, outputName)
	if err != nil {
		return state, fmt.Errorf("检查状态文件失败: %w", err)
	}
	if !exists {
		return state, nil
	}
	if initial.Size() > pullBatchStateMaxBytes {
		return state, fmt.Errorf("状态文件超过 %d 字节上限: %s", pullBatchStateMaxBytes, outputPath)
	}
	file, err := openPullBatchStateFile(root, outputName)
	if err != nil {
		return state, fmt.Errorf("打开状态文件失败: %w", err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !safePullBatchFileIdentity(initial, opened) {
		_ = file.Close()
		return state, errors.Join(statErr, fmt.Errorf("状态文件在打开时发生变化: %s", outputPath))
	}
	data, readErr := io.ReadAll(io.LimitReader(file, pullBatchStateMaxBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return state, fmt.Errorf("读取状态文件失败: %w", err)
	}
	if int64(len(data)) > pullBatchStateMaxBytes {
		return state, fmt.Errorf("状态文件超过 %d 字节上限: %s", pullBatchStateMaxBytes, outputPath)
	}
	if err := verifyPullJSONOutputUnchanged(root, outputName, outputPath, initial, true); err != nil {
		return state, fmt.Errorf("读取状态文件时身份校验失败: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("解析状态文件失败: %w", err)
	}
	if state.Items == nil {
		state.Items = map[string]pullBatchStateItem{}
	}
	if commitUntrusted || state.CommitProtocol != pullBatchStateCommitProtocolVersion {
		for key, item := range state.Items {
			if item.Status != pullBatchStatusSuccess {
				continue
			}
			item.Status = pullBatchStatusFailed
			item.Message = "上次状态文件提交未确认，需要重新执行"
			item.Fingerprint = nil
			state.Items[key] = item
		}
	}
	return state, nil
}

func copyPullBatchStateItems(items map[string]pullBatchStateItem) map[string]pullBatchStateItem {
	copied := map[string]pullBatchStateItem{}
	for key, value := range items {
		value.Fingerprint = clonePullBatchResumeFingerprint(value.Fingerprint)
		copied[key] = value
	}
	return copied
}

func writePullBatchState(path string, state pullBatchState) error {
	if err := rejectPullInternalOutputName(path); err != nil {
		return err
	}
	locks, err := acquirePullBatchLifecycleLocks(context.Background(), path)
	if err != nil {
		return err
	}
	defer releasePullBatchLifecycleLocks(locks)
	ctx := withPullBatchLifecycleLocks(context.Background(), locks)
	return persistPullBatchState(ctx, path, state, writePullBatchStateWhileLocked, writePullBatchStateUntrustedMarker, clearPullBatchStateUntrustedMarker)
}

func writePullBatchStateWhileLocked(ctx context.Context, path string, state pullBatchState) error {
	return writePullBatchStateWhileLockedWithSync(ctx, path, state, syncPullOutputDirectory)
}

func writePullBatchStateWhileLockedWithSync(ctx context.Context, path string, state pullBatchState, syncDirectory func(*os.Root) error) error {
	data, err := marshalPullBatchState(state)
	if err != nil {
		return err
	}
	return writeAtomicJSONWhileLockedWithSync(ctx, path, data, syncDirectory)
}

func persistPullBatchState(
	ctx context.Context,
	path string,
	state pullBatchState,
	writeState func(context.Context, string, pullBatchState) error,
	writeMarker func(context.Context, string, pullBatchStateUntrustedMarker) (pullBatchStateMarkerCommit, error),
	clearMarker func(context.Context, string, pullBatchStateMarkerCommit) error,
) error {
	marker, err := newPullBatchStateUntrustedMarker(path)
	if err != nil {
		return fmt.Errorf("生成状态文件不可信标记失败: %w", err)
	}
	commit, err := writeMarker(ctx, path, marker)
	if err != nil {
		return fmt.Errorf("创建状态文件不可信标记失败: %w", err)
	}
	if err := writeState(ctx, path, state); err != nil {
		return err
	}
	if err := clearMarker(ctx, path, commit); err != nil {
		return fmt.Errorf("清理状态文件不可信标记失败: %w", err)
	}
	return nil
}

func writePullBatchStateUntrustedMarker(ctx context.Context, statePath string, marker pullBatchStateUntrustedMarker) (pullBatchStateMarkerCommit, error) {
	return writePullBatchStateUntrustedMarkerWithSync(ctx, statePath, marker, syncPullOutputDirectory)
}

func writePullBatchStateUntrustedMarkerWithSync(ctx context.Context, statePath string, marker pullBatchStateUntrustedMarker, syncDirectory func(*os.Root) error) (pullBatchStateMarkerCommit, error) {
	var commit pullBatchStateMarkerCommit
	normalizedStatePath, err := normalizePullBatchPath(statePath)
	if err != nil {
		return commit, err
	}
	stateName := filepath.Base(normalizedStatePath)
	markerStateName, err := decodePullBatchStateMarkerName(marker.StateNameHex)
	if err != nil {
		return commit, err
	}
	if markerStateName != stateName {
		return commit, fmt.Errorf("状态文件不可信标记所属文件 %q 与当前状态文件 %q 不匹配", markerStateName, stateName)
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return commit, err
	}
	if _, err := parsePullBatchStateUntrustedMarker(data); err != nil {
		return commit, err
	}
	markerPath := pullBatchStateUntrustedMarkerPath(normalizedStatePath)
	if err := verifyPullBatchLifecyclePath(ctx, markerPath); err != nil {
		return commit, fmt.Errorf("写入状态文件不可信标记前生命周期锁校验失败: %w", err)
	}
	root, markerName, absoluteMarkerPath, parentInfo, err := openPullArchiveOutput(markerPath)
	if err != nil {
		return commit, err
	}
	defer root.Close()
	initial, initialExists, err := snapshotPullJSONOutput(root, markerName)
	if err != nil {
		return commit, err
	}
	lockName := pullAtomicJSONLockName(markerName)
	lockFile, lockInfo, err := openPullAtomicJSONLock(root, lockName)
	if err != nil {
		return commit, fmt.Errorf("打开状态文件不可信标记锁失败: %w", err)
	}
	locked := false
	defer func() {
		if locked {
			_ = unlockPullAtomicJSONFile(lockFile)
		}
		_ = lockFile.Close()
	}()
	if err := lockPullAtomicJSONFile(lockFile); err != nil {
		return commit, fmt.Errorf("获取状态文件不可信标记锁失败: %w", err)
	}
	locked = true
	if err := verifyPullAtomicJSONLock(root, lockName, lockInfo); err != nil {
		return commit, err
	}
	if err := verifyPullJSONOutputUnchanged(root, markerName, absoluteMarkerPath, initial, initialExists); err != nil {
		return commit, err
	}
	if initialExists {
		existing, _, err := readPullBatchStateUntrustedMarkerFromSnapshot(root, markerName, absoluteMarkerPath, initial)
		if err != nil {
			return commit, err
		}
		existingStateName, err := decodePullBatchStateMarkerName(existing.StateNameHex)
		if err != nil {
			return commit, err
		}
		if err := verifyPullBatchStateMarkerOwner(root, existingStateName, stateName); err != nil {
			return commit, err
		}
	}
	publishedInfo, err := publishAtomicJSONFromSnapshot(root, markerName, absoluteMarkerPath, parentInfo, initial, initialExists, data, syncDirectory)
	commit = pullBatchStateMarkerCommit{marker: marker, info: publishedInfo}
	if err != nil {
		return commit, err
	}
	if err := verifyPullAtomicJSONLock(root, lockName, lockInfo); err != nil {
		return commit, err
	}
	if err := verifyPullBatchLifecyclePath(ctx, markerPath); err != nil {
		return commit, fmt.Errorf("写入状态文件不可信标记后生命周期锁校验失败: %w", err)
	}
	return commit, nil
}

func clearPullBatchStateUntrustedMarker(ctx context.Context, statePath string, commit pullBatchStateMarkerCommit) error {
	markerPath := pullBatchStateUntrustedMarkerPath(statePath)
	if err := verifyPullBatchLifecyclePath(ctx, markerPath); err != nil {
		return err
	}
	root, markerName, absoluteMarkerPath, _, err := openPullArchiveOutput(markerPath)
	if err != nil {
		return err
	}
	defer root.Close()
	initial, exists, err := snapshotPullJSONOutput(root, markerName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("状态文件不可信标记在确认前被删除: %s", markerPath)
	}
	if !safePullBatchFileIdentity(commit.info, initial) {
		return fmt.Errorf("状态文件不可信标记在确认前被替换: %s", markerPath)
	}
	marker, openedInfo, err := readPullBatchStateUntrustedMarkerFromSnapshot(root, markerName, absoluteMarkerPath, initial)
	if err != nil {
		return err
	}
	if !safePullBatchFileIdentity(commit.info, openedInfo) || marker != commit.marker {
		return fmt.Errorf("状态文件不可信标记事务在确认前发生变化: %s", markerPath)
	}
	markerStateName, err := decodePullBatchStateMarkerName(marker.StateNameHex)
	if err != nil {
		return err
	}
	if err := verifyPullBatchStateMarkerOwner(root, markerStateName, filepath.Base(statePath)); err != nil {
		return err
	}
	if err := root.Remove(markerName); err != nil {
		return err
	}
	// The state rename was already durably confirmed. A failed sync here can
	// only resurrect the marker after a crash, which causes a conservative pull.
	_ = syncPullOutputDirectory(root)
	return nil
}

func marshalPullBatchState(state pullBatchState) ([]byte, error) {
	state.CommitProtocol = pullBatchStateCommitProtocolVersion
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	state.Items = copyPullBatchStateItemsForPersistence(state.Items, sensitive.DefaultProfile())
	return json.MarshalIndent(state, "", "  ")
}

func writePullBatchReport(path string, report PullBatchReport) error {
	data, err := marshalPullBatchReport(report)
	if err != nil {
		return err
	}
	return writeAtomicJSON(path, data)
}

func writePullBatchReportWhileLocked(ctx context.Context, path string, report PullBatchReport) error {
	data, err := marshalPullBatchReport(report)
	if err != nil {
		return err
	}
	return writeAtomicJSONWhileLocked(ctx, path, data)
}

func marshalPullBatchReport(report PullBatchReport) ([]byte, error) {
	report.Items = copyPullBatchResultsForPersistence(report.Items, sensitive.DefaultProfile())
	return json.MarshalIndent(report, "", "  ")
}

func writeAtomicJSON(path string, data []byte) error {
	return writeAtomicJSONWithSync(path, data, syncPullOutputDirectory)
}

func writeAtomicJSONWithSync(path string, data []byte, syncDirectory func(*os.Root) error) error {
	normalizedPath, err := normalizePullBatchPath(path)
	if err != nil {
		return fmt.Errorf("解析 JSON 输出路径失败: %w", err)
	}
	outputRoot, outputName, outputPath, parentInfo, err := openPullArchiveOutput(normalizedPath)
	if err != nil {
		return fmt.Errorf("准备 JSON 输出失败: %w", err)
	}
	defer outputRoot.Close()
	initialOutput, initialOutputExists, err := snapshotPullJSONOutput(outputRoot, outputName)
	if err != nil {
		return fmt.Errorf("记录 JSON 输出初始状态失败: %w", err)
	}
	return writeAtomicJSONFromSnapshotWithSync(outputRoot, outputName, outputPath, parentInfo, initialOutput, initialOutputExists, data, syncDirectory)
}

func writeAtomicJSONWhileLocked(ctx context.Context, path string, data []byte) error {
	return writeAtomicJSONWhileLockedWithSync(ctx, path, data, syncPullOutputDirectory)
}

func writeAtomicJSONWhileLockedWithSync(ctx context.Context, path string, data []byte, syncDirectory func(*os.Root) error) error {
	if err := verifyPullBatchLifecyclePath(ctx, path); err != nil {
		return fmt.Errorf("写入 JSON 前生命周期锁校验失败: %w", err)
	}
	if err := writeAtomicJSONWithSync(path, data, syncDirectory); err != nil {
		return err
	}
	if err := verifyPullBatchLifecyclePath(ctx, path); err != nil {
		return fmt.Errorf("写入 JSON 后生命周期锁校验失败: %w", err)
	}
	return nil
}

func writeAtomicJSONFromSnapshot(outputRoot *os.Root, outputName, outputPath string, parentInfo, initialOutput os.FileInfo, initialOutputExists bool, data []byte) error {
	return writeAtomicJSONFromSnapshotWithSync(outputRoot, outputName, outputPath, parentInfo, initialOutput, initialOutputExists, data, syncPullOutputDirectory)
}

func writeAtomicJSONFromSnapshotWithSync(outputRoot *os.Root, outputName, outputPath string, parentInfo, initialOutput os.FileInfo, initialOutputExists bool, data []byte, syncDirectory func(*os.Root) error) error {
	lockName := pullAtomicJSONLockName(outputName)
	lockFile, lockInfo, err := openPullAtomicJSONLock(outputRoot, lockName)
	if err != nil {
		return fmt.Errorf("打开 JSON 输出锁失败: %w", err)
	}
	locked := false
	defer func() {
		if locked {
			_ = unlockPullAtomicJSONFile(lockFile)
		}
		_ = lockFile.Close()
	}()
	if err := lockPullAtomicJSONFile(lockFile); err != nil {
		return fmt.Errorf("获取 JSON 输出锁失败: %w", err)
	}
	locked = true
	if err := verifyPullAtomicJSONLock(outputRoot, lockName, lockInfo); err != nil {
		return err
	}
	if err := verifyPullJSONOutputUnchanged(outputRoot, outputName, outputPath, initialOutput, initialOutputExists); err != nil {
		return err
	}
	if _, err := publishAtomicJSONFromSnapshot(outputRoot, outputName, outputPath, parentInfo, initialOutput, initialOutputExists, data, syncDirectory); err != nil {
		return err
	}
	if err := verifyPullAtomicJSONLock(outputRoot, lockName, lockInfo); err != nil {
		return fmt.Errorf("发布 JSON 后锁身份校验失败: %w", err)
	}
	return nil
}

func publishAtomicJSONFromSnapshot(outputRoot *os.Root, outputName, outputPath string, parentInfo, initialOutput os.FileInfo, initialOutputExists bool, data []byte, syncDirectory func(*os.Root) error) (os.FileInfo, error) {
	stagingName, file, stagingOwner, err := createPullArchiveStaging(outputRoot)
	if err != nil {
		return nil, fmt.Errorf("创建 JSON 临时文件失败: %w", err)
	}
	fileOpen := true
	cleanupStaging := true
	defer func() {
		if fileOpen {
			_ = file.Close()
		}
		if cleanupStaging {
			_ = removeOwnedPullArchiveStaging(outputRoot, stagingName, stagingOwner)
		}
	}()

	payload := make([]byte, 0, len(data)+1)
	payload = append(payload, data...)
	payload = append(payload, '\n')
	_, writeErr := file.Write(payload)
	chmodErr := file.Chmod(0600)
	syncErr := file.Sync()
	stagedInfo, statErr := file.Stat()
	if statErr == nil {
		// Payload writes update size/mtime; cleanup must compare against the
		// final state of our descriptor while still rejecting replacements.
		stagingOwner = stagedInfo
	}
	closeErr := file.Close()
	fileOpen = false
	if err := errors.Join(writeErr, chmodErr, syncErr, statErr, closeErr); err != nil {
		return nil, fmt.Errorf("写入 JSON 临时文件失败: %w", err)
	}
	if err := verifyPullArchivePublication(outputRoot, stagingName, outputName, outputPath, parentInfo, stagedInfo); err != nil {
		return nil, fmt.Errorf("发布 JSON 前安全检查失败: %w", err)
	}
	if err := verifyPullJSONOutputUnchanged(outputRoot, outputName, outputPath, initialOutput, initialOutputExists); err != nil {
		return nil, err
	}
	if err := outputRoot.Rename(stagingName, outputName); err != nil {
		return nil, fmt.Errorf("原子发布 JSON %s 失败: %w", outputPath, err)
	}
	cleanupStaging = false
	if syncDirectory == nil {
		syncDirectory = syncPullOutputDirectory
	}
	if err := syncDirectory(outputRoot); err != nil {
		return stagedInfo, fmt.Errorf("同步 JSON 输出目录失败: %w", err)
	}
	return stagedInfo, nil
}

func pullAtomicJSONLockName(outputName string) string {
	return ".dm-pull-json-lock-" + sha256Hash(outputName)[:32] + ".lock"
}

func pullAtomicJSONLockPath(outputPath string) string {
	return filepath.Join(filepath.Dir(outputPath), pullAtomicJSONLockName(filepath.Base(outputPath)))
}

func pullBatchStateUntrustedMarkerPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), pullBatchStateMarkerFileName)
}

func newPullBatchStateUntrustedMarker(statePath string) (pullBatchStateUntrustedMarker, error) {
	stateName := filepath.Base(statePath)
	if err := validatePullBatchStateMarkerName(stateName); err != nil {
		return pullBatchStateUntrustedMarker{}, err
	}
	transaction := make([]byte, 16)
	if _, err := rand.Read(transaction); err != nil {
		return pullBatchStateUntrustedMarker{}, err
	}
	return pullBatchStateUntrustedMarker{
		Version:      pullBatchStateMarkerVersion,
		StateNameHex: hex.EncodeToString([]byte(stateName)),
		Transaction:  hex.EncodeToString(transaction),
	}, nil
}

func validatePullBatchStateMarkerName(stateName string) error {
	if stateName == "" || stateName == "." || stateName == ".." || filepath.Base(stateName) != stateName || !filepath.IsLocal(stateName) ||
		strings.ContainsAny(stateName, `/\\`) || strings.IndexByte(stateName, 0) >= 0 || len([]byte(stateName)) > pullBatchStateMarkerNameMaxBytes {
		return fmt.Errorf("状态文件标记包含无效文件名: %q", stateName)
	}
	if pullInternalOutputNameReserved(stateName) {
		return fmt.Errorf("状态文件标记使用了 pull 内部保留命名空间: %s", stateName)
	}
	if err := validatePullBatchPlatformPath(stateName); err != nil {
		return fmt.Errorf("状态文件标记包含平台非法文件名 %q: %w", stateName, err)
	}
	return nil
}

func decodePullBatchStateMarkerName(encoded string) (string, error) {
	if encoded == "" || len(encoded) > 2*pullBatchStateMarkerNameMaxBytes || len(encoded)%2 != 0 {
		return "", fmt.Errorf("状态文件不可信标记包含无效 state_name_hex")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("状态文件不可信标记包含无效 state_name_hex: %w", err)
	}
	if hex.EncodeToString(decoded) != encoded {
		return "", fmt.Errorf("状态文件不可信标记包含非规范 state_name_hex")
	}
	stateName := string(decoded)
	if err := validatePullBatchStateMarkerName(stateName); err != nil {
		return "", err
	}
	return stateName, nil
}

func parsePullBatchStateUntrustedMarker(data []byte) (pullBatchStateUntrustedMarker, error) {
	var marker pullBatchStateUntrustedMarker
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return marker, fmt.Errorf("解析状态文件不可信标记失败: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return marker, fmt.Errorf("状态文件不可信标记包含额外 JSON 数据")
	}
	if marker.Version != pullBatchStateMarkerVersion {
		return marker, fmt.Errorf("状态文件不可信标记版本不受支持: %d", marker.Version)
	}
	if _, err := decodePullBatchStateMarkerName(marker.StateNameHex); err != nil {
		return marker, err
	}
	transaction, err := hex.DecodeString(marker.Transaction)
	if err != nil || len(transaction) != 16 || hex.EncodeToString(transaction) != marker.Transaction {
		return marker, fmt.Errorf("状态文件不可信标记包含无效 transaction")
	}
	return marker, nil
}

func readPullBatchStateUntrustedMarker(root *os.Root, stateName string) (*pullBatchStateUntrustedMarker, os.FileInfo, error) {
	markerPath := filepath.Join(root.Name(), pullBatchStateMarkerFileName)
	initial, exists, err := snapshotPullJSONOutput(root, pullBatchStateMarkerFileName)
	if err != nil {
		return nil, nil, err
	}
	if !exists {
		return nil, nil, nil
	}
	marker, openedInfo, err := readPullBatchStateUntrustedMarkerFromSnapshot(root, pullBatchStateMarkerFileName, markerPath, initial)
	if err != nil {
		return nil, nil, err
	}
	markerStateName, err := decodePullBatchStateMarkerName(marker.StateNameHex)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyPullBatchStateMarkerOwner(root, markerStateName, stateName); err != nil {
		return nil, nil, err
	}
	return &marker, openedInfo, nil
}

func readPullBatchStateUntrustedMarkerFromSnapshot(root *os.Root, markerName, markerPath string, initial os.FileInfo) (pullBatchStateUntrustedMarker, os.FileInfo, error) {
	var marker pullBatchStateUntrustedMarker
	if initial == nil || initial.Size() > pullBatchStateMarkerMaxBytes {
		return marker, nil, fmt.Errorf("状态文件不可信标记超过 %d 字节上限: %s", pullBatchStateMarkerMaxBytes, markerPath)
	}
	file, err := openPullBatchStateFile(root, markerName)
	if err != nil {
		return marker, nil, fmt.Errorf("打开状态文件不可信标记失败: %w", err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !safePullBatchFileIdentity(initial, openedInfo) {
		_ = file.Close()
		return marker, nil, errors.Join(statErr, fmt.Errorf("状态文件不可信标记在打开时发生变化: %s", markerPath))
	}
	data, readErr := io.ReadAll(io.LimitReader(file, pullBatchStateMarkerMaxBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return marker, nil, fmt.Errorf("读取状态文件不可信标记失败: %w", err)
	}
	if int64(len(data)) > pullBatchStateMarkerMaxBytes {
		return marker, nil, fmt.Errorf("状态文件不可信标记超过 %d 字节上限: %s", pullBatchStateMarkerMaxBytes, markerPath)
	}
	if err := verifyPullJSONOutputUnchanged(root, markerName, markerPath, initial, true); err != nil {
		return marker, nil, err
	}
	marker, err = parsePullBatchStateUntrustedMarker(data)
	return marker, openedInfo, err
}

func verifyPullBatchStateMarkerOwner(root *os.Root, markerStateName, currentStateName string) error {
	if err := validatePullBatchStateMarkerName(markerStateName); err != nil {
		return err
	}
	if err := validatePullBatchStateMarkerName(currentStateName); err != nil {
		return err
	}
	if markerStateName == currentStateName {
		return nil
	}
	markerStateInfo, markerStateErr := root.Lstat(markerStateName)
	currentStateInfo, currentStateErr := root.Lstat(currentStateName)
	if markerStateErr == nil && currentStateErr == nil && safePullBatchFileIdentity(markerStateInfo, currentStateInfo) {
		return nil
	}
	if markerStateErr != nil && !os.IsNotExist(markerStateErr) {
		return fmt.Errorf("检查不可信标记所属状态文件失败: %w", markerStateErr)
	}
	if currentStateErr != nil && !os.IsNotExist(currentStateErr) {
		return fmt.Errorf("检查当前状态文件失败: %w", currentStateErr)
	}
	return fmt.Errorf("状态文件不可信标记属于另一状态文件 %q，拒绝访问 %q", markerStateName, currentStateName)
}

func validatePullBatchStateUntrustedMarker(statePath string) error {
	normalizedPath, err := normalizePullBatchPath(statePath)
	if err != nil {
		return err
	}
	root, stateName, _, _, err := openPullArchiveOutput(normalizedPath)
	if err != nil {
		return err
	}
	defer root.Close()
	_, _, err = readPullBatchStateUntrustedMarker(root, stateName)
	return err
}

func safePullAtomicJSONLockIdentity(opened, current os.FileInfo) bool {
	return safePullBatchFileIdentity(opened, current)
}

func safePullBatchFileIdentity(opened, current os.FileInfo) bool {
	return opened != nil && current != nil && opened.Mode().IsRegular() && current.Mode().IsRegular() &&
		!pullArchiveInfoIsReparsePoint(opened) && !pullArchiveInfoIsReparsePoint(current) &&
		os.SameFile(opened, current) && opened.Size() == current.Size() && opened.ModTime().Equal(current.ModTime())
}

func verifyPullAtomicJSONLock(root *os.Root, lockName string, openedInfo os.FileInfo) error {
	currentInfo, err := root.Lstat(lockName)
	if err != nil {
		return fmt.Errorf("重新检查 JSON 输出锁失败: %w", err)
	}
	if !safePullAtomicJSONLockIdentity(openedInfo, currentInfo) {
		return fmt.Errorf("JSON 输出锁在等待期间发生变化: %s", lockName)
	}
	return nil
}

func snapshotPullJSONOutput(root *os.Root, outputName string) (os.FileInfo, bool, error) {
	info, err := root.Lstat(outputName)
	switch {
	case err == nil:
		if pullArchiveInfoIsReparsePoint(info) || !info.Mode().IsRegular() {
			return nil, false, fmt.Errorf("JSON 输出不是普通文件: %s", outputName)
		}
		return info, true, nil
	case os.IsNotExist(err):
		return nil, false, nil
	default:
		return nil, false, err
	}
}

func verifyPullJSONOutputUnchanged(root *os.Root, outputName, outputPath string, initial os.FileInfo, initiallyExisted bool) error {
	current, err := root.Lstat(outputName)
	if !initiallyExisted {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("重新检查 JSON 输出失败: %w", err)
		}
		return fmt.Errorf("JSON 输出在发布前被创建，拒绝覆盖: %s", outputPath)
	}
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("JSON 输出在发布前被删除，拒绝继续: %s", outputPath)
		}
		return fmt.Errorf("重新检查 JSON 输出失败: %w", err)
	}
	if initial == nil || pullArchiveInfoIsReparsePoint(current) || !current.Mode().IsRegular() ||
		!os.SameFile(initial, current) || initial.Size() != current.Size() || !initial.ModTime().Equal(current.ModTime()) {
		return fmt.Errorf("JSON 输出在发布前发生变化，拒绝覆盖: %s", outputPath)
	}
	return nil
}

func copyPullBatchStateItemsForPersistence(items map[string]pullBatchStateItem, profile sensitive.Profile) map[string]pullBatchStateItem {
	copied := make(map[string]pullBatchStateItem, len(items))
	for key, item := range items {
		item.Message = sensitive.RedactText(item.Message, profile)
		copied[key] = item
	}
	return copied
}

func copyPullBatchResultsForPersistence(items []PullBatchResult, profile sensitive.Profile) []PullBatchResult {
	copied := append([]PullBatchResult(nil), items...)
	for index := range copied {
		copied[index].Message = sensitive.RedactText(copied[index].Message, profile)
	}
	return copied
}

func syncPullOutputDirectory(root *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func updatePullBatchReportCounts(report *PullBatchReport) {
	report.Succeeded = 0
	report.Failed = 0
	report.Skipped = 0
	for _, item := range report.Items {
		switch item.Status {
		case pullBatchStatusSuccess:
			report.Succeeded++
		case pullBatchStatusSkipped:
			report.Skipped++
		case pullBatchStatusFailed:
			report.Failed++
		}
	}
}

func printPullBatchReport(w io.Writer, report PullBatchReport) {
	_, _ = fmt.Fprintf(w, "Pull summary: total=%d success=%d skipped=%d failed=%d\n", report.Total, report.Succeeded, report.Skipped, report.Failed)
	if report.To != "" {
		_, _ = fmt.Fprintf(w, "Target: %s\n", report.To)
	}
	if report.StateFile != "" {
		_, _ = fmt.Fprintf(w, "State file: %s\n", report.StateFile)
	}
	for _, item := range report.Items {
		_, _ = fmt.Fprintf(w, "- [%s] %s", item.Status, item.Image)
		if item.Target != "" {
			_, _ = fmt.Fprintf(w, " -> %s", item.Target)
		}
		if item.Attempts > 0 {
			_, _ = fmt.Fprintf(w, " attempts=%d", item.Attempts)
		}
		if item.Message != "" {
			_, _ = fmt.Fprintf(w, " (%s)", item.Message)
		}
		_, _ = fmt.Fprintln(w)
	}
}
