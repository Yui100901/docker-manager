package diagnostics

import (
	"fmt"
	"io"
	"strings"
)

func printHealthReport(w io.Writer, report HealthReport) {
	fmt.Fprintf(w, "Docker 体检报告 (%s)\n", report.GeneratedAt)
	printDockerEndpoint(w, report.DockerEndpoint)
	printTargetSelection(w, report.Target)
	fmt.Fprintf(w, "容器: 总数=%d 运行中=%d 已停止=%d 重启中=%d 不健康=%d\n", report.Summary.Total, report.Summary.Running, report.Summary.Stopped, report.Summary.Restarting, report.Summary.Unhealthy)
	fmt.Fprintf(w, "风险: 重启=%d 日志=%d 公网端口=%d 问题=%d\n\n", report.Summary.RestartWarnings, report.Summary.LogWarnings, report.Summary.PublicBindings, len(report.Issues))

	fmt.Fprintln(w, "问题:")
	if len(report.Issues) == 0 {
		fmt.Fprintln(w, "  无")
	} else {
		for _, issue := range report.Issues {
			containerName := ""
			if issue.Container != "" {
				containerName = " " + issue.Container
			}
			fmt.Fprintf(w, "  - [%s] %s%s: %s\n", issue.Severity, issue.Type, containerName, issue.Message)
		}
	}

	fmt.Fprintln(w, "\n容器:")
	for _, c := range report.Containers {
		health := c.HealthStatus
		if health == "" {
			health = "none"
		}
		fmt.Fprintf(w, "  - %s 状态=%s 健康=%s 重启次数=%d 镜像=%s\n", c.Name, c.State, health, c.RestartCount, c.Image)
		if c.ImageID != "" || c.ImageDigest != "" {
			fmt.Fprintf(w, "      镜像ID=%s digest=%s\n", c.ImageID, c.ImageDigest)
		}
		if c.RestartPolicy != "" || c.LogDriver != "" || c.NetworkMode != "" {
			fmt.Fprintf(w, "      restart=%s log-driver=%s network-mode=%s\n", c.RestartPolicy, c.LogDriver, c.NetworkMode)
		}
		if c.LogReadability != "" || c.LogReadabilityMessage != "" {
			fmt.Fprintf(w, "      log-readable=%s note=%s\n", c.LogReadability, c.LogReadabilityMessage)
		}
		for _, network := range c.Networks {
			fmt.Fprintf(w, "      网络=%s ip=%s gateway=%s endpoint=%s aliases=%s\n", network.Name, network.IPAddress, network.Gateway, network.EndpointID, strings.Join(network.Aliases, ","))
		}
		for _, mount := range c.Mounts {
			fmt.Fprintf(w, "      挂载=%s name=%s source=%s target=%s rw=%v mode=%s\n", mount.Type, mount.Name, mount.Source, mount.Destination, mount.RW, mount.Mode)
		}
		for _, port := range c.PublicPorts {
			fmt.Fprintf(w, "      公网端口=%s\n", port)
		}
		for _, port := range c.ExposedPorts {
			fmt.Fprintf(w, "      暴露端口=%s\n", port)
		}
		for _, match := range c.LogMatches {
			fmt.Fprintf(w, "      日志[%s] %s\n", strings.Join(match.Keywords, ","), match.Line)
		}
	}
}
