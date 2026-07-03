package diagnostics

import (
	"fmt"
	"io"
)

func printVolumeReport(w io.Writer, report VolumeReport, opts VolumeOptions) {
	fmt.Fprintln(w, "Docker volume 报告")
	printDockerEndpoint(w, report.DockerEndpoint)
	fmt.Fprintf(w, "Volume: 总数=%d 已列出=%d 未使用=%d 疑似未使用=%d 使用中=%d 未知大小=%d 可回收=%s\n\n",
		report.Summary.Total,
		len(report.Volumes),
		report.Summary.Unused,
		report.Summary.SuspectedUnused,
		report.Summary.Used,
		report.Summary.UnknownSize,
		humanBytes(uint64FromInt64(report.Summary.ReclaimableSize)),
	)
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "警告:")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Volume:")
	if len(report.Volumes) == 0 {
		fmt.Fprintln(w, "  无")
		return
	}
	for _, vol := range report.Volumes {
		name := displayLayerText(vol.Name, opts.NoTrunc, 48)
		mountpoint := displayLayerText(vol.Mountpoint, opts.NoTrunc, 72)
		fmt.Fprintf(w, "  - %s 状态=%s driver=%s 引用=%d(%s) 大小=%s 匿名=%v\n", name, vol.Status, vol.Driver, vol.RefCount, vol.RefSource, volumeSizeText(vol), vol.Anonymous)
		if mountpoint != "" {
			fmt.Fprintf(w, "      mountpoint=%s\n", mountpoint)
		}
		if vol.SizeError != "" {
			fmt.Fprintf(w, "      size-error=%s\n", vol.SizeError)
		}
		if len(vol.Containers) == 0 {
			fmt.Fprintln(w, "      容器=无")
			continue
		}
		for _, c := range vol.Containers {
			fmt.Fprintf(w, "      容器=%s 镜像=%s 状态=%s 挂载点=%s 可写=%v 模式=%s\n", c.Name, c.Image, c.State, c.Destination, c.RW, c.Mode)
		}
	}
}

func volumeSizeText(vol VolumeRef) string {
	if vol.Size < 0 {
		return "未知"
	}
	if vol.SizeSource == "" {
		return humanBytes(uint64FromInt64(vol.Size))
	}
	return fmt.Sprintf("%s(%s)", humanBytes(uint64FromInt64(vol.Size)), vol.SizeSource)
}
