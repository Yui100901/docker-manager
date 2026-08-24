//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package backup

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func createBackupTransactionFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
}

func openBackupTransactionFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}

func lockBackupTransactionFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errBackupTransactionLocked
	}
	return err
}

func unlockBackupTransactionFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
