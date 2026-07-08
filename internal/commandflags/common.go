package commandflags

import (
	"time"

	"docker-manager/internal/completion"

	"github.com/spf13/cobra"
)

const (
	containerFilterHelp = "筛选容器，支持 name:/id:/image:/state:/status:/label: 和 * ? 通配符，可重复指定"
	redactProfileHelp   = "脱敏策略: none | basic | strict；未指定时 --redact-secrets 等价于 basic"
	dockerConfigHelp    = "Docker config.json 路径，默认使用 DOCKER_CONFIG/config.json 或 ~/.docker/config.json"
	plainHTTPHelp       = "使用 http:// 访问 registry /v2/，用于未启用 TLS 的内网 registry"
)

func AddContainerFilterFlags(cmd *cobra.Command, running *bool, filters *[]string, runningHelp string) {
	cmd.Flags().BoolVar(running, "running", false, runningHelp)
	cmd.Flags().StringArrayVarP(filters, "filter", "f", nil, containerFilterHelp)
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalContainers)
}

func AddContainerFilterFlag(cmd *cobra.Command, filters *[]string, help string) {
	if help == "" {
		help = containerFilterHelp
	}
	cmd.Flags().StringArrayVarP(filters, "filter", "f", nil, help)
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalContainers)
}

func AddRunningFlag(cmd *cobra.Command, running *bool, help string) {
	cmd.Flags().BoolVar(running, "running", false, help)
}

func AddRedactFlags(cmd *cobra.Command, redactSecrets *bool, redactProfile *string, secretsHelp string) {
	cmd.Flags().BoolVar(redactSecrets, "redact-secrets", false, secretsHelp)
	cmd.Flags().StringVar(redactProfile, "redact-profile", "", redactProfileHelp)
}

func AddVolumeFilterFlag(cmd *cobra.Command, filters *[]string) {
	cmd.Flags().StringArrayVarP(filters, "filter", "f", nil, "筛选 volume，支持名称/driver/mountpoint/label 和 * ? 通配符，可重复指定")
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalVolumes)
}

func AddVolumeSizeFlags(cmd *cobra.Command, sizeMode *string, defaultMode string, sizeImage *string, defaultImage string) {
	cmd.Flags().StringVar(sizeMode, "size-mode", defaultMode, "volume 大小探测方式: api | local-go | docker-run | auto")
	cmd.Flags().StringVar(sizeImage, "size-image", defaultImage, "docker-run/auto 大小探测使用的 helper 镜像，必须已存在于目标 Docker")
}

func AddReportAllVolumeSizeFlags(cmd *cobra.Command, sizeMode *string, defaultMode string, sizeImage *string, defaultImage string) {
	cmd.Flags().StringVar(sizeMode, "volume-size-mode", defaultMode, "volumes 子报告大小探测方式: api | local-go | docker-run | auto")
	cmd.Flags().StringVar(sizeImage, "volume-size-image", defaultImage, "docker-run/auto 大小探测使用的 helper 镜像")
}

func AddPruneScopeFlags(cmd *cobra.Command, only *[]string, filters *[]string, until *string, protectLabels *[]string) {
	addPruneScopeFlags(cmd, "", only, filters, until, protectLabels, true)
}

func AddReportAllPruneScopeFlags(cmd *cobra.Command, only *[]string, filters *[]string, until *string, protectLabels *[]string) {
	addPruneScopeFlags(cmd, "prune-", only, filters, until, protectLabels, false)
}

func addPruneScopeFlags(cmd *cobra.Command, prefix string, only *[]string, filters *[]string, until *string, protectLabels *[]string, shorthand bool) {
	cmd.Flags().StringArrayVar(only, prefix+"only", nil, "只处理指定资源类型，可重复指定: container | image | volume | build-cache")
	if shorthand {
		cmd.Flags().StringArrayVarP(filters, prefix+"filter", "f", nil, "清理筛选条件，支持 label=key、label=key=value、label!=key、until=<duration|timestamp>，可重复指定")
	} else {
		cmd.Flags().StringArrayVar(filters, prefix+"filter", nil, "清理筛选条件，支持 label=key、label=key=value、label!=key、until=<duration|timestamp>，可重复指定")
	}
	cmd.Flags().StringVar(until, prefix+"until", "", "仅清理该时间之前创建的资源，例如 24h、168h 或 RFC3339 时间")
	cmd.Flags().StringArrayVar(protectLabels, prefix+"protect-label", nil, "保护带有指定 label 的资源，例如 keep 或 env=prod，可重复指定")
}

func AddRegistryClientFlags(cmd *cobra.Command, dockerConfig *string, plainHTTP *bool, timeout *time.Duration, timeoutDefault time.Duration) {
	AddDockerConfigFlag(cmd, dockerConfig)
	AddPlainHTTPFlag(cmd, plainHTTP)
	cmd.Flags().DurationVar(timeout, "timeout", timeoutDefault, "registry 连通性检查超时时间")
}

func AddDockerConfigFlag(cmd *cobra.Command, dockerConfig *string) {
	cmd.Flags().StringVar(dockerConfig, "docker-config", "", dockerConfigHelp)
}

func AddPlainHTTPFlag(cmd *cobra.Command, plainHTTP *bool) {
	cmd.Flags().BoolVar(plainHTTP, "plain-http", false, plainHTTPHelp)
}
