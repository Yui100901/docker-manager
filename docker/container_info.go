package docker

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/container"
)

func ParseContainerName(ci *container.InspectResponse) string {
	return strings.TrimPrefix(ci.Name, "/")
}

func ParsePrivileged(ci *container.InspectResponse) bool {
	return ci.HostConfig.Privileged
}

func ParsePublishAllPorts(ci *container.InspectResponse) bool {
	return ci.HostConfig.PublishAllPorts
}

func ParseAutoRemove(ci *container.InspectResponse) bool {
	return ci.HostConfig.AutoRemove
}

func ParseUser(ci *container.InspectResponse) string {
	return ci.Config.User
}

func ParseEnvs(ci *container.InspectResponse) []string {
	return ci.Config.Env
}

func ParseMounts(ci *container.InspectResponse) []string {
	var mounts []string
	for _, mount := range ci.Mounts {
		if !filepath.IsAbs(mount.Destination) {
			// 非绝对路径时挂载匿名卷
			mounts = append(mounts, mount.Destination)
		} else {
			volume := fmt.Sprintf("%s:%s%s", mount.Source, mount.Destination,
				func() string {
					if mount.Mode == "" {
						return ""
					}
					return fmt.Sprintf(":%s", mount.Mode)
				}())
			mounts = append(mounts, volume)
		}
	}
	return mounts
}

func ParsePortBindings(ci *container.InspectResponse) []string {
	var portBindings []string
	for port, bindings := range ci.HostConfig.PortBindings {
		for _, binding := range bindings {
			// port.Port() 提取纯端口号
			portBindings = append(portBindings, fmt.Sprintf("%s:%s", binding.HostPort, port.Port()))
		}
	}
	return portBindings
}

func ParseRestartPolicy(ci *container.InspectResponse) string {
	return string(ci.HostConfig.RestartPolicy.Name)
}

func ParseImage(ci *container.InspectResponse) string {
	return ci.Config.Image
}

// DockerCommand 定义 DockerCommand 结构体
type DockerCommand struct {
	ContainerName   string
	Privileged      bool
	PublishAllPorts bool
	AutoRemove      bool
	RestartPolicy   string
	User            string
	Envs            []string
	Mounts          []string
	PortBindings    []string
	Image           string
	Cmd             []string
}

// NewDockerCommand 从 container.InspectResponse 创建 DockerCommand 实例
func NewDockerCommand(info *container.InspectResponse) *DockerCommand {
	return &DockerCommand{
		ContainerName:   ParseContainerName(info),
		Privileged:      ParsePrivileged(info),
		PublishAllPorts: ParsePublishAllPorts(info),
		AutoRemove:      ParseAutoRemove(info),
		RestartPolicy:   ParseRestartPolicy(info),
		User:            ParseUser(info),
		Envs:            ParseEnvs(info),
		Mounts:          ParseMounts(info),
		PortBindings:    ParsePortBindings(info),
		Image:           ParseImage(info),
		Cmd:             info.Config.Cmd, // 新增：容器启动命令
	}
}

// ToCommand 将 DockerCommand 转换为命令行参数
func (dc *DockerCommand) ToCommand() []string {
	var command []string
	command = append(command, "docker", "run", "-d")
	command = append(command, "--name", dc.ContainerName)

	if dc.Privileged {
		command = append(command, "--privileged")
	}
	if dc.PublishAllPorts {
		command = append(command, "-P")
	}
	if dc.AutoRemove {
		command = append(command, "--rm")
	}
	if dc.RestartPolicy != "" {
		command = append(command, "--restart", dc.RestartPolicy)
	}
	if dc.User != "" {
		command = append(command, "-u", dc.User)
	}
	// 设置环境变量
	for _, env := range dc.Envs {
		command = append(command, "-e", env)
	}
	// 设置卷挂载
	for _, mount := range dc.Mounts {
		command = append(command, "-v", mount)
	}
	// 设置端口映射
	for _, portBinding := range dc.PortBindings {
		command = append(command, "-p", portBinding)
	}

	// 镜像
	if dc.Image != "" {
		command = append(command, dc.Image)
	}

	// 容器启动命令
	if len(dc.Cmd) > 0 {
		command = append(command, dc.Cmd...)
	}

	return command
}
