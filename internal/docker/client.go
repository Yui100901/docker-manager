package docker

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-connections/tlsconfig"
	mobyclient "github.com/moby/moby/client"
)

const DefaultRequestTimeout = 30 * time.Second

var (
	mobyClient        *mobyclient.Client
	mobyClientOptions resolvedClientOptions
	mobyClientInfo    ConnectionInfo
	dockerClientMu    sync.Mutex
	clientOptions     Options
)

// Options describes the explicit Docker daemon settings selected by config or
// flags. Empty string fields inherit the corresponding DOCKER_* environment
// value unless their Set marker records an explicit empty override;
// TLSVerify uses nil to represent inheritance.
type Options struct {
	Host          string
	HostSet       bool
	TLSVerify     *bool
	CertPath      string
	CertPathSet   bool
	APIVersion    string
	APIVersionSet bool
	Timeout       time.Duration
}

// ConnectionInfo describes the transport that was built for a Docker client.
// For initialization errors, it describes the intended transport up to the
// point where validation failed.
type ConnectionInfo struct {
	Host              string
	Transport         string
	TLS               bool
	TLSVerify         bool
	CertPath          string
	CASource          string
	ClientCertificate bool
	APIVersion        string
	Timeout           time.Duration
}

type resolvedClientOptions struct {
	Host         string
	TLSVerify    bool
	TLSVerifySet bool
	CertPath     string
	APIVersion   string
	Timeout      time.Duration
}

// NewMobyClient returns the shared Moby API client for migrated code paths.
func NewMobyClient() (*mobyclient.Client, error) {
	return initMobyClient()
}

// NewMobyClientWithInfo returns the shared client and its actual transport
// configuration. Callers such as doctor use the metadata instead of inferring
// TLS state from configuration intent.
func NewMobyClientWithInfo() (*mobyclient.Client, ConnectionInfo, error) {
	return initMobyClientWithInfo()
}

// Configure sets the Docker API endpoint used by future clients. It is normally
// called once from the root command before any manager requests a client.
func Configure(opts Options) {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()

	opts = cloneOptions(opts)
	if sameOptions(clientOptions, opts) {
		return
	}
	resetMobyClientLocked()
	clientOptions = opts
}

// CurrentOptions returns the explicit options supplied through Configure.
func CurrentOptions() Options {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()
	return cloneOptions(clientOptions)
}

// Endpoint returns the selected Docker endpoint after resolving explicit
// configuration over DOCKER_HOST and the platform local default.
func Endpoint() string {
	opts := EffectiveOptions()
	if strings.TrimSpace(opts.Host) != "" {
		return strings.TrimSpace(opts.Host)
	}
	return defaultLocalEndpoint()
}

// IsRemoteEndpoint reports whether the selected endpoint is not the platform
// local Docker socket or named pipe.
func IsRemoteEndpoint() bool {
	host := strings.ToLower(strings.TrimSpace(Endpoint()))
	return !(strings.HasPrefix(host, "unix://") || strings.HasPrefix(host, "npipe://"))
}

// EffectiveOptions resolves explicit options over Docker's DOCKER_*
// environment variables without mutating process-wide environment state.
func EffectiveOptions() Options {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()
	return resolveClientOptions(clientOptions, os.LookupEnv).toOptions()
}

