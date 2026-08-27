package appconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const DefaultPath = ".dm.yaml"
const EnvName = "DM_CONFIG"
const ProfileEnvName = "DM_PROFILE"

const maxConfigFileSize = 1 << 20

var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

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

	DefaultProfile string                    `yaml:"default_profile" json:"default_profile,omitempty"`
	Profiles       map[string]Config         `yaml:"profiles" json:"profiles,omitempty"`
	Registries     map[string]RegistryPolicy `yaml:"registries" json:"registries,omitempty"`

	// Presence markers preserve an explicitly empty Docker setting so it can
	// clear the matching DOCKER_* environment fallback.
	DockerHostSet       bool `yaml:"-" json:"-"`
	DockerTLSVerifySet  bool `yaml:"-" json:"-"`
	DockerCertPathSet   bool `yaml:"-" json:"-"`
	DockerAPIVersionSet bool `yaml:"-" json:"-"`
}

type RegistryPolicy struct {
	CAFile          string   `yaml:"ca_file" json:"ca_file,omitempty"`
	CAPath          string   `yaml:"ca_path" json:"ca_path,omitempty"`
	Proxy           string   `yaml:"proxy" json:"proxy,omitempty"`
	NoProxy         bool     `yaml:"no_proxy" json:"no_proxy"`
	Timeout         string   `yaml:"timeout" json:"timeout,omitempty"`
	CredentialScope []string `yaml:"credential_scope" json:"credential_scope"`
	AuthRealms      []string `yaml:"auth_realms" json:"auth_realms,omitempty"`
	PlainHTTP       bool     `yaml:"plain_http" json:"plain_http"`

	CAFileSet          bool `yaml:"-" json:"-"`
	CAPathSet          bool `yaml:"-" json:"-"`
	ProxySet           bool `yaml:"-" json:"-"`
	NoProxySet         bool `yaml:"-" json:"-"`
	TimeoutSet         bool `yaml:"-" json:"-"`
	CredentialScopeSet bool `yaml:"-" json:"-"`
	AuthRealmsSet      bool `yaml:"-" json:"-"`
	PlainHTTPSet       bool `yaml:"-" json:"-"`
}

type LoadOptions struct {
	Required        bool
	Profile         string
	ProfileExplicit bool
}

type Loaded struct {
	Config            Config
	Path              string
	Exists            bool
	Fields            map[string]bool
	Profile           string
	ProfileSource     string
	AvailableProfiles []string
	FieldSources      map[string]string
}

type configPresence struct {
	Fields     map[string]bool
	Registries map[string]map[string]bool
}

