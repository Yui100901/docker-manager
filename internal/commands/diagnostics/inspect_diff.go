package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

type inspectDiffDockerService interface {
	InspectContainer(ctx context.Context, name string) (container.InspectResponse, error)
}

var newInspectDiffDockerService = func() (inspectDiffDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerInspectDiffService{cli: cli}, nil
}

type dockerInspectDiffService struct {
	cli *mobyclient.Client
}

type InspectDiffOptions struct {
	RedactSecrets bool
	commandflags.FormatOptions
}

type InspectDiffReport struct {
	DockerEndpoint string             `json:"docker_endpoint"`
	LeftName       string             `json:"left_name"`
	RightName      string             `json:"right_name"`
	Added          []InspectDiffEntry `json:"added,omitempty"`
	Removed        []InspectDiffEntry `json:"removed,omitempty"`
	Changed        []InspectDiffEntry `json:"changed,omitempty"`
}

type InspectDiffEntry struct {
	Path  string `json:"path"`
	Left  string `json:"left,omitempty"`
	Right string `json:"right,omitempty"`
}

func NewInspectDiffCommand() *cobra.Command {
	opts := InspectDiffOptions{}
	cmd := &cobra.Command{
		Use:   "diff <containerA> <containerB>",
		Short: "对比两个容器的关键配置差异",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := runInspectDiff(cmd.Context(), args[0], args[1], opts)
			if err != nil {
				return fmt.Errorf("对比容器 inspect 失败: %w", err)
			}
			return rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printInspectDiffReport(w, report)
			})
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	cmd.Flags().BoolVar(&opts.RedactSecrets, "redact-secrets", false, "脱敏 env/label/cmd/entrypoint/healthcheck/log config 等字段中的疑似敏感信息，便于分享输出")
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runInspectDiff(ctx context.Context, leftName, rightName string, opts InspectDiffOptions) (InspectDiffReport, error) {
	svc, err := newInspectDiffDockerService()
	if err != nil {
		return InspectDiffReport{}, err
	}
	names := []string{leftName, rightName}
	inspects := make([]container.InspectResponse, len(names))
	errs := make([]error, len(names))
	runDiagnosticsParallel(ctx, len(names), len(names), func(ctx context.Context, i int) {
		inspect, err := svc.InspectContainer(ctx, names[i])
		if err != nil {
			errs[i] = err
			return
		}
		inspects[i] = inspect
	})
	if err := ctx.Err(); err != nil {
		return InspectDiffReport{}, err
	}
	for i, err := range errs {
		if err != nil {
			return InspectDiffReport{}, fmt.Errorf("inspect %s: %w", names[i], err)
		}
	}
	return buildInspectDiffReport(leftName, rightName, inspects[0], inspects[1], opts), nil
}

func buildInspectDiffReport(leftName, rightName string, left, right container.InspectResponse, opts InspectDiffOptions) InspectDiffReport {
	leftFields := inspectComparableFields(left, opts)
	rightFields := inspectComparableFields(right, opts)
	report := InspectDiffReport{DockerEndpoint: docker.Endpoint(), LeftName: leftName, RightName: rightName}

	seen := map[string]bool{}
	for path, leftValue := range leftFields {
		seen[path] = true
		rightValue, ok := rightFields[path]
		if !ok {
			report.Removed = append(report.Removed, InspectDiffEntry{Path: path, Left: leftValue})
			continue
		}
		if leftValue != rightValue {
			report.Changed = append(report.Changed, InspectDiffEntry{Path: path, Left: leftValue, Right: rightValue})
		}
	}
	for path, rightValue := range rightFields {
		if seen[path] {
			continue
		}
		report.Added = append(report.Added, InspectDiffEntry{Path: path, Right: rightValue})
	}
	sortInspectDiffEntries(report.Added)
	sortInspectDiffEntries(report.Removed)
	sortInspectDiffEntries(report.Changed)
	return report
}

