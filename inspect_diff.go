package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

type inspectDiffDockerService interface {
	InspectContainer(ctx context.Context, name string) (container.InspectResponse, error)
}

var newInspectDiffDockerService = func() (inspectDiffDockerService, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &dockerInspectDiffService{cli: cli}, nil
}

type dockerInspectDiffService struct {
	cli *client.Client
}

type InspectDiffOptions struct {
	ShowSecrets bool
}

type InspectDiffReport struct {
	LeftName  string             `json:"left_name"`
	RightName string             `json:"right_name"`
	Added     []InspectDiffEntry `json:"added,omitempty"`
	Removed   []InspectDiffEntry `json:"removed,omitempty"`
	Changed   []InspectDiffEntry `json:"changed,omitempty"`
}

type InspectDiffEntry struct {
	Path  string `json:"path"`
	Left  string `json:"left,omitempty"`
	Right string `json:"right,omitempty"`
}

func newInspectDiffCommand() *cobra.Command {
	opts := InspectDiffOptions{}
	cmd := &cobra.Command{
		Use:   "inspect-diff <containerA> <containerB>",
		Short: "对比两个容器的关键配置差异",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := runInspectDiff(cmd.Context(), args[0], args[1], opts)
			if err != nil {
				return fmt.Errorf("inspect diff failed: %w", err)
			}
			printInspectDiffReport(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.ShowSecrets, "show-secrets", false, "显示 env/label 中疑似敏感字段的真实值")
	return cmd
}

func runInspectDiff(ctx context.Context, leftName, rightName string, opts InspectDiffOptions) (InspectDiffReport, error) {
	svc, err := newInspectDiffDockerService()
	if err != nil {
		return InspectDiffReport{}, err
	}
	left, err := svc.InspectContainer(ctx, leftName)
	if err != nil {
		return InspectDiffReport{}, fmt.Errorf("inspect %s: %w", leftName, err)
	}
	right, err := svc.InspectContainer(ctx, rightName)
	if err != nil {
		return InspectDiffReport{}, fmt.Errorf("inspect %s: %w", rightName, err)
	}
	return buildInspectDiffReport(leftName, rightName, left, right, opts), nil
}

func buildInspectDiffReport(leftName, rightName string, left, right container.InspectResponse, opts InspectDiffOptions) InspectDiffReport {
	leftFields := inspectComparableFields(left, opts)
	rightFields := inspectComparableFields(right, opts)
	report := InspectDiffReport{LeftName: leftName, RightName: rightName}

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
		add("config.env", envMap(cfg.Env, opts.ShowSecrets))
		add("config.labels", redactStringMap(cfg.Labels, opts.ShowSecrets))
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
		add("host.dns", sortedStrings(host.DNS))
		add("host.dns_search", sortedStrings(host.DNSSearch))
		add("host.extra_hosts", sortedStrings(host.ExtraHosts))
		add("host.cap_add", sortedStrings([]string(host.CapAdd)))
		add("host.cap_drop", sortedStrings([]string(host.CapDrop)))
		add("host.security_opt", sortedStrings(host.SecurityOpt))
		add("host.devices", host.Devices)
		add("host.ulimits", host.Ulimits)
		add("host.log_config", host.LogConfig)
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

func envMap(envs []string, showSecrets bool) map[string]string {
	result := map[string]string{}
	for _, env := range envs {
		key, value, found := strings.Cut(env, "=")
		if !found {
			result[env] = ""
			continue
		}
		if !showSecrets && isSensitiveKey(key) {
			value = "<redacted>"
		}
		result[key] = value
	}
	return result
}

func redactStringMap(values map[string]string, showSecrets bool) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		if !showSecrets && isSensitiveKey(key) {
			value = "<redacted>"
		}
		result[key] = value
	}
	return result
}

func isSensitiveKey(key string) bool {
	key = strings.ToLower(key)
	for _, needle := range []string{"password", "passwd", "secret", "token", "credential", "auth", "private_key", "apikey", "api_key"} {
		if strings.Contains(key, needle) {
			return true
		}
	}
	return false
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

func sortInspectDiffEntries(entries []InspectDiffEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
}

func printInspectDiffReport(w io.Writer, report InspectDiffReport) {
	total := len(report.Added) + len(report.Removed) + len(report.Changed)
	fmt.Fprintf(w, "Container inspect diff: %s -> %s\n", report.LeftName, report.RightName)
	fmt.Fprintf(w, "Summary: changed=%d added=%d removed=%d total=%d\n\n", len(report.Changed), len(report.Added), len(report.Removed), total)
	if total == 0 {
		fmt.Fprintln(w, "No comparable differences found.")
		return
	}
	printInspectDiffSection(w, "Changed", report.Changed, true)
	printInspectDiffSection(w, "Added in right", report.Added, false)
	printInspectDiffSection(w, "Removed from right", report.Removed, false)
}

func printInspectDiffSection(w io.Writer, title string, entries []InspectDiffEntry, changed bool) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", title)
	for _, entry := range entries {
		fmt.Fprintf(w, "  - %s\n", entry.Path)
		if changed {
			fmt.Fprintf(w, "      left:  %s\n", entry.Left)
			fmt.Fprintf(w, "      right: %s\n", entry.Right)
		} else if entry.Right != "" {
			fmt.Fprintf(w, "      value: %s\n", entry.Right)
		} else {
			fmt.Fprintf(w, "      value: %s\n", entry.Left)
		}
	}
	fmt.Fprintln(w)
}

func (s *dockerInspectDiffService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, name)
}