type documentPresence struct {
	Base     configPresence
	Profiles map[string]configPresence
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
			profile, source := ResolveProfile(opts.Profile, opts.ProfileExplicit, "")
			if profile != "" {
				return Loaded{}, fmt.Errorf("profile %q is not defined because config file %q does not exist", profile, path)
			}
			return Loaded{
				Path:          path,
				Fields:        map[string]bool{},
				Profile:       profile,
				ProfileSource: source,
				FieldSources:  map[string]string{},
			}, nil
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
	presence, err := inspectConfigDocument(data)
	if err != nil {
		return Loaded{}, fmt.Errorf("inspect config file %q: %w", path, err)
	}
	if err := prepareLoadedDocument(&cfg, &presence); err != nil {
		return Loaded{}, fmt.Errorf("validate config file %q: %w", path, err)
	}
	if err := validateLoadedDocument(cfg, presence); err != nil {
		return Loaded{}, fmt.Errorf("validate config file %q: %w", path, err)
	}

	profile, profileSource := ResolveProfile(opts.Profile, opts.ProfileExplicit, cfg.DefaultProfile)
	if profile != "" {
		if err := validateProfileName(profile); err != nil {
			return Loaded{}, fmt.Errorf("select profile: %w", err)
		}
		if _, exists := cfg.Profiles[profile]; !exists {
			return Loaded{}, fmt.Errorf("profile %q is not defined in config file %q", profile, path)
		}
	}
	effective, fields, fieldSources := resolveEffectiveConfig(cfg, presence, profile, path)
	return Loaded{
		Config:            effective,
		Path:              path,
		Exists:            true,
		Fields:            fields,
		Profile:           profile,
		ProfileSource:     profileSource,
		AvailableProfiles: sortedProfileNames(cfg.Profiles),
		FieldSources:      fieldSources,
	}, nil
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

func ResolveProfile(profile string, explicit bool, defaultProfile string) (string, string) {
	if explicit {
		return strings.TrimSpace(profile), "flag:--profile"
	}
	if envProfile := strings.TrimSpace(os.Getenv(ProfileEnvName)); envProfile != "" {
		return envProfile, "env:" + ProfileEnvName
	}
	if configured := strings.TrimSpace(defaultProfile); configured != "" {
		return configured, "config:default_profile"
	}
	return "", "default"
}

func (policy RegistryPolicy) Validate() error {
	if policy.PlainHTTP && (strings.TrimSpace(policy.CAFile) != "" || strings.TrimSpace(policy.CAPath) != "") {
		return errors.New("plain_http=true cannot be combined with ca_file or ca_path")
	}
	if _, err := PositiveDuration("timeout", policy.Timeout, 0); err != nil {
		return err
	}
	if value := strings.TrimSpace(policy.Proxy); value != "" {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Fragment != "" {
			return fmt.Errorf("proxy %q must contain a scheme and host and must not contain a fragment", policy.Proxy)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "socks5":
		default:
			return fmt.Errorf("proxy %q uses unsupported scheme %q; expected http, https, or socks5", policy.Proxy, parsed.Scheme)
		}
	}
	seen := make(map[string]struct{}, len(policy.CredentialScope))
	for _, value := range policy.CredentialScope {
		operation := strings.ToLower(strings.TrimSpace(value))
		switch operation {
		case "pull", "push", "login":
		default:
			return fmt.Errorf("credential_scope entry %q must be pull, push, or login", value)
		}
		if _, duplicate := seen[operation]; duplicate {
			return fmt.Errorf("credential_scope contains duplicate operation %q", operation)
		}
		seen[operation] = struct{}{}
	}
	return validateHTTPSOrigins("auth_realms", policy.AuthRealms)
}

func (policy RegistryPolicy) AllowsCredential(operation string) bool {
	operation = strings.ToLower(strings.TrimSpace(operation))
	switch operation {
	case "pull", "push", "login":
	default:
		return false
	}
	if policy.CredentialScope == nil {
		return true
	}
	for _, allowed := range policy.CredentialScope {
		if strings.EqualFold(strings.TrimSpace(allowed), operation) {
			return true
		}
	}
	return false
}

func NormalizeRegistryKey(value string) (string, error) {
	return normalizeRegistry(value, true)
}

// NormalizeRegistryEndpoint validates a registry network endpoint without
// rewriting Docker Hub aliases used by policy-key matching.
func NormalizeRegistryEndpoint(value string) (string, error) {
	return normalizeRegistry(value, false)
}

func normalizeRegistry(value string, canonicalizeDockerHub bool) (string, error) {
	original := value
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("registry key cannot be empty")
	}
	if strings.Contains(value, "://") {
		return "", fmt.Errorf("registry key %q must be an exact host[:port] without a scheme", original)
	}
	parsed, err := url.Parse("//" + value)
	if err != nil {
		return "", fmt.Errorf("invalid registry key %q: %w", original, err)
	}
	if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("registry key %q must be an exact host[:port] without credentials, path, query, or fragment", original)
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || strings.ContainsAny(host, "*%\t\r\n ") {
		return "", fmt.Errorf("registry key %q contains an invalid host", original)
	}
	if ip := net.ParseIP(host); ip != nil {
		host = strings.ToLower(ip.String())
	} else if !validRegistryHostname(host) {
		return "", fmt.Errorf("registry key %q contains an invalid host", original)
	}
	port := parsed.Port()
	if strings.HasSuffix(parsed.Host, ":") && port == "" {
		return "", fmt.Errorf("registry key %q contains an empty port", original)
	}
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("registry key %q contains an invalid port", original)
		}
		port = strconv.Itoa(portNumber)
	}
	if canonicalizeDockerHub {
		switch host {
		case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
			host = "docker.io"
		}
	}
	if port != "" {
		return net.JoinHostPort(host, port), nil
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]", nil
	}
	return host, nil
}

