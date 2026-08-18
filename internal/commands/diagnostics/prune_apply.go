package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/build"
	mobyclient "github.com/moby/moby/client"
)

const pruneRemovalVerificationTimeout = 3 * time.Second

func applyPruneReport(ctx context.Context, svc pruneDockerService, report PruneReport) (PruneApplyResult, error) {
	var result PruneApplyResult
	seenContainers := make(map[string]struct{})
	seenImages := make(map[string]struct{})
	seenVolumes := make(map[string]struct{})

	for _, candidate := range report.StoppedContainers {
		if err := ctx.Err(); err != nil {
			return finalizePruneApplyResult(result), err
		}
		if alreadySeen(seenContainers, candidate.ID) {
			continue
		}
		if strings.TrimSpace(candidate.ID) == "" {
			recordPruneApplyFailure(&result, pruneKindContainer, candidate.ID, errors.New("snapshot candidate has an empty ID"))
			continue
		}
		if err := svc.RemoveContainer(ctx, candidate.ID); err != nil {
			reconcilePruneRemovalError(ctx, &result, pruneKindContainer, candidate.ID, candidate.Size, err, func(checkCtx context.Context) error {
				_, inspectErr := svc.InspectContainer(checkCtx, candidate.ID)
				return inspectErr
			})
			if cancelErr := pruneCancellationError(ctx, err); cancelErr != nil {
				return finalizePruneApplyResult(result), cancelErr
			}
			continue
		}
		result.ContainersDeleted = append(result.ContainersDeleted, candidate.ID)
		addPositiveSize(&result.EstimatedBytesReclaimed, candidate.Size)
	}

	for _, candidate := range report.DanglingImages {
		if err := ctx.Err(); err != nil {
			return finalizePruneApplyResult(result), err
		}
		if alreadySeen(seenImages, normalizedPruneImageID(candidate.ID)) {
			continue
		}
		if strings.TrimSpace(candidate.ID) == "" {
			recordPruneApplyFailure(&result, pruneKindImage, candidate.ID, errors.New("snapshot candidate has an empty ID"))
			continue
		}
		current, err := svc.InspectImage(ctx, candidate.ID)
		if err != nil {
			if cancelErr := pruneCancellationError(ctx, err); cancelErr != nil {
				return finalizePruneApplyResult(result), cancelErr
			}
			recordPruneApplyFailure(&result, pruneKindImage, candidate.ID, fmt.Errorf("inspect before delete: %w", err))
			continue
		}
		if !samePruneImageID(current.ID, candidate.ID) {
			recordPruneApplyFailure(&result, pruneKindImage, candidate.ID, fmt.Errorf("image identity changed: inspect returned %q", current.ID))
			continue
		}
		if !isDanglingRepoTags(current.RepoTags) {
			recordPruneApplyFailure(&result, pruneKindImage, candidate.ID, errors.New("image is no longer dangling"))
			continue
		}
		if err := ctx.Err(); err != nil {
			return finalizePruneApplyResult(result), err
		}
		if err := svc.RemoveImage(ctx, candidate.ID); err != nil {
			reconcilePruneRemovalError(ctx, &result, pruneKindImage, candidate.ID, candidate.Size, err, func(checkCtx context.Context) error {
				_, inspectErr := svc.InspectImage(checkCtx, candidate.ID)
				return inspectErr
			})
			if cancelErr := pruneCancellationError(ctx, err); cancelErr != nil {
				return finalizePruneApplyResult(result), cancelErr
			}
			continue
		}
		result.ImagesDeleted = append(result.ImagesDeleted, candidate.ID)
		addPositiveSize(&result.EstimatedBytesReclaimed, candidate.Size)
	}

	var volumeRefs map[string][]VolumeContainerRef
	var volumePreflightErr error
	if len(report.UnusedVolumes) > 0 {
		if err := ctx.Err(); err != nil {
			return finalizePruneApplyResult(result), err
		}
		var warnings []string
		var err error
		volumeRefs, warnings, err = inspectPruneVolumeRefs(ctx, svc)
		if err != nil {
			if cancelErr := pruneCancellationError(ctx, err); cancelErr != nil {
				return finalizePruneApplyResult(result), cancelErr
			}
			volumePreflightErr = fmt.Errorf("volume reference check failed: %w", err)
		} else if len(warnings) > 0 {
			volumePreflightErr = fmt.Errorf("volume reference check incomplete: %s", strings.Join(warnings, "; "))
		}
	}

	for _, candidate := range report.UnusedVolumes {
		if err := ctx.Err(); err != nil {
			return finalizePruneApplyResult(result), err
		}
		if alreadySeen(seenVolumes, candidate.Name) {
			continue
		}
		if strings.TrimSpace(candidate.Name) == "" {
			recordPruneApplyFailure(&result, pruneKindVolume, candidate.Name, errors.New("snapshot candidate has an empty name"))
			continue
		}
		if volumePreflightErr != nil {
			recordPruneApplyFailure(&result, pruneKindVolume, candidate.Name, volumePreflightErr)
			continue
		}
		if refs := volumeRefs[candidate.Name]; len(refs) > 0 {
			recordPruneApplyFailure(&result, pruneKindVolume, candidate.Name, fmt.Errorf("volume is now referenced by %d container(s)", len(refs)))
			continue
		}
		if candidate.snapshot == nil {
			recordPruneApplyFailure(&result, pruneKindVolume, candidate.Name, errors.New("snapshot identity is missing"))
			continue
		}
		current, err := svc.InspectVolume(ctx, candidate.Name)
		if err != nil {
			if cancelErr := pruneCancellationError(ctx, err); cancelErr != nil {
				return finalizePruneApplyResult(result), cancelErr
			}
			recordPruneApplyFailure(&result, pruneKindVolume, candidate.Name, fmt.Errorf("inspect before delete: %w", err))
			continue
		}
		if !candidate.snapshot.matches(current) {
			recordPruneApplyFailure(&result, pruneKindVolume, candidate.Name, errors.New("volume identity changed after the report snapshot"))
			continue
		}
		if err := ctx.Err(); err != nil {
			return finalizePruneApplyResult(result), err
		}
		if err := svc.RemoveVolume(ctx, candidate.Name); err != nil {
			reconcilePruneRemovalError(ctx, &result, pruneKindVolume, candidate.Name, candidate.Size, err, func(checkCtx context.Context) error {
				_, inspectErr := svc.InspectVolume(checkCtx, candidate.Name)
				return inspectErr
			})
			if cancelErr := pruneCancellationError(ctx, err); cancelErr != nil {
				return finalizePruneApplyResult(result), cancelErr
			}
			continue
		}
		result.VolumesDeleted = append(result.VolumesDeleted, candidate.Name)
		addPositiveSize(&result.EstimatedBytesReclaimed, candidate.Size)
	}

	buildCacheCandidates := make(map[string]PruneBuildCacheRef)
	for _, candidate := range report.BuildCaches {
		if candidate.snapshot == nil || candidate.snapshot.ID != candidate.ID {
			continue
		}
		if _, exists := buildCacheCandidates[candidate.ID]; !exists {
			buildCacheCandidates[candidate.ID] = candidate
		}
	}
	confirmedBuildCaches := make(map[string]bool)
	seenBuildCaches := make(map[string]struct{})
	for _, candidate := range report.BuildCaches {
		if err := ctx.Err(); err != nil {
			return finalizePruneApplyResult(result), err
		}
		if alreadySeen(seenBuildCaches, candidate.ID) || confirmedBuildCaches[candidate.ID] {
			continue
		}
		if strings.TrimSpace(candidate.ID) == "" {
			recordPruneApplyFailure(&result, pruneKindBuildCache, candidate.ID, errors.New("snapshot candidate has an empty ID"))
			continue
		}
		if candidate.snapshot == nil || candidate.snapshot.ID != candidate.ID {
			recordPruneApplyFailure(&result, pruneKindBuildCache, candidate.ID, errors.New("trusted snapshot identity is missing or does not match the candidate"))
			continue
		}
		if err := recheckPruneBuildCacheCandidate(ctx, svc, candidate); err != nil {
			if cancelErr := pruneCancellationError(ctx, err); cancelErr != nil {
				return finalizePruneApplyResult(result), cancelErr
			}
			recordPruneApplyFailure(&result, pruneKindBuildCache, candidate.ID, fmt.Errorf("pre-delete eligibility check failed: %w", err))
			continue
		}
		cacheReport, err := svc.RemoveBuildCache(ctx, candidate.ID, candidate.snapshot.UntilCutoff, candidate.snapshot.HasUntilCutoff)
		if err != nil {
			if cacheReport != nil {
				recordBuildCachePruneReport(&result, candidate.ID, cacheReport, buildCacheCandidates, confirmedBuildCaches)
			}
			if !confirmedBuildCaches[candidate.ID] {
				reconcileBuildCacheRemovalError(ctx, svc, &result, candidate, err, confirmedBuildCaches)
			}
			if cancelErr := pruneCancellationError(ctx, err); cancelErr != nil {
				return finalizePruneApplyResult(result), cancelErr
			}
			continue
		}
		if cacheReport != nil {
			recordBuildCachePruneReport(&result, candidate.ID, cacheReport, buildCacheCandidates, confirmedBuildCaches)
		}
		if !confirmedBuildCaches[candidate.ID] {
			reconcileUnconfirmedBuildCacheRemoval(ctx, svc, &result, candidate, confirmedBuildCaches)
		}
	}

	result = finalizePruneApplyResult(result)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(result.Failures) > 0 || len(result.UnknownOutcomes) > 0 {
		return result, fmt.Errorf("prune apply completed with %d failed candidate(s) and %d unknown outcome(s)", len(result.Failures), len(result.UnknownOutcomes))
	}
	return result, nil
}

