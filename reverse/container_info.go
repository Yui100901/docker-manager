package reverse

import (
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
)

type ContainerSpec struct {
	Image           string
	ContainerName   string
	Privileged      bool
	PublishAllPorts bool
	AutoRemove      bool
	RestartPolicy   string
	User            string
	Envs            []string
	Mounts          []string
	PortBindings    []string
	Cmd             []string
	Entrypoint      []string
	WorkingDir      string
	NetworkMode     string
}

func NewDockerSpec(ci *container.InspectResponse) *ContainerSpec {
	return &ContainerSpec{
		Image:           ci.Config.Image,
		ContainerName:   strings.TrimPrefix(ci.Name, "/"),
		Privileged:      ci.HostConfig.Privileged,
		PublishAllPorts: ci.HostConfig.PublishAllPorts,
		AutoRemove:      ci.HostConfig.AutoRemove,
		RestartPolicy:   parseRestartPolicy(ci),
		User:            ci.Config.User,
		Envs:            ci.Config.Env,
		Mounts:          parseMounts(ci),
		PortBindings:    parsePortBindings(ci),
		Cmd:             ci.Config.Cmd,
		Entrypoint:      ci.Config.Entrypoint,
		WorkingDir:      ci.Config.WorkingDir,
		NetworkMode:     string(ci.HostConfig.NetworkMode),
	}
}

func parseRestartPolicy(ci *container.InspectResponse) string {
	p := ci.HostConfig.RestartPolicy
	if p.Name == "on-failure" && p.MaximumRetryCount > 0 {
		return fmt.Sprintf("on-failure:%d", p.MaximumRetryCount)
	}
	return string(p.Name)
}

func parseMounts(ci *container.InspectResponse) []string {
	var mounts []string
	for _, m := range ci.Mounts {
		if m.Type == "volume" {
			mounts = append(mounts, m.Destination)
		} else {
			mode := ""
			if m.Mode != "" {
				mode = ":" + m.Mode
			}
			mounts = append(mounts, fmt.Sprintf("%s:%s%s", m.Source, m.Destination, mode))
		}
	}
	return mounts
}

func parsePortBindings(ci *container.InspectResponse) []string {
	var ports []string
	for port, bindings := range ci.HostConfig.PortBindings {
		for _, b := range bindings {
			host := b.HostIP
			if host == "" {
				host = "0.0.0.0"
			}
			ports = append(ports, fmt.Sprintf("%s:%s:%s", host, b.HostPort, port.Port()))
		}
	}
	return ports
}

func (dc *ContainerSpec) ToCommand() []string {
	cmd := []string{"docker", "run", "-d"}

	add := func(args ...string) {
		cmd = append(cmd, args...)
	}

	add("--name", dc.ContainerName)

	if dc.Privileged {
		add("--privileged")
	}
	if dc.PublishAllPorts {
		add("-P")
	}
	if dc.AutoRemove {
		add("--rm")
	}
	if dc.RestartPolicy != "" {
		add("--restart", dc.RestartPolicy)
	}
	if dc.User != "" {
		add("-u", dc.User)
	}

	if len(dc.Entrypoint) > 0 {
		add("--entrypoint", strings.Join(dc.Entrypoint, " "))
	}

	if dc.WorkingDir != "" {
		add("-w", dc.WorkingDir)
	}

	if dc.NetworkMode != "" && dc.NetworkMode != "default" {
		add("--network", dc.NetworkMode)
	}

	for _, e := range dc.Envs {
		add("-e", e)
	}
	for _, v := range dc.Mounts {
		add("-v", v)
	}
	for _, p := range dc.PortBindings {
		add("-p", p)
	}

	add(dc.Image)
	add(dc.Cmd...)

	return cmd
}

func (dc *ContainerSpec) ToComposeService() ComposeService {
	return ComposeService{
		Image:         dc.Image,
		ContainerName: dc.ContainerName,
		Privileged:    dc.Privileged,
		Restart:       dc.RestartPolicy,
		User:          dc.User,
		Environment:   dc.Envs,
		Volumes:       dc.Mounts,
		Ports:         dc.PortBindings,
		Entrypoint:    dc.Entrypoint,
		WorkingDir:    dc.WorkingDir,
		Command:       dc.Cmd,
		NetworkMode:   dc.NetworkMode,
	}
}