func ResolveRegistryPolicy(policies map[string]RegistryPolicy, registry string) (RegistryPolicy, bool) {
	key, err := NormalizeRegistryKey(registry)
	if err != nil {
		return RegistryPolicy{}, false
	}
	if policy, exists := policies[key]; exists {
		return policy, true
	}
	keys := make([]string, 0, len(policies))
	for candidate := range policies {
		keys = append(keys, candidate)
	}
	sort.Strings(keys)
	for _, candidate := range keys {
		normalized, normalizeErr := NormalizeRegistryKey(candidate)
		if normalizeErr == nil && normalized == key {
			return policies[candidate], true
		}
	}
	return RegistryPolicy{}, false
}

func (cfg Config) Validate() error {
	if err := validateConfigValues(cfg); err != nil {
		return err
	}
	if err := validateRegistryPolicies(cfg.Registries); err != nil {
		return err
	}
	if configured := strings.TrimSpace(cfg.DefaultProfile); configured != "" {
		if err := validateProfileName(configured); err != nil {
			return fmt.Errorf("default_profile: %w", err)
		}
		if _, exists := cfg.Profiles[configured]; !exists {
			return fmt.Errorf("default_profile %q is not defined in profiles", configured)
		}
	}
	for _, name := range sortedProfileNames(cfg.Profiles) {
		if err := validateProfileName(name); err != nil {
			return err
		}
		profile := cfg.Profiles[name]
		if profile.DefaultProfile != "" || len(profile.Profiles) != 0 {
			return fmt.Errorf("profile %q cannot contain default_profile or nested profiles", name)
		}
		if err := validateConfigValues(profile); err != nil {
			return fmt.Errorf("profile %q: %w", name, err)
		}
		if err := validateRegistryPolicies(profile.Registries); err != nil {
			return fmt.Errorf("profile %q: %w", name, err)
		}
	}
	return nil
}

func validateConfigValues(cfg Config) error {
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
	return validateHTTPSOrigins("registry_auth_realms", cfg.RegistryAuthRealms)
}

func validateProfileName(name string) error {
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("profile name %q must match %s", name, profileNamePattern.String())
	}
	return nil
}

func validateRegistryPolicies(policies map[string]RegistryPolicy) error {
	seen := make(map[string]string, len(policies))
	for _, key := range sortedRegistryKeys(policies) {
		normalized, err := NormalizeRegistryKey(key)
		if err != nil {
			return err
		}
		if previous, duplicate := seen[normalized]; duplicate {
			return fmt.Errorf("registry keys %q and %q normalize to the same key %q", previous, key, normalized)
		}
		seen[normalized] = key
		if err := policies[key].Validate(); err != nil {
			return fmt.Errorf("registry %q: %w", key, err)
		}
	}
	return nil
}