func recordPruneApplyFailure(result *PruneApplyResult, kind, id string, err error) {
	result.Failures = append(result.Failures, PruneApplyFailure{Kind: kind, ID: id, Error: err.Error()})
}

func recordPruneApplyUnknown(result *PruneApplyResult, kind, id string, err error) {
	result.UnknownOutcomes = append(result.UnknownOutcomes, PruneApplyUnknownOutcome{Kind: kind, ID: id, Reason: err.Error()})
}

func finalizePruneApplyResult(result PruneApplyResult) PruneApplyResult {
	result.ContainersDeleted = sortedUniquePruneIDs(result.ContainersDeleted)
	result.ImagesDeleted = sortedUniquePruneIDs(result.ImagesDeleted)
	result.VolumesDeleted = sortedUniquePruneIDs(result.VolumesDeleted)
	result.BuildCachesDeleted = sortedUniquePruneIDs(result.BuildCachesDeleted)
	return result
}

func pruneCancellationError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func samePruneImageID(left, right string) bool {
	left = normalizedPruneImageID(left)
	right = normalizedPruneImageID(right)
	return left != "" && left == right
}

func normalizedPruneImageID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "sha256:")
}

func alreadySeen(seen map[string]struct{}, id string) bool {
	if _, exists := seen[id]; exists {
		return true
	}
	seen[id] = struct{}{}
	return false
}

func sortedUniquePruneIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	out := ids[:0]
	for _, id := range ids {
		if len(out) == 0 || out[len(out)-1] != id {
			out = append(out, id)
		}
	}
	return out
}

func reconcilePruneRemovalError(
	ctx context.Context,
	result *PruneApplyResult,
	kind string,
	id string,
	size int64,
	operationErr error,
	inspect func(context.Context) error,
) {
	removed, known, verificationErr := verifyPruneRemoval(ctx, inspect)
	if !known {
		recordPruneApplyUnknown(result, kind, id, fmt.Errorf("destructive request ended with %v; removal verification failed: %w", operationErr, verificationErr))
		return
	}
	if removed {
		recordConfirmedPruneRemoval(result, kind, id, size)
		return
	}
	recordPruneApplyUnknown(result, kind, id, fmt.Errorf("destructive request ended with %v; the resource was still visible during verification, but the request may still complete", operationErr))
}

func verifyPruneRemoval(ctx context.Context, inspect func(context.Context) error) (removed bool, known bool, err error) {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pruneRemovalVerificationTimeout)
	defer cancel()

	err = inspect(checkCtx)
	switch {
	case err == nil:
		return false, true, nil
	case cerrdefs.IsNotFound(err):
		return true, true, nil
	default:
		return false, false, err
	}
}

func recordConfirmedPruneRemoval(result *PruneApplyResult, kind, id string, size int64) {
	switch kind {
	case pruneKindContainer:
		result.ContainersDeleted = append(result.ContainersDeleted, id)
	case pruneKindImage:
		result.ImagesDeleted = append(result.ImagesDeleted, id)
	case pruneKindVolume:
		result.VolumesDeleted = append(result.VolumesDeleted, id)
	case pruneKindBuildCache:
		result.BuildCachesDeleted = append(result.BuildCachesDeleted, id)
	}
	addPositiveSize(&result.EstimatedBytesReclaimed, size)
}

