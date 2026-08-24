package pull

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

const tarBlockSize int64 = 512

func workspaceRegularBytes(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > int64Max-total {
			return fmt.Errorf("镜像临时文件大小溢出")
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func validatePackageTemporaryBudget(ctx context.Context, root string, limit int64) error {
	if limit <= 0 {
		return fmt.Errorf("镜像临时文件峰值上限必须大于 0")
	}
	var workspaceBytes int64
	archiveBytes := 2 * tarBlockSize
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if archiveBytes > int64Max-tarBlockSize {
			return fmt.Errorf("镜像 tar 大小溢出")
		}
		archiveBytes += tarBlockSize
		if !info.Mode().IsRegular() {
			return nil
		}
		size := info.Size()
		if size > int64Max-workspaceBytes {
			return fmt.Errorf("镜像临时文件大小溢出")
		}
		workspaceBytes += size
		padded, err := paddedTarSize(size)
		if err != nil {
			return err
		}
		if padded > int64Max-archiveBytes {
			return fmt.Errorf("镜像 tar 大小溢出")
		}
		archiveBytes += padded
		return nil
	})
	if err != nil {
		return err
	}
	if archiveBytes > limit || workspaceBytes > limit-archiveBytes {
		return fmt.Errorf("打包临时文件峰值 %d 超过上限 %d", workspaceBytes+archiveBytes, limit)
	}
	return nil
}

func paddedTarSize(size int64) (int64, error) {
	if size < 0 || size > int64Max-(tarBlockSize-1) {
		return 0, fmt.Errorf("镜像 tar 条目大小无效: %d", size)
	}
	return ((size + tarBlockSize - 1) / tarBlockSize) * tarBlockSize, nil
}

const int64Max = int64(^uint64(0) >> 1)
