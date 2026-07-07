package diagnostics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

type healthDockerService interface {
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerLogs(ctx context.Context, id string, options mobyclient.ContainerLogsOptions) (io.ReadCloser, error)
}

var newHealthDockerService = func() (healthDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerHealthService{cli: cli}, nil
}

type dockerHealthService struct {
	cli *mobyclient.Client
}

type HealthOptions struct {
	RunningOnly      bool
	NoLogs           bool
	LogTail          int
	RestartThreshold int
	Keywords         []string
	ContainerFilters []string
	RedactSecrets    bool
	commandflags.FormatOptions
}

type HealthReport struct {
	GeneratedAt    string            `json:"generated_at"`
	DockerEndpoint string            `json:"docker_endpoint"`
	Target         TargetSelection   `json:"target"`
	Summary        HealthSummary     `json:"summary"`
	Containers     []HealthContainer `json:"containers"`
	Issues         []HealthIssue     `json:"issues,omitempty"`
}

type HealthSummary struct {
	Total           int `json:"total"`
	Running         int `json:"running"`
	Stopped         int `json:"stopped"`
	Restarting      int `json:"restarting"`
	Unhealthy       int `json:"unhealthy"`
	RestartWarnings int `json:"restart_warnings"`
	LogWarnings     int `json:"log_warnings"`
	LogsUnavailable int `json:"logs_unavailable"`
	PublicBindings  int `json:"public_bindings"`
}

type HealthContainer struct {
	ID                    string             `json:"id"`
	Name                  string             `json:"name"`
	Image                 string             `json:"image,omitempty"`
	ImageID               string             `json:"image_id,omitempty"`
	ImageDigest           string             `json:"image_digest,omitempty"`
	State                 string             `json:"state,omitempty"`
	Status                string             `json:"status,omitempty"`
	RestartCount          int                `json:"restart_count"`
	RestartPolicy         string             `json:"restart_policy,omitempty"`
	HealthStatus          string             `json:"health_status,omitempty"`
	FailingStreak         int                `json:"failing_streak,omitempty"`
	ExitCode              int                `json:"exit_code,omitempty"`
	Error                 string             `json:"error,omitempty"`
	PublicPorts           []string           `json:"public_ports,omitempty"`
	ExposedPorts          []string           `json:"exposed_ports,omitempty"`
	Ports                 []HealthPortRef    `json:"ports,omitempty"`
	Networks              []HealthNetworkRef `json:"networks,omitempty"`
	Mounts                []HealthMountRef   `json:"mounts,omitempty"`
	LogDriver             string             `json:"log_driver,omitempty"`
	LogOptions            map[string]string  `json:"log_options,omitempty"`
	LogReadability        string             `json:"log_readability,omitempty"`
	LogReadabilityMessage string             `json:"log_readability_message,omitempty"`
	NetworkMode           string             `json:"network_mode,omitempty"`
	LogMatches            []LogMatch         `json:"log_matches,omitempty"`
}

type HealthPortRef struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      uint16 `json:"host_port,omitempty"`
	ContainerPort uint16 `json:"container_port"`
	Protocol      string `json:"protocol"`
	Published     bool   `json:"published"`
	Source        string `json:"source,omitempty"`
}

