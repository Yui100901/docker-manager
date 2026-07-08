package diagnostics

import (
	"fmt"
	"io"
)

func printRegistryLoginCheckReport(w io.Writer, report RegistryLoginCheckReport) {
	fmt.Fprintln(w, "Docker registry 登录检查")
	fmt.Fprintf(w, "Registry: %s\n", report.Registry)
	fmt.Fprintf(w, "Docker config: %s 已找到=%v\n", report.DockerConfig, report.ConfigFound)
	fmt.Fprintf(w, "凭据: 已找到=%v 来源=%s", report.Credential.Found, valueOr(report.Credential.Source, "无"))
	if report.Credential.Helper != "" {
		fmt.Fprintf(w, " helper=%s", report.Credential.Helper)
	}
	if report.Credential.Username != "" {
		fmt.Fprintf(w, " 用户=%s", report.Credential.Username)
	}
	if report.Credential.Message != "" {
		fmt.Fprintf(w, " 信息=%s", report.Credential.Message)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Registry 连通性: %s", checkStatusText(report.RegistryPing.Status))
	if report.RegistryPing.HTTPStatus != 0 {
		fmt.Fprintf(w, " http=%d", report.RegistryPing.HTTPStatus)
	}
	if report.RegistryPing.Message != "" {
		fmt.Fprintf(w, " 信息=%s", report.RegistryPing.Message)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Docker 登录: %s", checkStatusText(report.DockerLogin.Status))
	if report.DockerLogin.Message != "" {
		fmt.Fprintf(w, " 信息=%s", report.DockerLogin.Message)
	}
	fmt.Fprintln(w)
	if len(report.Recommendations) > 0 {
		fmt.Fprintln(w, "\n建议:")
		for _, tip := range report.Recommendations {
			fmt.Fprintf(w, "  - %s\n", tip)
		}
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func checkStatusText(status string) string {
	switch status {
	case "ok":
		return "通过"
	case "warning":
		return "警告"
	case "failed":
		return "失败"
	case "skipped":
		return "跳过"
	default:
		return status
	}
}
