//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package audit

import (
	"fmt"
	"os"
)

func openAuditLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
}

func tryLockAuditFile(_ *os.File) (bool, error) {
	return false, fmt.Errorf("cross-process audit locking is unsupported on this platform")
}

func unlockAuditFile(_ *os.File) error {
	return nil
}