func defaultLocalEndpoint() string {
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

func cloneOptions(opts Options) Options {
	if opts.TLSVerify != nil {
		value := *opts.TLSVerify
		opts.TLSVerify = &value
	}
	return opts
}

func sameOptions(a, b Options) bool {
	if a.Host != b.Host || a.HostSet != b.HostSet ||
		a.CertPath != b.CertPath || a.CertPathSet != b.CertPathSet ||
		a.APIVersion != b.APIVersion || a.APIVersionSet != b.APIVersionSet ||
		a.Timeout != b.Timeout {
		return false
	}
	switch {
	case a.TLSVerify == nil && b.TLSVerify == nil:
		return true
	case a.TLSVerify != nil && b.TLSVerify != nil:
		return *a.TLSVerify == *b.TLSVerify
	default:
		return false
	}
}

func resolveClientOptions(explicit Options, lookupEnv func(string) (string, bool)) resolvedClientOptions {
	resolved := resolvedClientOptions{Timeout: DefaultRequestTimeout}
	if value, ok := lookupEnv(mobyclient.EnvOverrideHost); ok {
		resolved.Host = strings.TrimSpace(value)
	}
	if value, ok := lookupEnv(mobyclient.EnvOverrideCertPath); ok {
		resolved.CertPath = strings.TrimSpace(value)
	}
	if value, ok := lookupEnv(mobyclient.EnvOverrideAPIVersion); ok {
		resolved.APIVersion = strings.TrimSpace(value)
	}
	if value, ok := lookupEnv(mobyclient.EnvTLSVerify); ok && value != "" {
		resolved.TLSVerify = true
		resolved.TLSVerifySet = true
	}

	if value := strings.TrimSpace(explicit.Host); explicit.HostSet || value != "" {
		resolved.Host = value
	}
	if value := strings.TrimSpace(explicit.CertPath); explicit.CertPathSet || value != "" {
		resolved.CertPath = value
	}
	if value := strings.TrimSpace(explicit.APIVersion); explicit.APIVersionSet || value != "" {
		resolved.APIVersion = value
	}
	if explicit.TLSVerify != nil {
		resolved.TLSVerify = *explicit.TLSVerify
		resolved.TLSVerifySet = true
	}
	if explicit.Timeout > 0 {
		resolved.Timeout = explicit.Timeout
	}
	return resolved
}

func (opts resolvedClientOptions) toOptions() Options {
	result := Options{
		Host:       opts.Host,
		CertPath:   opts.CertPath,
		APIVersion: opts.APIVersion,
		Timeout:    opts.Timeout,
	}
	if opts.TLSVerifySet {
		value := opts.TLSVerify
		result.TLSVerify = &value
	}
	return result
}

func initMobyClient() (*mobyclient.Client, error) {
	cli, _, err := initMobyClientWithInfo()
	return cli, err
}

func initMobyClientWithInfo() (*mobyclient.Client, ConnectionInfo, error) {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()

	resolved := resolveClientOptions(clientOptions, os.LookupEnv)
	if mobyClient != nil && mobyClientOptions != resolved {
		resetMobyClientLocked()
	}
	if mobyClient != nil {
		return mobyClient, mobyClientInfo, nil
	}

	cli, info, err := buildMobyClient(resolved)
	if err != nil {
		return nil, info, err
	}
	mobyClient = cli
	mobyClientOptions = resolved
	mobyClientInfo = info
	return mobyClient, mobyClientInfo, nil
}

func resetMobyClientLocked() {
	if mobyClient != nil {
		_ = mobyClient.Close()
	}
	mobyClient = nil
	mobyClientOptions = resolvedClientOptions{}
	mobyClientInfo = ConnectionInfo{}
}

func buildMobyClient(opts resolvedClientOptions) (*mobyclient.Client, ConnectionInfo, error) {
	host := opts.Host
	if host == "" {
		host = defaultLocalEndpoint()
	}
	info := ConnectionInfo{
		Host:       host,
		TLSVerify:  opts.TLSVerify,
		CertPath:   opts.CertPath,
		APIVersion: opts.APIVersion,
		Timeout:    opts.Timeout,
	}

	hostURL, err := mobyclient.ParseHostURL(host)
	if err != nil {
		return nil, info, fmt.Errorf("configure Docker host %q: %w", host, err)
	}
	endpointTransport := strings.ToLower(hostURL.Scheme)
	info.Transport = endpointTransport
	tlsEnabled := opts.CertPath != "" || (opts.TLSVerifySet && opts.TLSVerify)
	if tlsEnabled {
		info.TLS = true
	}
	switch endpointTransport {
	case "tcp":
		if tlsEnabled {
			info.Transport = "https"
		} else {
			info.Transport = "http"
		}
	case "unix", "npipe":
		if tlsEnabled {
			return nil, info, fmt.Errorf("Docker TLS is only supported for tcp:// endpoints, got %q", host)
		}
	default:
		return nil, info, fmt.Errorf("unsupported Docker host scheme %q in %q; use tcp://, unix://, or npipe://", endpointTransport, host)
	}
	if opts.TLSVerify && opts.CertPath == "" {
		return nil, info, fmt.Errorf("Docker TLS verification requires a certificate directory containing ca.pem, cert.pem, and key.pem")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	transport := &http.Transport{
		MaxIdleConns:          6,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	clientOpts := make([]mobyclient.Opt, 0, 6)
	if tlsEnabled {
		tlsConfig, caSource, err := loadDockerTLSConfig(opts.CertPath, opts.TLSVerify)
		info.CASource = caSource
		if err != nil {
			return nil, info, err
		}
		info.ClientCertificate = true
		transport.TLSClientConfig = tlsConfig
	}
	httpClient := &http.Client{
		Transport:     transport,
		CheckRedirect: mobyclient.CheckRedirect,
	}
	clientOpts = append(clientOpts, mobyclient.WithHTTPClient(httpClient))
	clientOpts = append(clientOpts, mobyclient.WithHost(host))
	if endpointTransport == "tcp" {
		dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
		clientOpts = append(clientOpts, mobyclient.WithDialContext(dialer.DialContext))
	}
	if tlsEnabled {
		clientOpts = append(clientOpts, mobyclient.WithScheme("https"))
	} else {
		clientOpts = append(clientOpts, mobyclient.WithScheme("http"))
	}
	if opts.APIVersion != "" {
		clientOpts = append(clientOpts, mobyclient.WithAPIVersion(opts.APIVersion))
	}

	cli, err := mobyclient.New(clientOpts...)
	if err != nil {
		return nil, info, fmt.Errorf("initialize Docker client: %w", err)
	}
	return cli, info, nil
}

func loadDockerTLSConfig(certPath string, verify bool) (*tls.Config, string, error) {
	certPath = strings.TrimSpace(certPath)
	if certPath == "" {
		return nil, "", fmt.Errorf("Docker TLS certificate directory is empty")
	}
	caFile := filepath.Join(certPath, "ca.pem")
	caSource := "verification-disabled"
	if verify {
		caSource = "system+" + caFile
	}
	pathInfo, err := os.Stat(certPath)
	if err != nil {
		return nil, caSource, fmt.Errorf("inspect Docker TLS certificate directory %q: %w", certPath, err)
	}
	if !pathInfo.IsDir() {
		return nil, caSource, fmt.Errorf("Docker TLS certificate path %q is not a directory", certPath)
	}

	certFile := filepath.Join(certPath, "cert.pem")
	keyFile := filepath.Join(certPath, "key.pem")
	for _, path := range []string{caFile, certFile, keyFile} {
		fileInfo, err := os.Stat(path)
		if err != nil {
			return nil, caSource, fmt.Errorf("inspect Docker TLS file %q: %w", path, err)
		}
		if !fileInfo.Mode().IsRegular() {
			return nil, caSource, fmt.Errorf("Docker TLS file %q is not a regular file", path)
		}
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, caSource, fmt.Errorf("read Docker TLS CA file %q: %w", caFile, err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(caPEM) {
		return nil, caSource, fmt.Errorf("Docker TLS CA file %q does not contain a valid PEM certificate", caFile)
	}

	tlsConfig, err := tlsconfig.Client(tlsconfig.Options{
		CAFile:             caFile,
		CertFile:           certFile,
		KeyFile:            keyFile,
		InsecureSkipVerify: !verify, // #nosec G402 -- explicit Docker-compatible opt-out
		ExclusiveRootPools: false,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		return nil, caSource, fmt.Errorf("configure Docker TLS from %q: %w", certPath, err)
	}
	return tlsConfig, caSource, nil
}
