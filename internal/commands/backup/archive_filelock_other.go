//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package backup

import (
	"fmt"
	"os"
)

func createBackupTransactionFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
}

func openBackupTransactionFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}

func lockBackupTransactionFile(_ *os.File) error {
	return fmt.Errorf("split backup transaction locking is unsupported on this platform")
}

func unlockBackupTransactionFile(_ *os.File) error {
	return nil
}