func inspectComparableFields(info container.InspectResponse, opts InspectDiffOptions) map[string]string {
	fields := map[string]string{}
	add := func(path string, value interface{}) {
		if opts.RedactSecrets {
			value = redactInspectDiffValue(value)
		}
		fields[path] = inspectDiffValue(value)
	}

	if info.Config != nil {
		cfg := info.Config
		add("config.image", cfg.Image)
		add("config.user", cfg.User)
		add("config.working_dir", cfg.WorkingDir)
		add("config.hostname", cfg.Hostname)
		add("config.domainname", cfg.Domainname)
		add("config.cmd", []string(cfg.Cmd))
		add("config.entrypoint", []string(cfg.Entrypoint))
		add("config.healthcheck", comparableHealthcheck(cfg.Healthcheck))
		add("config.env", envMap(cfg.Env, opts.RedactSecrets))
		add("config.labels", cfg.Labels)
		add("config.exposed_ports", cfg.ExposedPorts)
		add("config.tty", cfg.Tty)
		add("config.open_stdin", cfg.OpenStdin)
		add("config.stop_signal", cfg.StopSignal)
	}

	if info.HostConfig != nil {
		host := info.HostConfig
		add("host.restart_policy", host.RestartPolicy)
		add("host.network_mode", host.NetworkMode)
		add("host.privileged", host.Privileged)
		add("host.auto_remove", host.AutoRemove)
		add("host.publish_all_ports", host.PublishAllPorts)
		add("host.port_bindings", host.PortBindings)
		add("host.binds", sortedStrings(host.Binds))
		add("host.dns", sortedNetIPAddrs(host.DNS))
		add("host.dns_search", sortedStrings(host.DNSSearch))
		add("host.extra_hosts", sortedStrings(host.ExtraHosts))
		add("host.cap_add", sortedStrings([]string(host.CapAdd)))
		add("host.cap_drop", sortedStrings([]string(host.CapDrop)))
		add("host.security_opt", sortedStrings(host.SecurityOpt))
		add("host.devices", host.Devices)
		add("host.ulimits", host.Ulimits)
		add("host.log_config", comparableLogConfig(host.LogConfig))
		add("host.memory", host.Memory)
		add("host.memory_reservation", host.MemoryReservation)
		add("host.memory_swap", host.MemorySwap)
		add("host.nano_cpus", host.NanoCPUs)
		add("host.cpu_shares", host.CPUShares)
		add("host.cpu_quota", host.CPUQuota)
		add("host.cpu_period", host.CPUPeriod)
		add("host.cpuset_cpus", host.CpusetCpus)
		add("host.shm_size", host.ShmSize)
		add("host.readonly_rootfs", host.ReadonlyRootfs)
		add("host.tmpfs", host.Tmpfs)
		add("host.sysctls", host.Sysctls)
		add("host.runtime", host.Runtime)
		add("host.ipc_mode", host.IpcMode)
		add("host.pid_mode", host.PidMode)
		add("host.userns_mode", host.UsernsMode)
	}

	add("mounts", comparableMounts(info.Mounts))
	if info.NetworkSettings != nil {
		add("networks", comparableNetworks(info.NetworkSettings.Networks))
	}
	return fields
}

func redactInspectDiffValue(value interface{}) interface{} {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		return redactSensitiveText(v)
	case []string:
		return redactStringSlice(v)
	case map[string]string:
		return redactStringMap(v)
	case map[string]interface{}:
		result := make(map[string]interface{}, len(v))
		for key, item := range v {
			if isSensitiveKey(key) {
				result[key] = redactedValue
			} else {
				result[key] = redactInspectDiffValue(item)
			}
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, item := range v {
			result[i] = redactInspectDiffValue(item)
		}
		return result
	case []map[string]interface{}:
		result := make([]map[string]interface{}, len(v))
		for i, item := range v {
			result[i] = redactInspectDiffValue(item).(map[string]interface{})
		}
		return result
	default:
		return value
	}
}

