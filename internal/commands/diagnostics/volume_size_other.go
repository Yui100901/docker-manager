//go:build !linux

package diagnostics

import "os"

func localFileDiskUsage(info os.FileInfo) int64 {
	return info.Size()
}
