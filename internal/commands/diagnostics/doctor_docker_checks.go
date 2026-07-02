package diagnostics

import (
	"context"
	"docker-manager/internal/docker"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func checkDoctorDocker(ctx context.Context, timeout time.Duration) []DoctorCheck {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	svc, err := newDoctorDockerService()
	if err != nil {
		checks := []DoctorCheck{dockerEndpointCheck("", "")}
		return append(checks, DoctorCheck{
			Name:        "docker-daemon",
			Status:      "failed",
			Message:     err.Error(),
			Recommended: "确认 Docker daemon 已启动，当前用户有访问 Docker socket 或 named pipe 的权限",
		})
	}
	checks := []DoctorCheck{dockerEndpointCheck(svc.DaemonHost(), svc.ClientVersion())}
	ping, err := svc.Ping(checkCtx)
	if err != nil {
		return append(checks, DoctorCheck{
			Name:        "docker-daemon",
			Status:      "failed",
			Message:     err.Error(),
			Recommended: "确认 Docker daemon 已启动；Linux 下检查 docker 组或 sudo 权限",
		})
	}
	checks = append(checks, DoctorCheck{
		Name:    "docker-daemon",
		Status:  "ok",
		Message: "Docker daemon 可访问",
		Detail:  "api_version=" + ping.APIVersion + " os_type=" + ping.OSType,
	})
	version, err := svc.ServerVersion(checkCtx)
	if err != nil {
		checks = append(checks, DoctorCheck{
			Name:        "docker-version",
			Status:      "warning",
			Message:     err.Error(),
			Recommended: "Docker ping 可用但版本读取失败，建议检查 daemon API 兼容性",
		})
		return checks
	}
	checks = append(checks, DoctorCheck{
		Name:    "docker-version",
		Status:  "ok",
		Message: "Docker 版本读取成功",
		Detail:  "version=" + version.Version + " api_version=" + version.APIVersion + " os=" + version.Os + " arch=" + version.Arch,
	})
	return checks
}

func dockerEndpointCheck(daemonHost, clientVersion string) DoctorCheck {
	effective := docker.EffectiveOptions()
	host := strings.TrimSpace(daemonHost)
	if host == "" {
		host = strings.TrimSpace(effective.Host)
	}
	if host == "" {
		host = "Docker SDK 默认本地 endpoint"
	}
	var detail []string
	detail = append(detail, "host="+host)
	if clientVersion != "" {
		detail = append(detail, "client_api_version="+clientVersion)
	} else if effective.APIVersion != "" {
		detail = append(detail, "client_api_version="+effective.APIVersion)
	}
	if effective.CertPath != "" {
		detail = append(detail, "cert_path="+effective.CertPath)
	}
	tlsEnabled := effective.TLSVerify != nil && *effective.TLSVerify
	if tlsEnabled {
		detail = append(detail, "tls_verify=true")
	}
	if strings.HasPrefix(strings.ToLower(host), "tcp://") && !tlsEnabled {
		return DoctorCheck{
			Name:        "docker-endpoint",
			Status:      "warning",
			Message:     "当前 Docker endpoint 是未启用 TLS 校验的 TCP 地址",
			Detail:      strings.Join(detail, " "),
			Recommended: "生产环境不要裸露 2375；优先使用 TLS 2376、SSH 隧道、VPN 或内网访问控制",
		}
	}
	return DoctorCheck{
		Name:    "docker-endpoint",
		Status:  "ok",
		Message: "Docker endpoint 已解析",
		Detail:  strings.Join(detail, " "),
	}
}

func checkDoctorDaemonConfig() []DoctorCheck {
	path := dockerDaemonConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []DoctorCheck{{
				Name:        "docker-daemon-config",
				Status:      "skipped",
				Message:     "未找到 Docker daemon 配置文件",
				Detail:      path,
				Recommended: "需要 HTTP 或自签 registry 时，可在 daemon.json 中配置 insecure-registries 或 registry mirror，并重启 Docker",
			}}
		}
		return []DoctorCheck{{
			Name:        "docker-daemon-config",
			Status:      "warning",
			Message:     err.Error(),
			Detail:      path,
			Recommended: "检查 Docker daemon 配置文件读取权限",
		}}
	}
	var cfg struct {
		InsecureRegistries []string `json:"insecure-registries"`
		RegistryMirrors    []string `json:"registry-mirrors"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return []DoctorCheck{{
			Name:        "docker-daemon-config",
			Status:      "warning",
			Message:     err.Error(),
			Detail:      path,
			Recommended: "检查 daemon.json 是否为合法 JSON",
		}}
	}
	var detail []string
	if len(cfg.InsecureRegistries) > 0 {
		detail = append(detail, "insecure-registries="+strings.Join(cfg.InsecureRegistries, ","))
	}
	if len(cfg.RegistryMirrors) > 0 {
		detail = append(detail, "registry-mirrors="+strings.Join(cfg.RegistryMirrors, ","))
	}
	if len(detail) == 0 {
		return []DoctorCheck{{
			Name:    "docker-daemon-config",
			Status:  "ok",
			Message: "Docker daemon 配置可解析，未配置 insecure registry",
			Detail:  path,
		}}
	}
	return []DoctorCheck{{
		Name:        "docker-daemon-config",
		Status:      "ok",
		Message:     "Docker daemon registry 相关配置可解析",
		Detail:      path + " " + strings.Join(detail, "; "),
		Recommended: "确认 insecure-registries 仅用于可信内网 registry",
	}}
}

func dockerDaemonConfigPath() string {
	if runtime.GOOS == "windows" {
		if programData := os.Getenv("ProgramData"); programData != "" {
			return filepath.Join(programData, "docker", "config", "daemon.json")
		}
		return filepath.Join(`C:\ProgramData`, "docker", "config", "daemon.json")
	}
	return "/etc/docker/daemon.json"
}