func sortedProfileNames(profiles map[string]Config) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedRegistryKeys(policies map[string]RegistryPolicy) []string {
	keys := make([]string, 0, len(policies))
	for key := range policies {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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

func prepareLoadedDocument(cfg *Config, presence *documentPresence) error {
	baseRegistries, basePresence, err := prepareRegistryPolicies(cfg.Registries, presence.Base.Registries)
	if err != nil {
		return err
	}
	cfg.Registries = baseRegistries
	presence.Base.Registries = basePresence
	setConfigPresence(cfg, presence.Base.Fields)

	for _, name := range sortedProfileNames(cfg.Profiles) {
		profile := cfg.Profiles[name]
		profilePresence := presence.Profiles[name]
		registries, registriesPresence, err := prepareRegistryPolicies(profile.Registries, profilePresence.Registries)
		if err != nil {
			return fmt.Errorf("profile %q: %w", name, err)
		}
		profile.Registries = registries
		profilePresence.Registries = registriesPresence
		setConfigPresence(&profile, profilePresence.Fields)
		cfg.Profiles[name] = profile
		presence.Profiles[name] = profilePresence
	}
	return nil
}

func prepareRegistryPolicies(policies map[string]RegistryPolicy, fields map[string]map[string]bool) (map[string]RegistryPolicy, map[string]map[string]bool, error) {
	if len(policies) == 0 {
		return policies, map[string]map[string]bool{}, nil
	}
	normalizedPolicies := make(map[string]RegistryPolicy, len(policies))
	normalizedFields := make(map[string]map[string]bool, len(policies))
	originalKeys := make(map[string]string, len(policies))
	for _, key := range sortedRegistryKeys(policies) {
		normalized, err := NormalizeRegistryKey(key)
		if err != nil {
			return nil, nil, err
		}
		if previous, duplicate := originalKeys[normalized]; duplicate {
			return nil, nil, fmt.Errorf("registry keys %q and %q normalize to the same key %q", previous, key, normalized)
		}
		originalKeys[normalized] = key
		policyFields := copyBoolMap(fields[key])
		policy := policies[key]
		setRegistryPolicyPresence(&policy, policyFields)
		if policy.CredentialScopeSet {
			if policy.CredentialScope == nil {
				policy.CredentialScope = []string{}
			}
			for i := range policy.CredentialScope {
				policy.CredentialScope[i] = strings.ToLower(strings.TrimSpace(policy.CredentialScope[i]))
			}
		}
		if err := policy.Validate(); err != nil {
			return nil, nil, fmt.Errorf("registry %q: %w", key, err)
		}
		normalizedPolicies[normalized] = policy
		normalizedFields[normalized] = policyFields
	}
	return normalizedPolicies, normalizedFields, nil
}

func validateLoadedDocument(cfg Config, presence documentPresence) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	for _, name := range sortedProfileNames(cfg.Profiles) {
		effective := mergeConfig(cfg, cfg.Profiles[name], presence.Profiles[name])
		if err := validateConfigValues(effective); err != nil {
			return fmt.Errorf("profile %q effective config: %w", name, err)
		}
		if err := validateRegistryPolicies(effective.Registries); err != nil {
			return fmt.Errorf("profile %q effective config: %w", name, err)
		}
	}
	return nil
}

func resolveEffectiveConfig(cfg Config, presence documentPresence, profile, path string) (Config, map[string]bool, map[string]string) {
	fields := copyBoolMap(presence.Base.Fields)
	fieldSources := make(map[string]string)
	for field := range fields {
		fieldSources[field] = "config:" + path
	}
	setRegistryFieldSources(fieldSources, presence.Base.Registries, "config:"+path)
	if profile == "" {
		return cfg, fields, fieldSources
	}

	profilePresence := presence.Profiles[profile]
	for field := range profilePresence.Fields {
		fields[field] = true
		fieldSources[field] = "profile:" + profile + "@" + path
	}
	setRegistryFieldSources(fieldSources, profilePresence.Registries, "profile:"+profile+"@"+path)
	return mergeConfig(cfg, cfg.Profiles[profile], profilePresence), fields, fieldSources
}

func mergeConfig(base, overlay Config, presence configPresence) Config {
	result := base
	fields := presence.Fields
	if fields["proxy"] {
		result.Proxy = overlay.Proxy
	}
	if fields["os"] {
		result.TargetOS = overlay.TargetOS
	}
	if fields["arch"] {
		result.Arch = overlay.Arch
	}
	if fields["output_dir"] {
		result.OutputDir = overlay.OutputDir
	}
	if fields["docker_host"] {
		result.DockerHost = overlay.DockerHost
		result.DockerHostSet = true
	}
	if fields["docker_tls_verify"] {
		result.DockerTLSVerify = overlay.DockerTLSVerify
		result.DockerTLSVerifySet = true
	}
	if fields["docker_cert_path"] {
		result.DockerCertPath = overlay.DockerCertPath
		result.DockerCertPathSet = true
	}
	if fields["docker_api_version"] {
		result.DockerAPIVersion = overlay.DockerAPIVersion
		result.DockerAPIVersionSet = true
	}
	if fields["docker_timeout"] {
		result.DockerTimeout = overlay.DockerTimeout
	}
	if fields["ca_file"] {
		result.CAFile = overlay.CAFile
	}
	if fields["ca_path"] {
		result.CAPath = overlay.CAPath
	}
	if fields["registry_ca_file"] {
		result.RegistryCAFile = overlay.RegistryCAFile
	}
	if fields["registry_ca_path"] {
		result.RegistryCAPath = overlay.RegistryCAPath
	}
	if fields["ready_timeout"] {
		result.ReadyTimeout = overlay.ReadyTimeout
	}
	if fields["redact_profile"] {
		result.RedactProfile = overlay.RedactProfile
	}
	if fields["redact_secrets"] {
		result.RedactSecrets = overlay.RedactSecrets
	}
	if fields["credential_helpers_disabled"] {
		result.CredentialHelpersDisabled = overlay.CredentialHelpersDisabled
	}
	if fields["credential_helper_timeout"] {
		result.CredentialHelperTimeout = overlay.CredentialHelperTimeout
	}
	if fields["registry_auth_realms"] {
		result.RegistryAuthRealms = cloneStrings(overlay.RegistryAuthRealms)
	}
	if fields["verbose"] {
		result.Verbose = overlay.Verbose
	}
	if fields["quiet"] {
		result.Quiet = overlay.Quiet
	}
	if fields["log_json"] {
		result.JSON = overlay.JSON
	}
	result.Registries = mergeRegistryPolicies(base.Registries, overlay.Registries, presence.Registries)
	return result
}

func mergeRegistryPolicies(base, overlay map[string]RegistryPolicy, fields map[string]map[string]bool) map[string]RegistryPolicy {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	result := make(map[string]RegistryPolicy, len(base)+len(overlay))
	for key, policy := range base {
		result[key] = cloneRegistryPolicy(policy)
	}
	for key, policy := range overlay {
		result[key] = mergeRegistryPolicy(result[key], policy, fields[key])
	}
	return result
}

func mergeRegistryPolicy(base, overlay RegistryPolicy, fields map[string]bool) RegistryPolicy {
	result := base
	if fields["ca_file"] {
		result.CAFile = overlay.CAFile
		result.CAFileSet = true
	}
	if fields["ca_path"] {
		result.CAPath = overlay.CAPath
		result.CAPathSet = true
	}
	if fields["proxy"] {
		result.Proxy = overlay.Proxy
		result.ProxySet = true
	}
	if fields["no_proxy"] {
		result.NoProxy = overlay.NoProxy
		result.NoProxySet = true
	}
	if fields["timeout"] {
		result.Timeout = overlay.Timeout
		result.TimeoutSet = true
	}
	if fields["credential_scope"] {
		result.CredentialScope = cloneStrings(overlay.CredentialScope)
		result.CredentialScopeSet = true
	}
	if fields["auth_realms"] {
		result.AuthRealms = cloneStrings(overlay.AuthRealms)
		result.AuthRealmsSet = true
	}
	if fields["plain_http"] {
		result.PlainHTTP = overlay.PlainHTTP
		result.PlainHTTPSet = true
	}
	return result
}

func cloneRegistryPolicy(policy RegistryPolicy) RegistryPolicy {
	policy.CredentialScope = cloneStrings(policy.CredentialScope)
	policy.AuthRealms = cloneStrings(policy.AuthRealms)
	return policy
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	result := append([]string(nil), values...)
	if result == nil {
		return []string{}
	}
	return result
}

func setConfigPresence(cfg *Config, fields map[string]bool) {
	cfg.DockerHostSet = fields["docker_host"]
	cfg.DockerTLSVerifySet = fields["docker_tls_verify"]
	cfg.DockerCertPathSet = fields["docker_cert_path"]
	cfg.DockerAPIVersionSet = fields["docker_api_version"]
}

func setRegistryPolicyPresence(policy *RegistryPolicy, fields map[string]bool) {
	policy.CAFileSet = fields["ca_file"]
	policy.CAPathSet = fields["ca_path"]
	policy.ProxySet = fields["proxy"]
	policy.NoProxySet = fields["no_proxy"]
	policy.TimeoutSet = fields["timeout"]
	policy.CredentialScopeSet = fields["credential_scope"]
	policy.AuthRealmsSet = fields["auth_realms"]
	policy.PlainHTTPSet = fields["plain_http"]
}

func setRegistryFieldSources(target map[string]string, registries map[string]map[string]bool, source string) {
	for registry, fields := range registries {
		target["registries."+registry] = source
		for field := range fields {
			target["registries."+registry+"."+field] = source
		}
	}
}

func copyBoolMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func configFields(data []byte) (map[string]bool, error) {
	presence, err := inspectConfigDocument(data)
	if err != nil {
		return nil, err
	}
	return presence.Base.Fields, nil
}

func inspectConfigDocument(data []byte) (documentPresence, error) {
	result := documentPresence{
		Base: configPresence{
			Fields:     map[string]bool{},
			Registries: map[string]map[string]bool{},
		},
		Profiles: map[string]configPresence{},
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return result, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return documentPresence{}, err
	}
	if len(document.Content) == 0 {
		return result, nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return documentPresence{}, errors.New("config root must be a YAML mapping")
	}
	result.Base.Fields = mappingFields(root)
	if result.Base.Fields["<<"] {
		return documentPresence{}, errors.New("YAML merge keys are not supported")
	}
	if node, exists := mappingValue(root, "default_profile"); exists && (node.Kind != yaml.ScalarNode || node.Tag != "!!str") {
		return documentPresence{}, errors.New("default_profile must be a string")
	}
	if node, exists := mappingValue(root, "registries"); exists {
		registries, err := inspectRegistries(node)
		if err != nil {
			return documentPresence{}, fmt.Errorf("registries: %w", err)
		}
		result.Base.Registries = registries
	}
	if node, exists := mappingValue(root, "profiles"); exists {
		if node.Kind != yaml.MappingNode {
			return documentPresence{}, errors.New("profiles must be a YAML mapping")
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			name := node.Content[i].Value
			profileNode := node.Content[i+1]
			if profileNode.Kind != yaml.MappingNode {
				return documentPresence{}, fmt.Errorf("profile %q must be a YAML mapping", name)
			}
			profileFields := mappingFields(profileNode)
			if profileFields["<<"] {
				return documentPresence{}, fmt.Errorf("profile %q: YAML merge keys are not supported", name)
			}
			if profileFields["default_profile"] || profileFields["profiles"] {
				return documentPresence{}, fmt.Errorf("profile %q cannot contain default_profile or nested profiles", name)
			}
			profilePresence := configPresence{Fields: profileFields, Registries: map[string]map[string]bool{}}
			if registriesNode, exists := mappingValue(profileNode, "registries"); exists {
				registries, err := inspectRegistries(registriesNode)
				if err != nil {
					return documentPresence{}, fmt.Errorf("profile %q registries: %w", name, err)
				}
				profilePresence.Registries = registries
			}
			result.Profiles[name] = profilePresence
		}
	}
	return result, nil
}

func inspectRegistries(node *yaml.Node) (map[string]map[string]bool, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("must be a YAML mapping")
	}
	result := make(map[string]map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		policyNode := node.Content[i+1]
		if policyNode.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("registry %q policy must be a YAML mapping", key)
		}
		fields := mappingFields(policyNode)
		if fields["<<"] {
			return nil, fmt.Errorf("registry %q: YAML merge keys are not supported", key)
		}
		if scope, exists := mappingValue(policyNode, "credential_scope"); exists && scope.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("registry %q credential_scope must be a YAML sequence", key)
		}
		if realms, exists := mappingValue(policyNode, "auth_realms"); exists && realms.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("registry %q auth_realms must be a YAML sequence", key)
		}
		result[key] = fields
	}
	return result, nil
}

func mappingFields(node *yaml.Node) map[string]bool {
	fields := make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		fields[node.Content[i].Value] = true
	}
	return fields
}

func mappingValue(node *yaml.Node, name string) (*yaml.Node, bool) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == name {
			return node.Content[i+1], true
		}
	}
	return nil, false
}

func validateHTTPSOrigins(name string, origins []string) error {
	for _, origin := range origins {
		parsed, err := url.Parse(strings.TrimSpace(origin))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
			strings.Contains(parsed.Hostname(), "*") || (parsed.Path != "" && parsed.Path != "/") ||
			parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("%s entry %q must be an HTTPS origin without credentials, path, query, or fragment", name, origin)
		}
	}
	return nil
}

func validRegistryHostname(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || !isASCIIAlphaNumeric(label[0]) || !isASCIIAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for i := 1; i+1 < len(label); i++ {
			if !isASCIIAlphaNumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}
