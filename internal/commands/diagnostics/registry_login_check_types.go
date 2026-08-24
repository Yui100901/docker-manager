package diagnostics

import (
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/registryauth"
)

type RegistryLoginCheckOptions struct {
	DockerConfig             string
	PlainHTTP                bool
	Timeout                  time.Duration
	FailOnError              bool
	FailOnWarning            bool
	DisableCredentialHelpers bool
	CredentialHelperTimeout  time.Duration
	commandflags.FormatOptions
}

type RegistryLoginCheckDefaults struct {
	DisableCredentialHelpers bool
	CredentialHelperTimeout  time.Duration
}

type RegistryLoginCheckReport struct {
	Registry        string           `json:"registry"`
	DockerConfig    string           `json:"docker_config"`
	ConfigFound     bool             `json:"config_found"`
	Credential      CredentialReport `json:"credential"`
	RegistryPing    CheckResult      `json:"registry_ping"`
	DockerLogin     CheckResult      `json:"docker_login"`
	Recommendations []string         `json:"recommendations,omitempty"`
}

type CredentialReport struct {
	Found        bool   `json:"found"`
	Source       string `json:"source,omitempty"`
	Helper       string `json:"helper,omitempty"`
	HelperSource string `json:"helper_source,omitempty"`
	HelperPath   string `json:"helper_path,omitempty"`
	Username     string `json:"username,omitempty"`
	Message      string `json:"message,omitempty"`
}

type CheckResult struct {
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type dockerConfigFile = registryauth.Config
type dockerAuthEntry = registryauth.AuthEntry
type registryCredential = registryauth.Credential
