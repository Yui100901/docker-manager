package docker

import (
	"fmt"
	"github.com/Yui100901/MyGo/command"
	"github.com/Yui100901/MyGo/log_utils"
	"strings"
)

//
// @Author yfy2001
// @Date 2024/12/17 16 47
//

// ContainerStop 停止docker容器
func ContainerStop(containers ...string) error {
	log_utils.Info.Println("停止容器", containers)
	args := append([]string{"stop"}, containers...)
	return command.RunCommand("docker", args...)
}

// ContainerKill 强制停止docker容器
func ContainerKill(containers ...string) error {
	log_utils.Info.Println("强制停止容器", containers)
	args := append([]string{"kill"}, containers...)
	return command.RunCommand("docker", args...)
}

// ContainerRemove 删除docker容器
func ContainerRemove(containers ...string) error {
	log_utils.Info.Println("删除容器", containers)
	args := append([]string{"rm"}, containers...)
	return command.RunCommand("docker", args...)
}

// ContainerInspect 获取容器详细信息
func ContainerInspect(names ...string) (string, error) {
	log_utils.Info.Println("获取容器详细信息", names)
	args := append([]string{"container", "inspect"}, names...)
	output, err := command.RunCommandOutput("docker", args...)
	return output, err
}

// ImageListFormatted 获取docker镜像列表
func ImageListFormatted() (string, error) {
	log_utils.Info.Println("列出格式化的镜像列表")
	args := []string{"images", "--format", "{{.Repository}}:{{.Tag}}"}
	output, err := command.RunCommandOutput("docker", args...)
	return output, err
}

// ImageRemove 删除docker镜像
func ImageRemove(images ...string) error {
	log_utils.Info.Println("删除镜像", images)
	args := append([]string{"rmi"}, images...)
	return command.RunCommand("docker", args...)
}

// BuildImage 构建Docker镜像
func BuildImage(name string) error {
	log_utils.Info.Println("构建镜像", name)
	args := []string{"build", "-t", name, "."}
	return command.RunCommand("docker", args...)
}

// Save 导出Docker镜像
func Save(name, path string) error {
	log_utils.Info.Println("导出镜像", name)
	sanitizedFilename := strings.ReplaceAll(name, ":", "_")
	sanitizedFilename = strings.ReplaceAll(sanitizedFilename, "/", "_")
	filename := fmt.Sprintf("%s/%s.tar", path, sanitizedFilename)
	args := []string{"save", "-o", filename, name}
	return command.RunCommand("docker", args...)
}

// Load 导入Docker镜像
func Load(path string) error {
	log_utils.Info.Println("导入镜像", path)
	args := []string{"load", "-i", path}
	return command.RunCommand("docker", args...)
}

// ImagePrune 清理docker镜像
func ImagePrune() error {
	log_utils.Info.Println("清理镜像")
	args := []string{"image", "prune", "-f"}
	return command.RunCommand("docker", args...)
}

// DefaultRun 默认启动Docker容器
func DefaultRun(name string, ports []string, envs map[string]string) error {
	log_utils.Info.Println("默认启动", name)
	args := []string{
		"run",
		"-d",
		"--name", name,
		"-v", "/etc/localtime:/etc/localtime:ro",
	}
	for _, p := range ports {
		args = append(args, "-p", p+":"+p)
	}
	for k, v := range envs {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, name+":latest")
	return command.RunCommand("docker", args...)
}

// ContainerRerun 重新创建Docker容器
func ContainerRerun(name string, ports []string, envs map[string]string) error {
	if err := ContainerStop(name); err != nil {

	}
	if err := ContainerRemove(name); err != nil {

	}
	if err := DefaultRun(name, ports, envs); err != nil {

	}
	return nil
}
