package docker

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

//
// @Author yfy2001
// @Date 2024/12/17 16 53
//

// Mount 定义 Mount 结构体
type Mount struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
}

// PortBinding 定义 PortBinding 结构体
type PortBinding struct {
	HostPort string `json:"HostPort"`
}

// RestartPolicy 定义 RestartPolicy 结构体
type RestartPolicy struct {
	Name string `json:"Name"`
}

// HostConfig 定义 HostConfig 结构体
type HostConfig struct {
	PortBindings    map[string][]PortBinding `json:"PortBindings"`
	RestartPolicy   RestartPolicy            `json:"RestartPolicy"`
	AutoRemove      bool                     `json:"AutoRemove"`
	Privileged      bool                     `json:"Privileged"`
	PublishAllPorts bool                     `json:"PublishAllPorts"`
	Runtime         string                   `json:"Runtime"`
}

// Config 定义 Config 结构体
type Config struct {
	User  *string  `json:"User"`
	Env   []string `json:"Env"`
	Cmd   []string `json:"Cmd"`
	Image string   `json:"Image"`
}

// ContainerInfo 定义 ContainerInfo 结构体
type ContainerInfo struct {
	Name       string     `json:"Name"`
	Config     Config     `json:"Config"`
	HostConfig HostConfig `json:"HostConfig"`
	Mounts     []Mount    `json:"Mounts"`
}

// GenerateContainerInfoList 获取docker容器信息信息并序列化为对象
func GenerateContainerInfoList(names ...string) ([]ContainerInfo, error) {
	data, err := ContainerInspect(names...)
	if err != nil {
		return nil, err
	}
	var containerInfoList []ContainerInfo
	cleanData := strings.ReplaceAll(data, "\n", "")
	if err := json.Unmarshal([]byte(cleanData), &containerInfoList); err != nil {
		return nil, err
	}
	return containerInfoList, nil
}

func (ci *ContainerInfo) ParseContainerName() string {
	return strings.TrimPrefix(ci.Name, "/")
}

func (ci *ContainerInfo) ParsePrivileged() bool {
	return ci.HostConfig.Privileged
}

func (ci *ContainerInfo) ParsePublishAllPorts() bool {
	return ci.HostConfig.PublishAllPorts
}

func (ci *ContainerInfo) ParseAutoRemove() bool {
	return ci.HostConfig.AutoRemove
}

func (ci *ContainerInfo) ParseUser() string {
	if ci.Config.User != nil && *ci.Config.User != "" {
		return *ci.Config.User
	}
	return ""
}

func (ci *ContainerInfo) ParseEnvs() []string {
	return ci.Config.Env
}

func (ci *ContainerInfo) ParseMounts() []string {
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

func (ci *ContainerInfo) ParsePortBindings() []string {
	var portBindings []string
	for port, bindings := range ci.HostConfig.PortBindings {
		for _, binding := range bindings {
			portBindings = append(portBindings, fmt.Sprintf("%s:%s", binding.HostPort, port))
		}
	}
	return portBindings
}

func (ci *ContainerInfo) ParseRestartPolicy() string {
	return ci.HostConfig.RestartPolicy.Name
}

func (ci *ContainerInfo) ParseImage() string {
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
}

// NewDockerCommand 从 ContainerInfo 创建 DockerCommand 实例
func NewDockerCommand(info *ContainerInfo) *DockerCommand {
	return &DockerCommand{
		ContainerName:   info.ParseContainerName(),
		Privileged:      info.ParsePrivileged(),
		PublishAllPorts: info.ParsePublishAllPorts(),
		AutoRemove:      info.ParseAutoRemove(),
		RestartPolicy:   info.ParseRestartPolicy(),
		User:            info.ParseUser(),
		Envs:            info.ParseEnvs(),
		Mounts:          info.ParseMounts(),
		PortBindings:    info.ParsePortBindings(),
		Image:           info.ParseImage(),
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
	//设置环境变量
	for _, env := range dc.Envs {
		command = append(command, "-e", env)
	}
	//设置卷挂载
	for _, mount := range dc.Mounts {
		command = append(command, "-v", mount)
	}
	//设置端口映射
	for _, portBinding := range dc.PortBindings {
		command = append(command, "-p", portBinding)
	}
	command = append(command, dc.Image)
	return command
}
