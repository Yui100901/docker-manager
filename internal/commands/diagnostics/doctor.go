package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type doctorDockerService interface {
	Ping(ctx context.Context) (types.Ping, error)
	ServerVersion(ctx context.Context) (types.Version, error)
}

var newDoctorDockerService = func() (doctorDockerService, error) {
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerDoctorService{cli: cli}, nil
}

type dockerDoctorService struct {
	cli *client.Client
}

func (s *dockerDoctorService) Ping(ctx context.Context) (types.Ping, error) {
	return s.cli.Ping(ctx)
}

func (s *dockerDoctorService) ServerVersion(ctx context.Context) (types.Version, error) {
	return s.cli.ServerVersion(ctx)
}

type DoctorOptions struct {
	Registries    []string
	PlainHTTP     bool
	DockerConfig  string
	ConfigPath    string
	OutputDir     string
	Timeout       time.Duration
	CheckE2E      bool
	MinDiskFreeMB int64
	rpt.FormatOptions
}

type DoctorDefaults struct {
	ConfigPath string
	OutputDir  string
}

type DoctorReport struct {
	GeneratedAt     string        `json:"generated_at"`
	Platform        string        `json:"platform"`
	OverallStatus   string        `json:"overall_status"`
	Checks          []DoctorCheck `json:"checks"`
	Recommendations []string      `json:"recommendations,omitempty"`
}

type DoctorCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Recommended string `json:"recommended,omitempty"`
}

type doctorConfig struct {
	Proxy          string `yaml:"proxy"`
	TargetOS       string `yaml:"os"`
	Arch           string `yaml:"arch"`
	OutputDir      string `yaml:"output_dir"`
	CAFile         string `yaml:"ca_file"`
	CAPath         string `yaml:"ca_path"`
	RegistryCAFile string `yaml:"registry_ca_file"`
	RegistryCAPath string `yaml:"registry_ca_path"`
	Verbose        bool   `yaml:"verbose"`
	Quiet          bool   `yaml:"quiet"`
	JSON           bool   `yaml:"json"`
	LogJSON        bool   `yaml:"log_json"`
}

func NewDoctorCommand() *cobra.Command {
	return NewDoctorCommandWithDefaults(nil)
}

func NewDoctorCommandWithDefaults(defaults func() DoctorDefaults) *cobra.Command {
	opts := DoctorOptions{
		ConfigPath:    ".dm.yaml",
		OutputDir:     ".",
		Timeout:       5 * time.Second,
		CheckE2E:      true,
		MinDiskFreeMB: 1024,
	}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "检查 Docker、registry、代理、磁盘和测试前置条件",
		RunE: func(cmd *cobra.Command, args []string) error {
			if defaults != nil {
				cfg := defaults()
				if cfg.ConfigPath != "" && !cmd.Flags().Changed("dm-config") {
					opts.ConfigPath = cfg.ConfigPath
				}
				if cfg.OutputDir != "" && !cmd.Flags().Changed("output-dir") {
					opts.OutputDir = cfg.OutputDir
				}
			}
			report := runDoctor(cmd.Context(), opts)
			return rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printDoctorReport(w, report)
			})
		},
	}
	cmd.Flags().StringArrayVar(&opts.Registries, "registry", nil, "检查 registry 连通性和凭据，可重复指定")
	cmd.Flags().BoolVar(&opts.PlainHTTP, "plain-http", false, "使用 http:// 检查 registry /v2/")
	cmd.Flags().StringVar(&opts.DockerConfig, "docker-config", "", "Docker config.json 路径，默认使用 DOCKER_CONFIG/config.json 或 ~/.docker/config.json")
	cmd.Flags().StringVar(&opts.ConfigPath, "dm-config", opts.ConfigPath, "dm 配置文件路径")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "检查磁盘空间的输出目录")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "单项网络/Docker 检查超时时间")
	cmd.Flags().BoolVar(&opts.CheckE2E, "check-e2e", opts.CheckE2E, "检查 scripts/e2e.sh、Go 和 vendor 前置条件")
	cmd.Flags().Int64Var(&opts.MinDiskFreeMB, "min-disk-free-mb", opts.MinDiskFreeMB, "磁盘剩余空间告警阈值，单位 MB")
	rpt.AddFormatFlag(cmd, &opts.Format)
	return cmd
}

