//go:build windows

package diagnostics

import (
	"errors"

	"golang.org/x/sys/windows"
)

func diskFreeBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeBytesAvailable uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeBytesAvailable, nil, nil); err != nil {
		return 0, err
	}
	return freeBytesAvailable, nil
}

func diskFreeInodes(path string) (uint64, error) {
	return 0, errors.New("inode 语义在 Windows 上不可用")
}
