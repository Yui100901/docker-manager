//go:build windows

package backup

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func createBackupTransactionFile(path string) (*os.File, error) {
	return openWindowsBackupTransactionFile(path, windows.CREATE_NEW)
}

func openBackupTransactionFile(path string) (*os.File, error) {
	return openWindowsBackupTransactionFile(path, windows.OPEN_EXISTING)
}

func openWindowsBackupTransactionFile(path string, creationDisposition uint32) (*os.File, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		creationDisposition,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func lockBackupTransactionFile(file *os.File) error {
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&windows.Overlapped{},
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errBackupTransactionLocked
	}
	return err
}

func unlockBackupTransactionFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}
