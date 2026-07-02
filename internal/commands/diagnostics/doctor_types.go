package diagnostics

import (
	"time"

	rpt "docker-manager/internal/report"
)

type DoctorOptions struct {
	Registries    []string
	PlainHTTP     bool
	DockerConfig  string
	ConfigPath    string
	OutputDir     string
	Timeout       time.Duration
	CheckE2E      bool
	MinDiskFreeMB int64
	rpt.FormatOptions
}

type DoctorDefaults struct {
	ConfigPath string
	OutputDir  string
}

type DoctorReport struct {
	GeneratedAt     string        `json:"generated_at"`
	Platform        string        `json:"platform"`
	OverallStatus   string        `json:"overall_status"`
	Checks          []DoctorCheck `json:"checks"`
	Recommendations []string      `json:"recommendations,omitempty"`
}

type DoctorCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Recommended string `json:"recommended,omitempty"`
}

type doctorConfig struct {
	Proxy            string `yaml:"proxy"`
	TargetOS         string `yaml:"os"`
	Arch             string `yaml:"arch"`
	OutputDir        string `yaml:"output_dir"`
	DockerHost       string `yaml:"docker_host"`
	DockerTLSVerify  *bool  `yaml:"docker_tls_verify"`
	DockerCertPath   string `yaml:"docker_cert_path"`
	DockerAPIVersion string `yaml:"docker_api_version"`
	CAFile           string `yaml:"ca_file"`
	CAPath           string `yaml:"ca_path"`
	RegistryCAFile   string `yaml:"registry_ca_file"`
	RegistryCAPath   string `yaml:"registry_ca_path"`
	Verbose          bool   `yaml:"verbose"`
	Quiet            bool   `yaml:"quiet"`
	JSON             bool   `yaml:"json"`
	LogJSON          bool   `yaml:"log_json"`
}