func runDoctor(ctx context.Context, opts DoctorOptions) DoctorReport {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = ".dm.yaml"
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "."
	}
	if opts.MinDiskFreeMB <= 0 {
		opts.MinDiskFreeMB = 1024
	}
	report := DoctorReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Platform:    runtime.GOOS + "/" + runtime.GOARCH,
	}
	report.Checks = append(report.Checks, checkDoctorDocker(ctx, opts.Timeout)...)
	cfg, configChecks := checkDoctorConfig(opts.ConfigPath)
	report.Checks = append(report.Checks, configChecks...)
	report.Checks = append(report.Checks, checkDoctorProxy(cfg)...)
	report.Checks = append(report.Checks, checkDoctorCA(cfg)...)
	report.Checks = append(report.Checks, checkDoctorDaemonConfig()...)
	report.Checks = append(report.Checks, checkDoctorDisk(opts.OutputDir, opts.MinDiskFreeMB))
	report.Checks = append(report.Checks, checkDoctorDockerConfig(ctx, opts)...)
	for _, registry := range opts.Registries {
		report.Checks = append(report.Checks, checkDoctorRegistry(ctx, registry, opts)...)
	}
	if len(opts.Registries) == 0 {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:        "registry",
			Status:      "skipped",
			Message:     "未指定 --registry，跳过 registry 连通性检查",
			Recommended: "需要验证推送目标时执行 dm doctor --registry <registry>，内网 HTTP registry 可加 --plain-http",
		})
	}
	if opts.CheckE2E {
		report.Checks = append(report.Checks, checkDoctorToolchain()...)
	}
	report.OverallStatus = doctorOverallStatus(report.Checks)
	report.Recommendations = doctorRecommendations(report.Checks)
	return report
}

func checkDoctorDocker(ctx context.Context, timeout time.Duration) []DoctorCheck {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	svc, err := newDoctorDockerService()
	if err != nil {
		return []DoctorCheck{{
			Name:        "docker-daemon",
			Status:      "failed",
			Message:     err.Error(),
			Recommended: "确认 Docker daemon 已启动，当前用户有访问 Docker socket 或 named pipe 的权限",
		}}
	}
	ping, err := svc.Ping(checkCtx)
	if err != nil {
		return []DoctorCheck{{
			Name:        "docker-daemon",
			Status:      "failed",
			Message:     err.Error(),
			Recommended: "确认 Docker daemon 已启动；Linux 下检查 docker 组或 sudo 权限",
		}}
	}
	checks := []DoctorCheck{{
		Name:    "docker-daemon",
		Status:  "ok",
		Message: "Docker daemon 可访问",
		Detail:  "api_version=" + ping.APIVersion + " os_type=" + ping.OSType,
	}}
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
		FormatOptions: rpt.FormatOptions{Format: rpt.FormatJSON},
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

func doctorOverallStatus(checks []DoctorCheck) string {
	hasWarning := false
	for _, check := range checks {
		switch check.Status {
		case "failed":
			return "failed"
		case "warning":
			hasWarning = true
		}
	}
	if hasWarning {
		return "warning"
	}
	return "ok"
}

func doctorRecommendations(checks []DoctorCheck) []string {
	seen := map[string]bool{}
	var recommendations []string
	for _, check := range checks {
		if check.Recommended == "" || seen[check.Recommended] {
			continue
		}
		seen[check.Recommended] = true
		recommendations = append(recommendations, check.Recommended)
	}
	return recommendations
}

func printDoctorReport(w io.Writer, report DoctorReport) {
	fmt.Fprintln(w, "Docker manager doctor")
	fmt.Fprintf(w, "Platform: %s\n", report.Platform)
	fmt.Fprintf(w, "Overall: %s\n", report.OverallStatus)
	for _, check := range report.Checks {
		fmt.Fprintf(w, "- [%s] %s: %s", check.Status, check.Name, check.Message)
		if check.Detail != "" {
			fmt.Fprintf(w, " (%s)", check.Detail)
		}
		fmt.Fprintln(w)
		if check.Recommended != "" {
			fmt.Fprintf(w, "  建议: %s\n", check.Recommended)
		}
	}
}
