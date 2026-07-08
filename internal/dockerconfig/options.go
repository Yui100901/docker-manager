package dockerconfig

import (
	"strconv"

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
}

func OptionsFromConfig(cfg appconfig.Config, flags FlagValues) docker.Options {
	opts := docker.Options{
		Host:       cfg.DockerHost,
		TLSVerify:  cfg.DockerTLSVerify,
		CertPath:   cfg.DockerCertPath,
		APIVersion: cfg.DockerAPIVersion,
	}
	if flags.HostChanged {
		opts.Host = flags.Host
	}
	if flags.TLSVerifyChanged {
		value := flags.TLSVerify
		opts.TLSVerify = &value
	}
	if flags.CertPathChanged {
		opts.CertPath = flags.CertPath
	}
	if flags.APIVersionChanged {
		opts.APIVersion = flags.APIVersion
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
	return OptionsFromConfig(cfg, values), nil
}
