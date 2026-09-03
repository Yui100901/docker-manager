//go:build !windows

package pull

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func validatePullBatchPlatformPath(string) error {
	return nil
}

func validatePullBatchPlatformPathClaims(map[string]pullBatchPathClaim) error {
	return nil
}

func pullBatchPathsEqual(left, right string) bool {
	// Keep case-only differences rejected on case-insensitive macOS volumes and
	// Linux mounts without probing or mutating the destination.
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func pullBatchPathLess(left, right string) bool {
	return strings.ToLower(filepath.Clean(left)) < strings.ToLower(filepath.Clean(right))
}

func pullBatchPathContains(parent, child string) bool {
	relative, err := filepath.Rel(strings.ToLower(filepath.Clean(parent)), strings.ToLower(filepath.Clean(child)))
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func rejectPullInternalOutputAlias(string) error {
	return nil
}

func rejectPullArchiveOutputLinks(outputPath string) error {
	for current, output := filepath.Clean(outputPath), true; ; current, output = filepath.Dir(current), false {
		info, err := os.Lstat(current)
		if err == nil {
			if pullArchiveInfoIsReparsePoint(info) {
				return fmt.Errorf("拒绝通过符号链接或重解析点写入归档输出: %s", current)
			}
			if output {
				if !info.Mode().IsRegular() {
					return fmt.Errorf("拒绝替换非普通归档输出: %s", current)
				}
			} else if !info.IsDir() {
				return fmt.Errorf("归档输出路径祖先不是目录: %s", current)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("检查归档输出路径 %s 失败: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func pullPathLstatNoFollow(path string) (os.FileInfo, error) {
	return os.Lstat(path)
}
