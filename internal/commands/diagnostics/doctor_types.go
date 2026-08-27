package diagnostics

import (
	"time"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/commandflags"
)

type DoctorOptions struct {
	Registries               []string
	PlainHTTP                bool
	Proxy                    string
	NoProxy                  bool
	RegistryCAFile           string
	RegistryCAPath           string
	DockerConfig             string
	ConfigPath               string
	LoadedConfig             *appconfig.Loaded
	ConfigLoadError          error
	OutputDir                string
	Timeout                  time.Duration
	CheckE2E                 bool
	MinDiskFreeMB            int64
	DisableCredentialHelpers bool
	CredentialHelperTimeout  time.Duration
	ResolveRegistryPolicy    RegistryPolicyResolver
	plainHTTPExplicit        bool
	proxyExplicit            bool
	noProxyExplicit          bool
	registryCAFileExplicit   bool
	registryCAPathExplicit   bool
	timeoutExplicit          bool
	commandflags.FormatOptions
}

type DoctorDefaults struct {
	ConfigPath               string
	LoadedConfig             *appconfig.Loaded
	ConfigLoadError          error
	OutputDir                string
	DisableCredentialHelpers bool
	CredentialHelperTimeout  time.Duration
	ResolveRegistryPolicy    RegistryPolicyResolver
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

type doctorConfig = appconfig.Config
