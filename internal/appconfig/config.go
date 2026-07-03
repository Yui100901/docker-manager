package appconfig

import (
	"errors"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultPath = ".dm.yaml"
const EnvName = "DM_CONFIG"

type Config struct {
	Proxy            string `yaml:"proxy"`
	TargetOS         string `yaml:"os"`
	Arch             string `yaml:"arch"`
	OutputDir        string `yaml:"output_dir"`
	DockerHost       string `yaml:"docker_host"`
	DockerTLSVerify  *bool  `yaml:"docker_tls_verify"`
	DockerCertPath   string `yaml:"docker_cert_path"`
	DockerAPIVersion string `yaml:"docker_api_version"`
	Verbose          bool   `yaml:"verbose"`
	Quiet            bool   `yaml:"quiet"`
	JSON             bool   `yaml:"log_json"`
}

func Load(path string) (Config, error) {
	var cfg Config
	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func ResolvePath(path string, flagChanged bool) string {
	if flagChanged {
		if strings.TrimSpace(path) == "" {
			return DefaultPath
		}
		return path
	}
	if envPath := strings.TrimSpace(os.Getenv(EnvName)); envPath != "" {
		return envPath
	}
	if strings.TrimSpace(path) == "" {
		return DefaultPath
	}
	return path
}
