//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package pull

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func lockPullAtomicJSONFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func tryLockPullAtomicJSONFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockPullAtomicJSONFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func openPullBatchStateFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
}

func openPullAtomicJSONLock(root *os.Root, lockName string) (*os.File, os.FileInfo, error) {
	for range 8 {
		info, err := root.Lstat(lockName)
		if os.IsNotExist(err) {
			file, createErr := root.OpenFile(lockName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
			if os.IsExist(createErr) {
				continue
			}
			if createErr != nil {
				return nil, nil, createErr
			}
			if chmodErr := file.Chmod(0600); chmodErr != nil {
				_ = file.Close()
				_ = root.Remove(lockName)
				return nil, nil, chmodErr
			}
			openedInfo, statErr := file.Stat()
			currentInfo, currentErr := root.Lstat(lockName)
			if statErr != nil || currentErr != nil || !safePullAtomicJSONLockIdentity(openedInfo, currentInfo) {
				_ = file.Close()
				if currentErr == nil && openedInfo != nil && os.SameFile(openedInfo, currentInfo) {
					_ = root.Remove(lockName)
				}
				return nil, nil, errors.Join(statErr, currentErr, fmt.Errorf("新建 JSON 输出锁身份校验失败: %s", lockName))
			}
			return file, openedInfo, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if pullArchiveInfoIsReparsePoint(info) || !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("JSON 输出锁不是普通文件: %s", lockName)
		}
		if info.Mode().Perm()&0077 != 0 {
			return nil, nil, fmt.Errorf("JSON 输出锁权限过宽: %s (%04o)", lockName, info.Mode().Perm())
		}
		file, openErr := root.OpenFile(lockName, os.O_RDWR, 0)
		if openErr != nil {
			return nil, nil, openErr
		}
		openedInfo, statErr := file.Stat()
		currentInfo, currentErr := root.Lstat(lockName)
		if statErr != nil || currentErr != nil || !safePullAtomicJSONLockIdentity(openedInfo, currentInfo) || !os.SameFile(info, currentInfo) {
			_ = file.Close()
			return nil, nil, errors.Join(statErr, currentErr, fmt.Errorf("JSON 输出锁在打开时发生变化: %s", lockName))
		}
		return file, openedInfo, nil
	}
	return nil, nil, fmt.Errorf("JSON 输出锁持续被并发替换: %s", lockName)
}
