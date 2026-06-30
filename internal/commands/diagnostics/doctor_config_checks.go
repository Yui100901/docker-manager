package diagnostics

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func checkDoctorConfig(path string) (doctorConfig, []DoctorCheck) {
	var cfg doctorConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, []DoctorCheck{{
				Name:        "dm-config",
				Status:      "skipped",
				Message:     "未找到配置文件 " + path,
				Recommended: "如需默认代理、平台或输出目录，可复制 .dm.yaml.example 为 .dm.yaml",
			}}
		}
		return cfg, []DoctorCheck{{
			Name:        "dm-config",
			Status:      "failed",
			Message:     err.Error(),
			Recommended: "检查配置文件路径和读取权限",
		}}
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, []DoctorCheck{{
			Name:        "dm-config",
			Status:      "failed",
			Message:     err.Error(),
			Recommended: "检查 YAML 格式，可参考 .dm.yaml.example",
		}}
	}
	check := DoctorCheck{Name: "dm-config", Status: "ok", Message: "配置文件可解析", Detail: path}
	if cfg.Proxy != "" {
		if _, err := url.ParseRequestURI(cfg.Proxy); err != nil {
			check.Status = "warning"
			check.Message = "配置文件 proxy 可能无效"
			check.Detail = err.Error()
			check.Recommended = "proxy 应包含 scheme 和 host，例如 http://127.0.0.1:7890"
		}
	}
	return cfg, []DoctorCheck{check}
}

func checkDoctorProxy(cfg doctorConfig) []DoctorCheck {
	envNames := []string{
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",
		"http_proxy",
		"https_proxy",
		"no_proxy",
	}
	var active []string
	var warnings []string
	for _, name := range envNames {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			continue
		}
		active = append(active, name+"="+value)
		if strings.Contains(strings.ToLower(name), "proxy") && !strings.Contains(strings.ToLower(name), "no_proxy") {
			if err := validateProxyURL(value); err != nil {
				warnings = append(warnings, name+": "+err.Error())
			}
		}
	}
	if cfg.Proxy != "" {
		active = append(active, "dm.yaml proxy="+cfg.Proxy)
		if err := validateProxyURL(cfg.Proxy); err != nil {
			warnings = append(warnings, "dm.yaml proxy: "+err.Error())
		}
	}
	if len(active) == 0 {
		return []DoctorCheck{{
			Name:        "proxy",
			Status:      "ok",
			Message:     "未设置代理，pull 将直连 registry",
			Recommended: "网络受限环境可设置 HTTP_PROXY/HTTPS_PROXY 或 .dm.yaml proxy",
		}}
	}
	sort.Strings(active)
	if len(warnings) > 0 {
		sort.Strings(warnings)
		return []DoctorCheck{{
			Name:        "proxy",
			Status:      "warning",
			Message:     "代理配置格式可能无效",
			Detail:      strings.Join(warnings, "; "),
			Recommended: "代理应包含 scheme 和 host，例如 http://127.0.0.1:7890；NO_PROXY 使用主机名、域名或 CIDR 列表",
		}}
	}
	return []DoctorCheck{{
		Name:    "proxy",
		Status:  "ok",
		Message: "检测到代理相关配置",
		Detail:  strings.Join(active, "; "),
	}}
}

func validateProxyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("缺少 scheme 或 host")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5":
		return nil
	default:
		return fmt.Errorf("不常见的代理 scheme %q", u.Scheme)
	}
}

func checkDoctorCA(cfg doctorConfig) []DoctorCheck {
	candidates := []struct {
		source string
		path   string
		isDir  bool
	}{
		{source: "dm.yaml ca_file", path: cfg.CAFile},
		{source: "dm.yaml ca_path", path: cfg.CAPath, isDir: true},
		{source: "dm.yaml registry_ca_file", path: cfg.RegistryCAFile},
		{source: "dm.yaml registry_ca_path", path: cfg.RegistryCAPath, isDir: true},
		{source: "SSL_CERT_FILE", path: os.Getenv("SSL_CERT_FILE")},
		{source: "SSL_CERT_DIR", path: os.Getenv("SSL_CERT_DIR"), isDir: true},
		{source: "DOCKER_CERT_PATH", path: os.Getenv("DOCKER_CERT_PATH"), isDir: true},
	}
	var active []string
	var missing []string
	for _, candidate := range candidates {
		path := strings.TrimSpace(candidate.path)
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			missing = append(missing, candidate.source+"="+path+": "+err.Error())
			continue
		}
		if candidate.isDir && !info.IsDir() {
			missing = append(missing, candidate.source+"="+path+": 不是目录")
			continue
		}
		if !candidate.isDir && info.IsDir() {
			missing = append(missing, candidate.source+"="+path+": 不是文件")
			continue
		}
		active = append(active, candidate.source+"="+path)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return []DoctorCheck{{
			Name:        "private-ca",
			Status:      "warning",
			Message:     "发现 CA 配置但路径不可用",
			Detail:      strings.Join(missing, "; "),
			Recommended: "检查私有 CA 文件/目录是否存在，或修正 SSL_CERT_FILE、SSL_CERT_DIR、DOCKER_CERT_PATH、dm.yaml CA 配置",
		}}
	}
	if len(active) == 0 {
		return []DoctorCheck{{
			Name:        "private-ca",
			Status:      "skipped",
			Message:     "未发现显式私有 CA 配置",
			Recommended: "使用自签或企业 CA 的 registry，可配置系统信任、SSL_CERT_FILE/SSL_CERT_DIR、DOCKER_CERT_PATH 或 dm.yaml CA 路径",
		}}
	}
	sort.Strings(active)
	return []DoctorCheck{{
		Name:    "private-ca",
		Status:  "ok",
		Message: "私有 CA 路径可访问",
		Detail:  strings.Join(active, "; "),
	}}
}
