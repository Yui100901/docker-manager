package docker

//
// @Author yfy2001
// @Date 2026/1/12 16 38
//

type ComposeFile struct {
	Services map[string]ComposeService `yaml:"services"`
}

type ComposeService struct {
	Image         string   `yaml:"image,omitempty"`
	ContainerName string   `yaml:"container_name,omitempty"`
	Privileged    bool     `yaml:"privileged,omitempty"`
	Restart       string   `yaml:"restart,omitempty"`
	User          string   `yaml:"user,omitempty"`
	Environment   []string `yaml:"environment,omitempty"`
	Volumes       []string `yaml:"volumes,omitempty"`
	Ports         []string `yaml:"ports,omitempty"`
	Command       []string `yaml:"command,omitempty"`
	Entrypoint    []string `yaml:"entrypoint,omitempty"`
	WorkingDir    string   `yaml:"working_dir,omitempty"`
	NetworkMode   string   `yaml:"network_mode,omitempty"`
}
