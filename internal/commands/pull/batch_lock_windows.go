//go:build windows

package pull

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func lockPullAtomicJSONFile(file *os.File) error {
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&windows.Overlapped{},
	)
}

func tryLockPullAtomicJSONFile(file *os.File) (bool, error) {
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&windows.Overlapped{},
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockPullAtomicJSONFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}

func openPullBatchStateFile(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}

func openPullAtomicJSONLock(root *os.Root, lockName string) (*os.File, os.FileInfo, error) {
	parent, err := root.Open(".")
	if err != nil {
		return nil, nil, fmt.Errorf("锚定 JSON 输出锁目录失败: %w", err)
	}
	defer parent.Close()
	parentHandle := windows.Handle(parent.Fd())

	for range 8 {
		info, statErr := root.Lstat(lockName)
		create := os.IsNotExist(statErr)
		if statErr != nil && !create {
			return nil, nil, statErr
		}
		if !create && (pullArchiveInfoIsReparsePoint(info) || !info.Mode().IsRegular()) {
			return nil, nil, fmt.Errorf("JSON 输出锁不是普通文件: %s", lockName)
		}

		disposition := uint32(windows.FILE_OPEN)
		if create {
			disposition = windows.FILE_CREATE
		}
		file, openErr := openPullAtomicJSONLockRelative(parentHandle, lockName, disposition)
		if create && openErr == windows.STATUS_OBJECT_NAME_COLLISION {
			continue
		}
		if !create && (openErr == windows.STATUS_OBJECT_NAME_NOT_FOUND || openErr == windows.STATUS_OBJECT_PATH_NOT_FOUND) {
			continue
		}
		if openErr == windows.STATUS_REPARSE_POINT_ENCOUNTERED || openErr == windows.STATUS_STOPPED_ON_SYMLINK {
			return nil, nil, fmt.Errorf("JSON 输出锁不是普通文件: %s", lockName)
		}
		if openErr != nil {
			return nil, nil, openErr
		}

		openedInfo, openedStatErr := file.Stat()
		currentInfo, currentErr := root.Lstat(lockName)
		identityMatches := openedStatErr == nil && currentErr == nil && safePullAtomicJSONLockIdentity(openedInfo, currentInfo)
		if identityMatches && !create {
			identityMatches = os.SameFile(info, currentInfo)
		}
		if !identityMatches {
			_ = file.Close()
			return nil, nil, errors.Join(openedStatErr, currentErr, fmt.Errorf("JSON 输出锁在禁止删除打开时发生变化: %s", lockName))
		}
		if create {
			if chmodErr := file.Chmod(0600); chmodErr != nil {
				_ = file.Close()
				return nil, nil, chmodErr
			}
		}
		return file, openedInfo, nil
	}
	return nil, nil, fmt.Errorf("JSON 输出锁持续被并发替换: %s", lockName)
}

func openPullAtomicJSONLockRelative(parent windows.Handle, lockName string, disposition uint32) (*os.File, error) {
	name, err := windows.NewNTUnicodeString(lockName)
	if err != nil {
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    name,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		disposition,
		windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), lockName)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("创建 JSON 输出锁文件句柄失败")
	}
	return file, nil
}
