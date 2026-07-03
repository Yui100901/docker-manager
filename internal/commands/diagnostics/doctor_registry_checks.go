package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"docker-manager/internal/commandflags"
	rpt "docker-manager/internal/report"
)

func checkDoctorDockerConfig(ctx context.Context, opts DoctorOptions) []DoctorCheck {
	configPath := opts.DockerConfig
	if configPath == "" {
		configPath = defaultDockerConfigPath()
	}
	cfg, found, err := readDockerConfig(configPath)
	if err != nil {
		return []DoctorCheck{{
			Name:        "docker-config",
			Status:      "failed",
			Message:     err.Error(),
			Detail:      configPath,
			Recommended: "检查 Docker config.json 是否为合法 JSON，或使用 --docker-config 指定路径",
		}}
	}
	if !found {
		return []DoctorCheck{{
			Name:        "docker-config",
			Status:      "warning",
			Message:     "未找到 Docker config.json",
			Detail:      configPath,
			Recommended: "需要访问私有 registry 或 push 时，先执行 docker login",
		}}
	}
	checks := []DoctorCheck{{
		Name:    "docker-config",
		Status:  "ok",
		Message: "Docker config.json 可解析",
		Detail:  configPath,
	}}
	checks = append(checks, checkDoctorCredentialHelpers(ctx, cfg)...)
	return checks
}

func checkDoctorCredentialHelpers(ctx context.Context, cfg dockerConfigFile) []DoctorCheck {
	helpers := map[string]bool{}
	if helper := strings.TrimSpace(cfg.CredsStore); helper != "" {
		helpers[helper] = true
	}
	for _, helper := range cfg.CredHelpers {
		if helper = strings.TrimSpace(helper); helper != "" {
			helpers[helper] = true
		}
	}
	if len(helpers) == 0 {
		return []DoctorCheck{{
			Name:    "docker-credential-helper",
			Status:  "skipped",
			Message: "Docker config 未配置 credsStore 或 credHelpers",
		}}
	}
	var checks []DoctorCheck
	for helper := range helpers {
		name := "docker-credential-" + helper
		if _, err := exec.LookPath(name); err != nil {
			checks = append(checks, DoctorCheck{
				Name:        "docker-credential-helper",
				Status:      "failed",
				Message:     name + " 不在 PATH 中",
				Recommended: "安装对应 credential helper，或改用 docker login 写入 auths",
			})
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := exec.CommandContext(checkCtx, name, "list").Output()
		cancel()
		status := "ok"
		message := name + " 可执行"
		recommend := ""
		if err != nil {
			status = "warning"
			message = name + " 可执行但 list 调用失败: " + err.Error()
			recommend = "如 registry 凭据读取失败，检查 credential helper 后端是否已登录或解锁"
		}
		checks = append(checks, DoctorCheck{Name: "docker-credential-helper", Status: status, Message: message, Recommended: recommend})
	}
	return checks
}

func checkDoctorRegistry(ctx context.Context, registry string, opts DoctorOptions) []DoctorCheck {
	checkCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	report, err := runRegistryLoginCheck(checkCtx, registry, RegistryLoginCheckOptions{
		DockerConfig:  opts.DockerConfig,
		PlainHTTP:     opts.PlainHTTP,
		Timeout:       opts.Timeout,
		FormatOptions: commandflags.FormatOptions{Format: rpt.FormatJSON},
	})
	if err != nil {
		return []DoctorCheck{{
			Name:        "registry:" + registry,
			Status:      "failed",
			Message:     err.Error(),
			Recommended: "检查 registry 地址格式，例如 registry.local:5000",
		}}
	}
	var checks []DoctorCheck
	checks = append(checks, DoctorCheck{
		Name:        "registry:" + report.Registry,
		Status:      report.RegistryPing.Status,
		Message:     report.RegistryPing.Message,
		Detail:      fmt.Sprintf("http_status=%d", report.RegistryPing.HTTPStatus),
		Recommended: strings.Join(report.Recommendations, "; "),
	})
	checks = append(checks, DoctorCheck{
		Name:    "registry-login:" + report.Registry,
		Status:  report.DockerLogin.Status,
		Message: report.DockerLogin.Message,
	})
	return checks
}

func checkDoctorToolchain() []DoctorCheck {
	var checks []DoctorCheck
	if path, err := exec.LookPath("go"); err != nil {
		checks = append(checks, DoctorCheck{
			Name:        "go-toolchain",
			Status:      "warning",
			Message:     "未找到 go",
			Recommended: "需要在目标机编译或运行 scripts/e2e.sh 默认构建流程时安装 Go；已上传 dm 二进制时可忽略",
		})
	} else {
		checks = append(checks, DoctorCheck{Name: "go-toolchain", Status: "ok", Message: "找到 go", Detail: path})
	}
	if _, err := os.Stat("vendor"); err == nil {
		checks = append(checks, DoctorCheck{Name: "vendor", Status: "ok", Message: "存在 vendor 目录，可离线构建"})
	} else if errors.Is(err, os.ErrNotExist) {
		checks = append(checks, DoctorCheck{Name: "vendor", Status: "skipped", Message: "未找到 vendor 目录", Recommended: "离线测试机可提前准备 vendor 或设置 DM_E2E_DM_BIN 跳过构建"})
	} else {
		checks = append(checks, DoctorCheck{Name: "vendor", Status: "warning", Message: err.Error()})
	}
	if info, err := os.Stat(filepath.Join("scripts", "e2e.sh")); err != nil {
		checks = append(checks, DoctorCheck{Name: "e2e-script", Status: "warning", Message: err.Error(), Recommended: "从项目根目录运行 doctor，或确认 scripts/e2e.sh 存在"})
	} else if info.IsDir() {
		checks = append(checks, DoctorCheck{Name: "e2e-script", Status: "failed", Message: "scripts/e2e.sh 是目录"})
	} else {
		checks = append(checks, DoctorCheck{Name: "e2e-script", Status: "ok", Message: "找到 scripts/e2e.sh"})
	}
	return checks
}
