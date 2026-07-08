package diagnostics

import "docker-manager/internal/commandflags"

type HealthOptions struct {
	RunningOnly      bool
	NoLogs           bool
	LogTail          int
	RestartThreshold int
	Keywords         []string
	ContainerFilters []string
	RedactSecrets    bool
	RedactProfile    string
	commandflags.FormatOptions
}

type HealthReport struct {
	GeneratedAt    string            `json:"generated_at"`
	DockerEndpoint string            `json:"docker_endpoint"`
	Target         TargetSelection   `json:"target"`
	Summary        HealthSummary     `json:"summary"`
	Containers     []HealthContainer `json:"containers"`
	Issues         []HealthIssue     `json:"issues,omitempty"`
}

type HealthSummary struct {
	Total           int `json:"total"`
	Running         int `json:"running"`
	Stopped         int `json:"stopped"`
	Restarting      int `json:"restarting"`
	Unhealthy       int `json:"unhealthy"`
	RestartWarnings int `json:"restart_warnings"`
	LogWarnings     int `json:"log_warnings"`
	LogsUnavailable int `json:"logs_unavailable"`
	PublicBindings  int `json:"public_bindings"`
}

type HealthContainer struct {
	ID                    string             `json:"id"`
	Name                  string             `json:"name"`
	Image                 string             `json:"image,omitempty"`
	ImageID               string             `json:"image_id,omitempty"`
	ImageDigest           string             `json:"image_digest,omitempty"`
	State                 string             `json:"state,omitempty"`
	Status                string             `json:"status,omitempty"`
	RestartCount          int                `json:"restart_count"`
	RestartPolicy         string             `json:"restart_policy,omitempty"`
	HealthStatus          string             `json:"health_status,omitempty"`
	FailingStreak         int                `json:"failing_streak,omitempty"`
	ExitCode              int                `json:"exit_code,omitempty"`
	Error                 string             `json:"error,omitempty"`
	PublicPorts           []string           `json:"public_ports,omitempty"`
	ExposedPorts          []string           `json:"exposed_ports,omitempty"`
	Ports                 []HealthPortRef    `json:"ports,omitempty"`
	Networks              []HealthNetworkRef `json:"networks,omitempty"`
	Mounts                []HealthMountRef   `json:"mounts,omitempty"`
	LogDriver             string             `json:"log_driver,omitempty"`
	LogOptions            map[string]string  `json:"log_options,omitempty"`
	LogReadability        string             `json:"log_readability,omitempty"`
	LogReadabilityMessage string             `json:"log_readability_message,omitempty"`
	NetworkMode           string             `json:"network_mode,omitempty"`
	LogMatches            []LogMatch         `json:"log_matches,omitempty"`
}

type HealthPortRef struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      uint16 `json:"host_port,omitempty"`
	ContainerPort uint16 `json:"container_port"`
	Protocol      string `json:"protocol"`
	Published     bool   `json:"published"`
	Source        string `json:"source,omitempty"`
}

type HealthNetworkRef struct {
	Name        string   `json:"name"`
	NetworkID   string   `json:"network_id,omitempty"`
	EndpointID  string   `json:"endpoint_id,omitempty"`
	IPAddress   string   `json:"ip_address,omitempty"`
	IPv6Address string   `json:"ipv6_address,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

type HealthMountRef struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	Mode        string `json:"mode,omitempty"`
	RW          bool   `json:"rw"`
}

type LogMatch struct {
	Line     string   `json:"line"`
	Keywords []string `json:"keywords"`
}

type HealthIssue struct {
	Severity  string `json:"severity"`
	Container string `json:"container,omitempty"`
	Type      string `json:"type"`
	Message   string `json:"message"`
}
