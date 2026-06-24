package main

import (
	"os"

	"docker-manager/pull"
	"docker-manager/reverse"

	"github.com/spf13/cobra"
)

//
// @Author yfy2001
// @Date 2025/7/18 09 43
//

func main() {
	cfg := appConfig{}
	opts := outputOptions{}
	rootCmd := newRootCommand(&cfg, &opts)
	if err := rootCmd.Execute(); err != nil {
		writeCommandError(rootCmd.ErrOrStderr(), err, opts)
		os.Exit(1)
	}
}

func newRootCommand(cfg *appConfig, opts *outputOptions) *cobra.Command {
	configPath := defaultConfigPath
	rootCmd := &cobra.Command{
		Use:           "dm <command>",
		Short:         "Docker manager helper\nAuthor:Yui",
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			loaded, err := loadAppConfig(configPath)
			if err != nil {
				return err
			}
			*cfg = loaded
			applyOutputDefaults(cmd, cfg, opts)
			configureLogging(*opts)
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}

	opts.Verbose = cfg.Verbose
	opts.Quiet = cfg.Quiet
	opts.JSON = cfg.JSON
	rootCmd.PersistentFlags().StringVar(&configPath, "config", defaultConfigPath, "config file path")
	rootCmd.PersistentFlags().BoolVar(&opts.Verbose, "verbose", opts.Verbose, "enable verbose logs")
	rootCmd.PersistentFlags().BoolVar(&opts.Quiet, "quiet", opts.Quiet, "suppress info logs")
	rootCmd.PersistentFlags().BoolVar(&opts.JSON, "json", opts.JSON, "emit machine-readable JSON logs and errors")

	rootCmd.AddCommand(newLoadCommand())
	rootCmd.AddCommand(newSaveCommandWithDefaults(func() string { return cfg.OutputDir }))
	rootCmd.AddCommand(newBackupCommand())
	rootCmd.AddCommand(newRestoreCommand())
	rootCmd.AddCommand(newPruneReportCommand())
	rootCmd.AddCommand(newNetworkCommand())
	rootCmd.AddCommand(newHealthCommand())
	rootCmd.AddCommand(newInspectDiffCommand())
	rootCmd.AddCommand(newImageCommand())
	rootCmd.AddCommand(newVolumeCommand())
	rootCmd.AddCommand(reverse.NewReverseCommand())
	rootCmd.AddCommand(pull.NewPullCommandWithDefaults(func() pull.CommandDefaults {
		return pull.CommandDefaults{
			Proxy:     cfg.Proxy,
			TargetOS:  cfg.TargetOS,
			Arch:      cfg.Arch,
			OutputDir: cfg.OutputDir,
		}
	}))
	return rootCmd
}

func applyOutputDefaults(cmd *cobra.Command, cfg *appConfig, opts *outputOptions) {
	flags := cmd.Root().PersistentFlags()
	if !flags.Changed("verbose") {
		opts.Verbose = cfg.Verbose
	}
	if !flags.Changed("quiet") {
		opts.Quiet = cfg.Quiet
	}
	if !flags.Changed("json") {
		opts.JSON = cfg.JSON
	}
	if flags.Changed("verbose") && opts.Verbose {
		opts.Quiet = false
	}
	if flags.Changed("quiet") && opts.Quiet {
		opts.Verbose = false
	}
}
