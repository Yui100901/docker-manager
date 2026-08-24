package dockerconfig

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/docker"

	"github.com/spf13/cobra"
)

type FlagValues struct {
	Host              string
	HostChanged       bool
	TLSVerify         bool
	TLSVerifyChanged  bool
	CertPath          string
	CertPathChanged   bool
	APIVersion        string
	APIVersionChanged bool
	Timeout           time.Duration
	TimeoutChanged    bool
}

func OptionsFromConfig(cfg appconfig.Config, flags FlagValues) docker.Options {
	timeout := docker.DefaultRequestTimeout
	if value := strings.TrimSpace(cfg.DockerTimeout); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	opts := docker.Options{
		Host:          cfg.DockerHost,
		HostSet:       cfg.DockerHostSet,
		TLSVerify:     cfg.DockerTLSVerify,
		CertPath:      cfg.DockerCertPath,
		CertPathSet:   cfg.DockerCertPathSet,
		APIVersion:    cfg.DockerAPIVersion,
		APIVersionSet: cfg.DockerAPIVersionSet,
		Timeout:       timeout,
	}
	if cfg.DockerTLSVerifySet && opts.TLSVerify == nil {
		value := false
		opts.TLSVerify = &value
	}
	if flags.HostChanged {
		opts.Host = flags.Host
		opts.HostSet = true
	}
	if flags.TLSVerifyChanged {
		value := flags.TLSVerify
		opts.TLSVerify = &value
	}
	if flags.CertPathChanged {
		opts.CertPath = flags.CertPath
		opts.CertPathSet = true
	}
	if flags.APIVersionChanged {
		opts.APIVersion = flags.APIVersion
		opts.APIVersionSet = true
	}
	if flags.TimeoutChanged {
		opts.Timeout = flags.Timeout
	}
	return opts
}

func OptionsFromRootFlags(cfg appconfig.Config, cmd *cobra.Command) (docker.Options, error) {
	if cmd == nil {
		return OptionsFromConfig(cfg, FlagValues{}), nil
	}
	root := cmd.Root()
	if root == nil {
		root = cmd
	}
	flags := root.PersistentFlags()
	values := FlagValues{}
	if flag := flags.Lookup("docker-host"); flag != nil {
		values.Host = flag.Value.String()
		values.HostChanged = flag.Changed
	}
	if flag := flags.Lookup("docker-tls-verify"); flag != nil {
		values.TLSVerifyChanged = flag.Changed
		if flag.Changed {
			value, err := strconv.ParseBool(flag.Value.String())
			if err != nil {
				return docker.Options{}, err
			}
			values.TLSVerify = value
		}
	}
	if flag := flags.Lookup("docker-cert-path"); flag != nil {
		values.CertPath = flag.Value.String()
		values.CertPathChanged = flag.Changed
	}
	if flag := flags.Lookup("docker-api-version"); flag != nil {
		values.APIVersion = flag.Value.String()
		values.APIVersionChanged = flag.Changed
	}
	if flag := flags.Lookup("docker-timeout"); flag != nil {
		values.TimeoutChanged = flag.Changed
		if flag.Changed {
			value, err := time.ParseDuration(flag.Value.String())
			if err != nil {
				return docker.Options{}, err
			}
			if value <= 0 {
				return docker.Options{}, fmt.Errorf("--docker-timeout must be greater than zero")
			}
			values.Timeout = value
		}
	}
	return OptionsFromConfig(cfg, values), nil
}
