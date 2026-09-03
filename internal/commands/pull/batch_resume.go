package pull

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	digest "github.com/opencontainers/go-digest"
)

const pullBatchResumeFingerprintVersion = 1

func buildPullBatchResumeFingerprint(ctx context.Context, plan pullBatchPlanItem, opts PullBatchOptions) (*pullBatchResumeFingerprint, error) {
	size, archiveDigest, err := fingerprintPullBatchArchive(ctx, plan.ArchivePath)
	if err != nil {
		return nil, err
	}
	return &pullBatchResumeFingerprint{
		Version:       pullBatchResumeFingerprintVersion,
		ArchivePath:   plan.ArchivePath,
		ArchiveSize:   size,
		ArchiveDigest: archiveDigest,
		TargetOS:      opts.platform.targetOS,
		TargetArch:    opts.platform.targetArch,
		DockerLoad:    opts.Load || plan.Target != "",
	}, nil
}

func pullBatchResumeFingerprintMatches(ctx context.Context, item pullBatchStateItem, plan pullBatchPlanItem, opts PullBatchOptions) (bool, error) {
	fingerprint := item.Fingerprint
	if fingerprint == nil || fingerprint.Version != pullBatchResumeFingerprintVersion || fingerprint.ArchiveSize < 0 ||
		fingerprint.TargetOS != opts.platform.targetOS || fingerprint.TargetArch != opts.platform.targetArch ||
		fingerprint.DockerLoad != (opts.Load || plan.Target != "") {
		return false, nil
	}
	savedPath, err := normalizePullBatchPath(fingerprint.ArchivePath)
	if err != nil || !pullBatchPathsEqual(savedPath, plan.ArchivePath) {
		return false, nil
	}
	expectedDigest, err := digest.Parse(fingerprint.ArchiveDigest)
	if err != nil || expectedDigest.Algorithm() != digest.SHA256 || expectedDigest.String() != fingerprint.ArchiveDigest {
		return false, nil
	}
	size, currentDigest, err := fingerprintPullBatchArchive(ctx, plan.ArchivePath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, nil
	}
	return size == fingerprint.ArchiveSize && currentDigest == fingerprint.ArchiveDigest, nil
}

func fingerprintPullBatchArchive(ctx context.Context, path string) (int64, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	root, outputName, outputPath, parentInfo, err := openPullArchiveOutput(path)
	if err != nil {
		return 0, "", err
	}
	defer root.Close()
	initial, err := root.Lstat(outputName)
	if err != nil {
		return 0, "", fmt.Errorf("检查 pull 归档失败: %w", err)
	}
	if pullArchiveInfoIsReparsePoint(initial) || !initial.Mode().IsRegular() {
		return 0, "", fmt.Errorf("pull 归档不是普通文件: %s", outputPath)
	}
	file, err := openPullBatchStateFile(root, outputName)
	if err != nil {
		return 0, "", fmt.Errorf("打开 pull 归档失败: %w", err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !safePullBatchFileIdentity(initial, opened) {
		_ = file.Close()
		return 0, "", errors.Join(statErr, fmt.Errorf("pull 归档在打开时发生变化: %s", outputPath))
	}
	hasher := sha256.New()
	copyErr := copyWithContext(ctx, hasher, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return 0, "", fmt.Errorf("计算 pull 归档摘要失败: %w", err)
	}
	if err := verifyPullArchiveOutputPath(root, outputName, outputPath, parentInfo); err != nil {
		return 0, "", err
	}
	current, err := root.Lstat(outputName)
	if err != nil {
		return 0, "", fmt.Errorf("重新检查 pull 归档失败: %w", err)
	}
	if !safePullBatchFileIdentity(initial, current) || initial.Size() != current.Size() || !initial.ModTime().Equal(current.ModTime()) {
		return 0, "", fmt.Errorf("pull 归档在计算摘要期间发生变化: %s", outputPath)
	}
	return initial.Size(), digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(hasher.Sum(nil))).String(), nil
}

func clonePullBatchResumeFingerprint(fingerprint *pullBatchResumeFingerprint) *pullBatchResumeFingerprint {
	if fingerprint == nil {
		return nil
	}
	cloned := *fingerprint
	return &cloned
}