func recordBuildCachePruneReport(
	result *PruneApplyResult,
	requestedID string,
	cacheReport *build.CachePruneReport,
	candidates map[string]PruneBuildCacheRef,
	confirmed map[string]bool,
) {
	result.SpaceReclaimed += cacheReport.SpaceReclaimed
	reported := make(map[string]struct{})
	for _, id := range cacheReport.CachesDeleted {
		if strings.TrimSpace(id) == "" {
			recordPruneApplyFailure(result, pruneKindBuildCache, requestedID, errors.New("Docker reported a deleted cache record with an empty ID"))
			continue
		}
		if alreadySeen(reported, id) {
			continue
		}

		result.BuildCachesDeleted = append(result.BuildCachesDeleted, id)
		candidate, inSnapshot := candidates[id]
		if !inSnapshot {
			recordPruneApplyFailure(result, pruneKindBuildCache, id, fmt.Errorf("Docker reported deletion outside the fixed snapshot candidate set while pruning %q", requestedID))
			continue
		}
		if id != requestedID {
			recordPruneApplyFailure(result, pruneKindBuildCache, id, fmt.Errorf("Docker reported deletion beyond the exact requested cache record %q", requestedID))
		}
		if !confirmed[id] {
			confirmed[id] = true
			addPositiveSize(&result.EstimatedBytesReclaimed, candidate.Size)
		}
	}
}

func recheckPruneBuildCacheCandidate(ctx context.Context, svc pruneDockerService, candidate PruneBuildCacheRef) error {
	usage, err := svc.DiskUsage(ctx, mobyclient.DiskUsageOptions{BuildCache: true})
	if err != nil {
		return err
	}
	var current *build.CacheRecord
	for _, cache := range usage.BuildCache {
		if cache == nil || cache.ID != candidate.ID {
			continue
		}
		if current != nil {
			return fmt.Errorf("Docker returned duplicate records for cache ID %q", candidate.ID)
		}
		current = cache
	}
	if current == nil {
		return fmt.Errorf("cache record no longer exists")
	}
	return candidate.snapshot.validateCurrent(current)
}

func reconcileBuildCacheRemovalError(
	ctx context.Context,
	svc pruneDockerService,
	result *PruneApplyResult,
	candidate PruneBuildCacheRef,
	operationErr error,
	confirmed map[string]bool,
) {
	removed, known, verificationErr := verifyPruneBuildCacheRemoval(ctx, svc, candidate.ID)
	if !known {
		recordPruneApplyUnknown(result, pruneKindBuildCache, candidate.ID, fmt.Errorf("destructive request ended with %v; removal verification failed: %w", operationErr, verificationErr))
		return
	}
	if removed {
		confirmed[candidate.ID] = true
		recordConfirmedPruneRemoval(result, pruneKindBuildCache, candidate.ID, candidate.Size)
		return
	}
	recordPruneApplyUnknown(result, pruneKindBuildCache, candidate.ID, fmt.Errorf("destructive request ended with %v; the cache record was still visible during verification, but the request may still complete", operationErr))
}

func reconcileUnconfirmedBuildCacheRemoval(
	ctx context.Context,
	svc pruneDockerService,
	result *PruneApplyResult,
	candidate PruneBuildCacheRef,
	confirmed map[string]bool,
) {
	removed, known, verificationErr := verifyPruneBuildCacheRemoval(ctx, svc, candidate.ID)
	if !known {
		recordPruneApplyUnknown(result, pruneKindBuildCache, candidate.ID, fmt.Errorf("Docker did not confirm deletion of the cache record; removal verification failed: %w", verificationErr))
		return
	}
	if removed {
		confirmed[candidate.ID] = true
		recordConfirmedPruneRemoval(result, pruneKindBuildCache, candidate.ID, candidate.Size)
		return
	}
	recordPruneApplyFailure(result, pruneKindBuildCache, candidate.ID, errors.New("Docker did not confirm deletion of the cache record, and it still exists"))
}

func verifyPruneBuildCacheRemoval(ctx context.Context, svc pruneDockerService, id string) (removed bool, known bool, err error) {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pruneRemovalVerificationTimeout)
	defer cancel()

	usage, err := svc.DiskUsage(checkCtx, mobyclient.DiskUsageOptions{BuildCache: true})
	if err != nil {
		return false, false, err
	}
	for _, cache := range usage.BuildCache {
		if cache != nil && cache.ID == id {
			return false, true, nil
		}
	}
	return true, true, nil
}