type HealthNetworkRef struct {
	Name        string   `json:"name"`
	NetworkID   string   `json:"network_id,omitempty"`
	EndpointID  string   `json:"endpoint_id,omitempty"`
	IPAddress   string   `json:"ip_address,omitempty"`
	IPv6Address string   `json:"ipv6_address,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

type HealthMountRef struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	Mode        string `json:"mode,omitempty"`
	RW          bool   `json:"rw"`
}

type LogMatch struct {
	Line     string   `json:"line"`
	Keywords []string `json:"keywords"`
}

type HealthIssue struct {
	Severity  string `json:"severity"`
	Container string `json:"container,omitempty"`
	Type      string `json:"type"`
	Message   string `json:"message"`
}

func NewHealthCommand() *cobra.Command {
	opts := HealthOptions{
		LogTail:          100,
		RestartThreshold: 3,
		Keywords:         []string{"error", "panic", "exception", "fatal", "oom", "killed"},
	}
	cmd := &cobra.Command{
		Use:   "health [container-pattern...]",
		Short: "输出本机 Docker 体检报告",
		RunE: func(cmd *cobra.Command, args []string) error {
			runOpts := opts
			runOpts.ContainerFilters = append(append([]string(nil), opts.ContainerFilters...), args...)
			report, err := runHealthReport(cmd.Context(), runOpts)
			if err != nil {
				return fmt.Errorf("生成体检报告失败: %w", err)
			}
			return rpt.Print(cmd.OutOrStdout(), runOpts.Format, report, func(w io.Writer) {
				printHealthReport(w, report)
			})
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	cmd.Flags().BoolVar(&opts.RunningOnly, "running", false, "只检查正在运行的容器")
	cmd.Flags().BoolVar(&opts.NoLogs, "no-logs", false, "不扫描容器日志")
	cmd.Flags().IntVar(&opts.LogTail, "log-tail", opts.LogTail, "每个容器扫描最近日志行数")
	cmd.Flags().IntVar(&opts.RestartThreshold, "restart-threshold", opts.RestartThreshold, "restart 次数达到该阈值时报告风险")
	cmd.Flags().StringArrayVar(&opts.Keywords, "keyword", opts.Keywords, "日志扫描关键词，可重复指定")
	cmd.Flags().StringArrayVarP(&opts.ContainerFilters, "filter", "f", nil, "筛选容器，支持 name:/id:/image:/state:/status:/label: 和 * ? 通配符，可重复指定")
	cmd.Flags().BoolVar(&opts.RedactSecrets, "redact-secrets", false, "脱敏日志命中行中的疑似敏感信息，便于分享输出")
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalContainers)
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runHealthReport(ctx context.Context, opts HealthOptions) (HealthReport, error) {
	svc, err := newHealthDockerService()
	if err != nil {
		return HealthReport{}, err
	}
	containers, err := svc.ListContainers(ctx, !opts.RunningOnly)
	if err != nil {
		return HealthReport{}, err
	}
	containers = filterContainerSummaries(containers, opts.ContainerFilters)
	report := buildHealthReport(ctx, svc, containers, opts)
	report.Target = buildContainerTargetSelection("检查", len(containers), opts.RunningOnly, opts.ContainerFilters)
	return report, nil
}

func buildHealthReport(ctx context.Context, svc healthDockerService, containers []container.Summary, opts HealthOptions) HealthReport {
	report := HealthReport{GeneratedAt: time.Now().Format(time.RFC3339), DockerEndpoint: docker.Endpoint()}
	keywords := normalizeKeywords(opts.Keywords)

	for _, summary := range containers {
		name := firstContainerName(summary.Names)
		if name == "" {
			name = shortID(summary.ID)
		}
		ref := summary.ID
		if ref == "" {
			ref = name
		}
		item := HealthContainer{
			ID:     shortID(summary.ID),
			Name:   name,
			Image:  summary.Image,
			State:  string(summary.State),
			Status: summary.Status,
		}
		report.Summary.Total++
		switch summary.State {
		case "running":
			report.Summary.Running++
		case "restarting":
			report.Summary.Restarting++
		default:
			report.Summary.Stopped++
		}

		inspect, err := svc.InspectContainer(ctx, ref)
		if err != nil {
			report.Issues = append(report.Issues, HealthIssue{
				Severity:  "error",
				Container: name,
				Type:      "inspect-failed",
				Message:   fmt.Sprintf("inspect 失败: %v", err),
			})
			applyHealthPorts(&item, summary, container.InspectResponse{}, false)
			report.Containers = append(report.Containers, item)
			continue
		}
		applyInspectHealth(&item, inspect)
		applyHealthPorts(&item, summary, inspect, true)
		addStateIssues(&report, item, inspect, opts)
		addLogDriverIssue(&report, item)

		for _, port := range item.PublicPorts {
			report.Summary.PublicBindings++
			report.Issues = append(report.Issues, HealthIssue{
				Severity:  "warn",
				Container: item.Name,
				Type:      "public-port",
				Message:   fmt.Sprintf("public port binding: %s", port),
			})
		}

		if !opts.NoLogs && item.LogReadability != "disabled" && item.LogReadability != "unsupported" && opts.LogTail != 0 && len(keywords) > 0 {
			matches, err := scanHealthLogs(ctx, svc, ref, inspect, opts.LogTail, keywords, opts.RedactSecrets)
			if err != nil {
				report.Issues = append(report.Issues, HealthIssue{
					Severity:  "warn",
					Container: item.Name,
					Type:      "logs-unavailable",
					Message:   fmt.Sprintf("扫描日志失败: %v", err),
				})
			} else if len(matches) > 0 {
				item.LogMatches = matches
				report.Summary.LogWarnings += len(matches)
				report.Issues = append(report.Issues, HealthIssue{
					Severity:  "warn",
					Container: item.Name,
					Type:      "log-keyword",
					Message:   fmt.Sprintf("matched %d recent log lines", len(matches)),
				})
			}
		}

		report.Containers = append(report.Containers, item)
	}
	sortHealthReport(&report)
	return report
}

func applyInspectHealth(item *HealthContainer, inspect container.InspectResponse) {
	if item.ID == "" {
		item.ID = shortID(inspect.ID)
	}
	if item.Name == "" {
		item.Name = normalizeContainerName(inspect.Name)
	}
	item.RestartCount = inspect.RestartCount
	if inspect.Image != "" {
		item.ImageID = shortID(inspect.Image)
	}
	if inspect.Config != nil {
		if item.Image == "" {
			item.Image = inspect.Config.Image
		}
	}
	if inspect.ImageManifestDescriptor != nil && inspect.ImageManifestDescriptor.Digest != "" {
		item.ImageDigest = inspect.ImageManifestDescriptor.Digest.String()
	}
	if hostConfig := healthHostConfig(inspect); hostConfig != nil {
		item.RestartPolicy = healthRestartPolicy(hostConfig.RestartPolicy)
		item.LogDriver = hostConfig.LogConfig.Type
		item.LogOptions = cloneStringMap(hostConfig.LogConfig.Config)
		item.NetworkMode = string(hostConfig.NetworkMode)
	}
	availability := containerLogDriverAvailability(inspect)
	item.LogDriver = availability.Driver
	item.LogReadability = availability.Status
	item.LogReadabilityMessage = availability.Reason
	item.Networks = healthNetworks(inspect)
	item.Mounts = healthMounts(inspect)
	if inspect.State == nil {
		return
	}
	if inspect.State.Status != "" {
		item.State = string(inspect.State.Status)
	}
	item.ExitCode = inspect.State.ExitCode
	item.Error = inspect.State.Error
	if inspect.State.Health != nil {
		item.HealthStatus = string(inspect.State.Health.Status)
		item.FailingStreak = inspect.State.Health.FailingStreak
	}
}

func healthHostConfig(inspect container.InspectResponse) *container.HostConfig {
	return inspect.HostConfig
}

func applyHealthPorts(item *HealthContainer, summary container.Summary, inspect container.InspectResponse, hasInspect bool) {
	mappings := networkPortMappings(summary, inspect, hasInspect, item.Name)
	if hasInspect && len(mappings) == 0 {
		mappings = networkPortMappings(summary, container.InspectResponse{}, false, item.Name)
	}
	item.Ports = make([]HealthPortRef, 0, len(mappings))
	item.PublicPorts = nil
	item.ExposedPorts = nil
	for _, mapping := range mappings {
		item.Ports = append(item.Ports, HealthPortRef{
			HostIP:        mapping.HostIP,
			HostPort:      mapping.HostPort,
			ContainerPort: mapping.ContainerPort,
			Protocol:      mapping.Protocol,
			Published:     mapping.Published,
			Source:        mapping.Source,
		})
		if mapping.Published && isPublicHostIP(mapping.HostIP) {
			item.PublicPorts = append(item.PublicPorts, fmt.Sprintf("%s:%d->%d/%s", mapping.HostIP, mapping.HostPort, mapping.ContainerPort, mapping.Protocol))
			continue
		}
		if !mapping.Published {
			item.ExposedPorts = append(item.ExposedPorts, fmt.Sprintf("%d/%s", mapping.ContainerPort, mapping.Protocol))
		}
	}
	sort.Strings(item.PublicPorts)
	sort.Strings(item.ExposedPorts)
}

func healthRestartPolicy(policy container.RestartPolicy) string {
	if policy.Name == "" {
		return ""
	}
	if policy.Name == "on-failure" && policy.MaximumRetryCount > 0 {
		return fmt.Sprintf("%s:%d", policy.Name, policy.MaximumRetryCount)
	}
	return string(policy.Name)
}

func healthNetworks(inspect container.InspectResponse) []HealthNetworkRef {
	if inspect.NetworkSettings == nil || len(inspect.NetworkSettings.Networks) == 0 {
		return nil
	}
	networks := make([]HealthNetworkRef, 0, len(inspect.NetworkSettings.Networks))
	for name, endpoint := range inspect.NetworkSettings.Networks {
		ref := HealthNetworkRef{Name: name}
		if endpoint != nil {
			ref.NetworkID = shortID(endpoint.NetworkID)
			ref.EndpointID = endpoint.EndpointID
			ref.IPAddress = formatNetworkValue(endpoint.IPAddress)
			ref.IPv6Address = formatNetworkValue(endpoint.GlobalIPv6Address)
			ref.Gateway = formatNetworkValue(endpoint.Gateway)
			ref.Aliases = sortedStrings(endpoint.Aliases)
		}
		networks = append(networks, ref)
	}
	sort.Slice(networks, func(i, j int) bool {
		return networks[i].Name < networks[j].Name
	})
	return networks
}

func healthMounts(inspect container.InspectResponse) []HealthMountRef {
	if len(inspect.Mounts) == 0 {
		return nil
	}
	mounts := make([]HealthMountRef, 0, len(inspect.Mounts))
	for _, m := range inspect.Mounts {
		mounts = append(mounts, HealthMountRef{
			Type:        string(m.Type),
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        m.Mode,
			RW:          m.RW,
		})
	}
	sort.Slice(mounts, func(i, j int) bool {
		return mounts[i].Destination < mounts[j].Destination
	})
	return mounts
}

func addStateIssues(report *HealthReport, item HealthContainer, inspect container.InspectResponse, opts HealthOptions) {
	if item.RestartCount >= opts.RestartThreshold && opts.RestartThreshold > 0 {
		report.Summary.RestartWarnings++
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "warn",
			Container: item.Name,
			Type:      "restart-count",
			Message:   fmt.Sprintf("restart count is %d", item.RestartCount),
		})
	}
	if item.HealthStatus == "unhealthy" {
		report.Summary.Unhealthy++
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "error",
			Container: item.Name,
			Type:      "unhealthy",
			Message:   fmt.Sprintf("healthcheck unhealthy, failing streak=%d", item.FailingStreak),
		})
	} else if item.HealthStatus == "starting" {
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "warn",
			Container: item.Name,
			Type:      "health-starting",
			Message:   "healthcheck is still starting",
		})
	}
	if item.State == "restarting" || item.State == "dead" {
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "error",
			Container: item.Name,
			Type:      "bad-state",
			Message:   fmt.Sprintf("container state is %s", item.State),
		})
	}
	if inspect.State != nil && inspect.State.OOMKilled {
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "error",
			Container: item.Name,
			Type:      "oom-killed",
			Message:   "container was OOM killed",
		})
	}
	if item.State != "running" && item.ExitCode != 0 {
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "warn",
			Container: item.Name,
			Type:      "non-zero-exit",
			Message:   fmt.Sprintf("container exited with code %d", item.ExitCode),
		})
	}
}

func addLogDriverIssue(report *HealthReport, item HealthContainer) {
	if item.LogReadability != "disabled" && item.LogReadability != "unsupported" {
		return
	}
	report.Summary.LogsUnavailable++
	report.Issues = append(report.Issues, HealthIssue{
		Severity:  "warn",
		Container: item.Name,
		Type:      "logs-unavailable",
		Message:   item.LogReadabilityMessage,
	})
}

func scanHealthLogs(ctx context.Context, svc healthDockerService, id string, inspect container.InspectResponse, tail int, keywords []string, redactSecrets bool) ([]LogMatch, error) {
	if availability := containerLogDriverAvailability(inspect); !availability.Readable {
		return nil, fmt.Errorf("%s", availability.Reason)
	}
	tailValue := strconv.Itoa(tail)
	if tail < 0 {
		tailValue = "all"
	}
	reader, err := svc.ContainerLogs(ctx, id, mobyclient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tailValue,
	})
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	text, err := readDockerLogs(reader, inspect.Config != nil && inspect.Config.Tty)
	if err != nil {
		return nil, err
	}
	matches := findLogMatches(text, keywords)
	if redactSecrets {
		for i := range matches {
			matches[i].Line = redactSensitiveText(matches[i].Line)
		}
	}
	return matches, nil
}

func readDockerLogs(reader io.Reader, tty bool) (string, error) {
	return readDockerLogsWithContext(context.Background(), reader, tty)
}

func readDockerLogsWithContext(ctx context.Context, reader io.Reader, tty bool) (string, error) {
	data, err := readAllWithContext(ctx, reader)
	if err != nil {
		return "", err
	}
	if tty {
		return string(data), nil
	}
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, bytes.NewReader(data)); err != nil {
		return string(data), nil
	}
	return stdout.String() + stderr.String(), nil
}

func readAllWithContext(ctx context.Context, reader io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := reader.Read(chunk)
		if n > 0 {
			if _, writeErr := buf.Write(chunk[:n]); writeErr != nil {
				return nil, writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				return buf.Bytes(), nil
			}
			return nil, err
		}
	}
}

func findLogMatches(text string, keywords []string) []LogMatch {
	var matches []LogMatch
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		var found []string
		for _, keyword := range keywords {
			if strings.Contains(lower, keyword) {
				found = append(found, keyword)
			}
		}
		if len(found) > 0 {
			matches = append(matches, LogMatch{Line: line, Keywords: found})
		}
	}
	return matches
}

func publicPortBindings(ports []container.PortSummary) []string {
	var result []string
	for _, port := range ports {
		if port.PublicPort == 0 {
			continue
		}
		hostIP := normalizeHostIP(port.IP)
		if !isPublicHostIP(hostIP) {
			continue
		}
		result = append(result, fmt.Sprintf("%s:%d->%d/%s", hostIP, port.PublicPort, port.PrivatePort, port.Type))
	}
	sort.Strings(result)
	return result
}

func normalizeKeywords(keywords []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, keyword := range keywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword == "" || seen[keyword] {
			continue
		}
		seen[keyword] = true
		result = append(result, keyword)
	}
	sort.Strings(result)
	return result
}

func sortHealthReport(report *HealthReport) {
	sort.Slice(report.Containers, func(i, j int) bool {
		return report.Containers[i].Name < report.Containers[j].Name
	})
	sort.Slice(report.Issues, func(i, j int) bool {
		if report.Issues[i].Severity == report.Issues[j].Severity {
			if report.Issues[i].Type == report.Issues[j].Type {
				return report.Issues[i].Container < report.Issues[j].Container
			}
			return report.Issues[i].Type < report.Issues[j].Type
		}
		return report.Issues[i].Severity < report.Issues[j].Severity
	})
}
