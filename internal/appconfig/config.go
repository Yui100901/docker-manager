package appconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const DefaultPath = ".dm.yaml"
const EnvName = "DM_CONFIG"

const maxConfigFileSize = 1 << 20

type Config struct {
	Proxy                     string   `yaml:"proxy"`
	TargetOS                  string   `yaml:"os"`
	Arch                      string   `yaml:"arch"`
	OutputDir                 string   `yaml:"output_dir"`
	DockerHost                string   `yaml:"docker_host"`
	DockerTLSVerify           *bool    `yaml:"docker_tls_verify"`
	DockerCertPath            string   `yaml:"docker_cert_path"`
	DockerAPIVersion          string   `yaml:"docker_api_version"`
	DockerTimeout             string   `yaml:"docker_timeout"`
	CAFile                    string   `yaml:"ca_file"`
	CAPath                    string   `yaml:"ca_path"`
	RegistryCAFile            string   `yaml:"registry_ca_file"`
	RegistryCAPath            string   `yaml:"registry_ca_path"`
	ReadyTimeout              string   `yaml:"ready_timeout"`
	RedactProfile             string   `yaml:"redact_profile"`
	RedactSecrets             bool     `yaml:"redact_secrets"`
	CredentialHelpersDisabled bool     `yaml:"credential_helpers_disabled"`
	CredentialHelperTimeout   string   `yaml:"credential_helper_timeout"`
	RegistryAuthRealms        []string `yaml:"registry_auth_realms"`
	Verbose                   bool     `yaml:"verbose"`
	Quiet                     bool     `yaml:"quiet"`
	JSON                      bool     `yaml:"log_json"`

	// Presence markers preserve an explicitly empty Docker setting so it can
	// clear the matching DOCKER_* environment fallback.
	DockerHostSet       bool `yaml:"-"`
	DockerTLSVerifySet  bool `yaml:"-"`
	DockerCertPathSet   bool `yaml:"-"`
	DockerAPIVersionSet bool `yaml:"-"`
}

type LoadOptions struct {
	Required bool
}

type Loaded struct {
	Config Config
	Path   string
	Exists bool
	Fields map[string]bool
}

func Load(path string) (Config, error) {
	loaded, err := LoadWithOptions(path, LoadOptions{})
	return loaded.Config, err
}

func LoadWithOptions(path string, opts LoadOptions) (Loaded, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if opts.Required {
				return Loaded{}, fmt.Errorf("config file %q does not exist: %w", path, os.ErrNotExist)
			}
			return Loaded{Path: path, Fields: map[string]bool{}}, nil
		}
		return Loaded{}, fmt.Errorf("open config file %q: %w", path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxConfigFileSize+1))
	if err != nil {
		return Loaded{}, fmt.Errorf("read config file %q: %w", path, err)
	}
	if len(data) > maxConfigFileSize {
		return Loaded{}, fmt.Errorf("config file %q exceeds %d bytes", path, maxConfigFileSize)
	}

	var cfg Config
	if len(bytes.TrimSpace(data)) > 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&cfg); err != nil {
			return Loaded{}, fmt.Errorf("decode config file %q: %w", path, err)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			if err != nil {
				return Loaded{}, fmt.Errorf("decode config file %q: %w", path, err)
			}
			return Loaded{}, fmt.Errorf("config file %q contains multiple YAML documents", path)
		}
	}
	if err := cfg.Validate(); err != nil {
		return Loaded{}, fmt.Errorf("validate config file %q: %w", path, err)
	}
	fields, err := configFields(data)
	if err != nil {
		return Loaded{}, fmt.Errorf("inspect config file %q: %w", path, err)
	}
	cfg.DockerHostSet = fields["docker_host"]
	cfg.DockerTLSVerifySet = fields["docker_tls_verify"]
	cfg.DockerCertPathSet = fields["docker_cert_path"]
	cfg.DockerAPIVersionSet = fields["docker_api_version"]
	return Loaded{Config: cfg, Path: path, Exists: true, Fields: fields}, nil
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

func IsExplicitPath(flagChanged bool) bool {
	return flagChanged || strings.TrimSpace(os.Getenv(EnvName)) != ""
}

func (cfg Config) Validate() error {
	if cfg.Verbose && cfg.Quiet {
		return errors.New("verbose and quiet cannot both be true")
	}
	profile := strings.ToLower(strings.TrimSpace(cfg.RedactProfile))
	switch profile {
	case "", "none", "basic", "strict":
	default:
		return fmt.Errorf("redact_profile must be none, basic, or strict, got %q", cfg.RedactProfile)
	}
	if cfg.RedactSecrets && profile == "none" {
		return errors.New("redact_secrets=true conflicts with redact_profile=none")
	}
	for _, setting := range []struct {
		name  string
		value string
	}{
		{name: "docker_timeout", value: cfg.DockerTimeout},
		{name: "ready_timeout", value: cfg.ReadyTimeout},
		{name: "credential_helper_timeout", value: cfg.CredentialHelperTimeout},
	} {
		if _, err := PositiveDuration(setting.name, setting.value, 0); err != nil {
			return err
		}
	}
	for _, realm := range cfg.RegistryAuthRealms {
		parsed, err := url.Parse(strings.TrimSpace(realm))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
			strings.Contains(parsed.Hostname(), "*") || (parsed.Path != "" && parsed.Path != "/") ||
			parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("registry_auth_realms entry %q must be an HTTPS origin without credentials, path, query, or fragment", realm)
		}
	}
	return nil
}

func (cfg Config) EffectiveRedactProfile() string {
	profile := strings.ToLower(strings.TrimSpace(cfg.RedactProfile))
	if profile == "" {
		if cfg.RedactSecrets {
			return "basic"
		}
		return "none"
	}
	return profile
}

func PositiveDuration(name, value string, fallback time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 30s or 2m: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return parsed, nil
}

func configFields(data []byte) (map[string]bool, error) {
	fields := make(map[string]bool)
	if len(bytes.TrimSpace(data)) == 0 {
		return fields, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		return fields, nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("config root must be a YAML mapping")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		fields[root.Content[i].Value] = true
	}
	return fields, nil
}
