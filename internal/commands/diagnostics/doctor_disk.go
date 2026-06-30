package diagnostics

import (
	"fmt"
	"os"
	"time"
)

func checkDoctorDisk(outputDir string, minFreeMB int64) DoctorCheck {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return DoctorCheck{
			Name:        "disk",
			Status:      "failed",
			Message:     err.Error(),
			Detail:      outputDir,
			Recommended: "检查输出目录权限或改用 --output-dir 指定可写目录",
		}
	}
	writeProbe, writeErr := probeOutputDirWritable(outputDir)
	freeBytes, err := diskFreeBytes(outputDir)
	if err != nil {
		return DoctorCheck{
			Name:        "disk",
			Status:      "warning",
			Message:     err.Error() + writeProbe,
			Detail:      outputDir,
			Recommended: "无法读取剩余空间，仍建议确认镜像 tar、backup 离线包和日志报告有足够空间",
		}
	}
	freeMB := freeBytes / 1024 / 1024
	freeInodes, inodeErr := diskFreeInodes(outputDir)
	minFree := uint64(minFreeMB)
	status := "ok"
	msg := fmt.Sprintf("输出目录剩余空间约 %d MB", freeMB)
	recommend := ""
	if freeMB < minFree {
		status = "warning"
		recommend = fmt.Sprintf("剩余空间低于 %d MB，建议清理磁盘或改用更大的 --output-dir", minFreeMB)
	}
	if writeErr != nil {
		status = "failed"
		msg += "；写入探测失败: " + writeErr.Error()
		recommend = "检查输出目录权限或改用 --output-dir 指定可写目录"
	} else {
		msg += writeProbe
	}
	detail := outputDir
	if inodeErr == nil {
		detail += fmt.Sprintf(" free_inodes=%d", freeInodes)
		if freeInodes > 0 && freeInodes < 1024 && status == "ok" {
			status = "warning"
			recommend = "剩余 inode 较少，批量备份、日志报告或镜像导出可能失败"
		}
	} else {
		detail += " free_inodes=unknown(" + inodeErr.Error() + ")"
	}
	return DoctorCheck{Name: "disk", Status: status, Message: msg, Detail: detail, Recommended: recommend}
}

func probeOutputDirWritable(outputDir string) (string, error) {
	start := time.Now()
	file, err := os.CreateTemp(outputDir, ".dm-doctor-write-*")
	if err != nil {
		return "", err
	}
	name := file.Name()
	_, writeErr := file.Write([]byte("docker-manager doctor write probe\n"))
	closeErr := file.Close()
	removeErr := os.Remove(name)
	elapsed := time.Since(start)
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if removeErr != nil {
		return "", removeErr
	}
	return fmt.Sprintf("；写入探测 %dms", elapsed.Milliseconds()), nil
}
