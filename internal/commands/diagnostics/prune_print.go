package diagnostics

import (
	"fmt"
	"io"
	"strings"
)

func printPruneReport(w io.Writer, report PruneReport) {
	fmt.Fprintf(w, "Docker 清理报告 (%s)\n", report.GeneratedAt)
	printDockerEndpoint(w, report.DockerEndpoint)
	printPruneScope(w, report.Scope)
	fmt.Fprintf(w, "预计可回收空间: %s\n\n", humanBytes(report.EstimatedBytes))

	printPruneSection(w, "已停止容器", len(report.StoppedContainers), func() {
		for _, c := range report.StoppedContainers {
			fmt.Fprintf(w, "  - %s %s image=%s size=%s status=%s\n", c.ID, c.Name, c.Image, humanBytes(uint64FromInt64(c.Size)), c.Status)
		}
	})
	printPruneSection(w, "悬空镜像", len(report.DanglingImages), func() {
		for _, img := range report.DanglingImages {
			fmt.Fprintf(w, "  - %s size=%s tags=%s\n", img.ID, humanBytes(uint64FromInt64(img.Size)), strings.Join(img.RepoTags, ","))
		}
	})
	printPruneSection(w, "未使用 volume", len(report.UnusedVolumes), func() {
		for _, vol := range report.UnusedVolumes {
			fmt.Fprintf(w, "  - %s driver=%s size=%s\n", vol.Name, vol.Driver, humanBytes(uint64FromInt64(vol.Size)))
		}
	})
	printPruneSection(w, "构建缓存", len(report.BuildCaches), func() {
		for _, cache := range report.BuildCaches {
			fmt.Fprintf(w, "  - %s type=%s size=%s %s\n", cache.ID, cache.Type, humanBytes(uint64FromInt64(cache.Size)), cache.Description)
		}
	})

	if report.Applied && report.ApplyResult != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "执行结果:")
		fmt.Fprintf(w, "  已删除容器: %d\n", len(report.ApplyResult.ContainersDeleted))
		fmt.Fprintf(w, "  已删除/取消标记镜像: %d\n", len(report.ApplyResult.ImagesDeleted))
		fmt.Fprintf(w, "  已删除 volume: %d\n", len(report.ApplyResult.VolumesDeleted))
		fmt.Fprintf(w, "  已删除构建缓存: %d\n", len(report.ApplyResult.BuildCachesDeleted))
		fmt.Fprintf(w, "  已回收空间: %s\n", humanBytes(report.ApplyResult.SpaceReclaimed))
	}
}

func printPruneScope(w io.Writer, scope PruneScope) {
	var parts []string
	if len(scope.Only) > 0 {
		parts = append(parts, "only="+strings.Join(scope.Only, ","))
	}
	if len(scope.Filters) > 0 {
		parts = append(parts, "filter="+strings.Join(scope.Filters, ","))
	}
	if scope.Until != "" {
		parts = append(parts, "until="+scope.Until)
	}
	if len(scope.ProtectLabels) > 0 {
		parts = append(parts, "protect-label="+strings.Join(scope.ProtectLabels, ","))
	}
	if len(parts) == 0 {
		fmt.Fprintln(w, "范围: 全部可清理资源")
		return
	}
	fmt.Fprintf(w, "范围: %s\n", strings.Join(parts, " "))
}

func printPruneSection(w io.Writer, title string, count int, printItems func()) {
	fmt.Fprintf(w, "%s: %d\n", title, count)
	if count > 0 {
		printItems()
	}
}
