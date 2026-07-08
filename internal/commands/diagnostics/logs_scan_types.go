package diagnostics

import "docker-manager/internal/commandflags"

type LogsScanOptions struct {
	RunningOnly   bool
	Tail          int
	Context       int
	Since         string
	Keywords      []string
	Filters       []string
	RedactSecrets bool
	RedactProfile string
	commandflags.FormatOptions
}

type LogsScanReport struct {
	GeneratedAt    string              `json:"generated_at"`
	DockerEndpoint string              `json:"docker_endpoint"`
	Target         TargetSelection     `json:"target"`
	Keywords       []string            `json:"keywords"`
	Containers     []LogsScanContainer `json:"containers"`
	Summary        LogsScanSummary     `json:"summary"`
}

type LogsScanSummary struct {
	ScannedContainers int `json:"scanned_containers"`
	ContainersMatched int `json:"containers_matched"`
	TotalMatches      int `json:"total_matches"`
	Errors            int `json:"errors"`
	LogsUnavailable   int `json:"logs_unavailable"`
}

type LogsScanContainer struct {
	ID                    string         `json:"id"`
	Name                  string         `json:"name"`
	Image                 string         `json:"image,omitempty"`
	State                 string         `json:"state,omitempty"`
	LogDriver             string         `json:"log_driver,omitempty"`
	LogReadability        string         `json:"log_readability,omitempty"`
	LogReadabilityMessage string         `json:"log_readability_message,omitempty"`
	Error                 string         `json:"error,omitempty"`
	Matches               []LogScanMatch `json:"matches,omitempty"`
}

type LogScanMatch struct {
	LineNumber int      `json:"line_number"`
	Line       string   `json:"line"`
	Keywords   []string `json:"keywords"`
	Before     []string `json:"before,omitempty"`
	After      []string `json:"after,omitempty"`
}
