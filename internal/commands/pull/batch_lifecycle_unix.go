//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package pull

import (
	"errors"
	"fmt"
	"os"
)

func openPullBatchLifecycleAnchor(root *os.Root, _ string) (*os.File, os.FileInfo, error) {
	anchor, err := root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	info, err := anchor.Stat()
	if err != nil || !info.IsDir() || pullArchiveInfoIsReparsePoint(info) {
		_ = anchor.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("pull 生命周期锁锚点不是普通目录")
	}
	return anchor, info, nil
}

func attachPullBatchLifecycleMarker(root *os.Root, lockName string, _ *os.File, _ os.FileInfo) (*os.File, os.FileInfo, error) {
	return openPullAtomicJSONLock(root, lockName)
}

func verifyPullBatchLifecycleLock(lock *pullBatchLifecycleLock) error {
	if lock == nil || lock.root == nil || lock.anchor == nil || lock.marker == nil {
		return fmt.Errorf("pull 输出目录生命周期锁不完整")
	}
	openedAnchor, anchorErr := lock.anchor.Stat()
	rootInfo, rootErr := lock.root.Stat(".")
	currentParent, parentErr := os.Lstat(lock.directory)
	if anchorErr != nil || rootErr != nil || parentErr != nil {
		return fmt.Errorf("重新检查 pull 输出目录生命周期锁失败: %w", errors.Join(anchorErr, rootErr, parentErr))
	}
	if lock.anchorInfo == nil || lock.parentInfo == nil || !openedAnchor.IsDir() || !rootInfo.IsDir() || !currentParent.IsDir() ||
		pullArchiveInfoIsReparsePoint(openedAnchor) || pullArchiveInfoIsReparsePoint(rootInfo) || pullArchiveInfoIsReparsePoint(currentParent) ||
		!os.SameFile(lock.anchorInfo, openedAnchor) || !os.SameFile(lock.parentInfo, rootInfo) || !os.SameFile(lock.parentInfo, currentParent) {
		return fmt.Errorf("pull 输出目录生命周期锁锚点在持有期间发生变化: %s", lock.directory)
	}
	openedMarker, markerStatErr := lock.marker.Stat()
	currentMarker, markerPathErr := lock.root.Lstat(lock.markerName)
	if markerStatErr != nil || markerPathErr != nil {
		return fmt.Errorf("重新检查 pull 输出目录生命周期锁标记失败: %w", errors.Join(markerStatErr, markerPathErr))
	}
	if !safePullAtomicJSONLockIdentity(lock.markerInfo, openedMarker) || !safePullAtomicJSONLockIdentity(lock.markerInfo, currentMarker) {
		return fmt.Errorf("pull 输出目录生命周期锁标记在持有期间发生变化: %s", lock.markerName)
	}
	return nil
}