func comparableHealthcheck(health *container.HealthConfig) map[string]interface{} {
	if health == nil {
		return nil
	}
	return map[string]interface{}{
		"test":         append([]string(nil), health.Test...),
		"interval":     health.Interval,
		"timeout":      health.Timeout,
		"start_period": health.StartPeriod,
		"retries":      health.Retries,
	}
}

func comparableLogConfig(config container.LogConfig) map[string]interface{} {
	return map[string]interface{}{
		"type":   config.Type,
		"config": cloneStringMap(config.Config),
	}
}

func comparableMounts(mounts []container.MountPoint) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(mounts))
	for _, mount := range mounts {
		result = append(result, map[string]interface{}{
			"type":        mount.Type,
			"name":        mount.Name,
			"source":      mount.Source,
			"destination": mount.Destination,
			"driver":      mount.Driver,
			"mode":        mount.Mode,
			"rw":          mount.RW,
			"propagation": mount.Propagation,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return fmt.Sprint(result[i]["destination"]) < fmt.Sprint(result[j]["destination"])
	})
	return result
}

func comparableNetworks(networks map[string]*network.EndpointSettings) map[string]interface{} {
	result := map[string]interface{}{}
	for name, endpoint := range networks {
		if endpoint == nil {
			result[name] = nil
			continue
		}
		result[name] = map[string]interface{}{
			"ip_address":  endpoint.IPAddress,
			"ipam_config": endpoint.IPAMConfig,
			"links":       sortedStrings(endpoint.Links),
			"aliases":     sortedStrings(endpoint.Aliases),
			"mac_address": endpoint.MacAddress,
			"driver_opts": endpoint.DriverOpts,
		}
	}
	return result
}

func envMap(envs []string, redactSecrets bool) map[string]string {
	result := map[string]string{}
	for _, env := range envs {
		key, value, found := strings.Cut(env, "=")
		if !found {
			result[env] = ""
			continue
		}
		if redactSecrets && isSensitiveKey(key) {
			value = redactedValue
		} else if redactSecrets {
			value = redactSensitiveText(value)
		}
		result[key] = value
	}
	return result
}

func inspectDiffValue(value interface{}) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSpace(buf.String())
}

func sortedStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	result := append([]string(nil), items...)
	sort.Strings(result)
	return result
}

func sortedNetIPAddrs(items []netip.Addr) []string {
	if len(items) == 0 {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item.IsValid() {
			result = append(result, item.String())
		}
	}
	sort.Strings(result)
	return result
}

func sortInspectDiffEntries(entries []InspectDiffEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
}

func printInspectDiffReport(w io.Writer, report InspectDiffReport) {
	total := len(report.Added) + len(report.Removed) + len(report.Changed)
	fmt.Fprintf(w, "容器 inspect 差异: %s -> %s\n", report.LeftName, report.RightName)
	printDockerEndpoint(w, report.DockerEndpoint)
	fmt.Fprintf(w, "摘要: 变更=%d 新增=%d 删除=%d 总计=%d\n\n", len(report.Changed), len(report.Added), len(report.Removed), total)
	if total == 0 {
		fmt.Fprintln(w, "未发现可对比差异。")
		return
	}
	printInspectDiffSection(w, "变更", report.Changed, true)
	printInspectDiffSection(w, "右侧新增", report.Added, false)
	printInspectDiffSection(w, "右侧删除", report.Removed, false)
}

func printInspectDiffSection(w io.Writer, title string, entries []InspectDiffEntry, changed bool) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", title)
	for _, entry := range entries {
		fmt.Fprintf(w, "  - %s\n", entry.Path)
		if changed {
			fmt.Fprintf(w, "      左侧: %s\n", entry.Left)
			fmt.Fprintf(w, "      右侧: %s\n", entry.Right)
		} else if entry.Right != "" {
			fmt.Fprintf(w, "      值: %s\n", entry.Right)
		} else {
			fmt.Fprintf(w, "      值: %s\n", entry.Left)
		}
	}
	fmt.Fprintln(w)
}

func (s *dockerInspectDiffService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	result, err := s.cli.ContainerInspect(ctx, name, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return docker.ConvertDockerType[container.InspectResponse](result.Container)
}
