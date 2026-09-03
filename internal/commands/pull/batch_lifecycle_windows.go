//go:build windows

package pull

import (
	"errors"
	"fmt"
	"os"
)

func openPullBatchLifecycleAnchor(root *os.Root, lockName string) (*os.File, os.FileInfo, error) {
	return openPullAtomicJSONLock(root, lockName)
}

func attachPullBatchLifecycleMarker(_ *os.Root, _ string, _ *os.File, anchorInfo os.FileInfo) (*os.File, os.FileInfo, error) {
	return nil, anchorInfo, nil
}

func verifyPullBatchLifecycleLock(lock *pullBatchLifecycleLock) error {
	if lock == nil || lock.root == nil || lock.anchor == nil {
		return fmt.Errorf("pull 输出目录生命周期锁不完整")
	}
	openedInfo, statErr := lock.anchor.Stat()
	currentInfo, currentErr := lock.root.Lstat(lock.markerName)
	rootInfo, rootErr := lock.root.Stat(".")
	currentParent, parentErr := pullPathLstatNoFollow(lock.directory)
	if statErr != nil || currentErr != nil || rootErr != nil || parentErr != nil {
		return fmt.Errorf("重新检查 pull 输出目录生命周期锁失败: %w", errors.Join(statErr, currentErr, rootErr, parentErr))
	}
	if !safePullAtomicJSONLockIdentity(lock.anchorInfo, openedInfo) || !safePullAtomicJSONLockIdentity(lock.anchorInfo, currentInfo) ||
		lock.parentInfo == nil || !rootInfo.IsDir() || !currentParent.IsDir() || pullArchiveInfoIsReparsePoint(currentParent) ||
		!os.SameFile(lock.parentInfo, rootInfo) || !os.SameFile(lock.parentInfo, currentParent) {
		return fmt.Errorf("pull 输出目录生命周期锁在持有期间发生变化: %s", lock.directory)
	}
	return nil
}
