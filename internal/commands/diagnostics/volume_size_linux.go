//go:build linux

package diagnostics

import (
	"os"
	"syscall"
)

func localFileDiskUsage(info os.FileInfo) int64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok && stat.Blocks > 0 {
		return stat.Blocks * 512
	}
	return info.Size()
}
