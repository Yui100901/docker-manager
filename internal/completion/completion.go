package completion

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/docker"
	"docker-manager/internal/dockerconfig"

	"github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

const completionTimeout = 5 * time.Second
const defaultConfigPath = appconfig.DefaultPath
const configEnvName = appconfig.EnvName

func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion <bash|zsh|fish|powershell>",
		Short: "生成 shell 自动补全脚本",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			default:
				return fmt.Errorf("不支持的 shell %q，请使用 bash、zsh、fish 或 powershell", args[0])
			}
		},
		ValidArgsFunction: FixedValues("bash", "zsh", "fish", "powershell"),
	}
	return cmd
}

func FixedValues(values ...string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return filterCompletionValues(values, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

func LocalContainers(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if err := prepareDockerCompletion(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	values, err := localContainerCompletionValues(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return filterCompletionValues(values, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func LocalImages(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if err := prepareDockerCompletion(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	values, err := localImageCompletionValues(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return filterCompletionValues(values, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func LocalVolumes(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if err := prepareDockerCompletion(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	values, err := localVolumeCompletionValues(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return filterCompletionValues(values, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func prepareDockerCompletion(cmd *cobra.Command) error {
	if cmd == nil {
		return nil
	}
	root := cmd.Root()
	if root == nil {
		root = cmd
	}
	flags := root.PersistentFlags()
	configPath := defaultConfigPath
	configFlagChanged := false
	if flag := flags.Lookup("config"); flag != nil {
		configPath = flag.Value.String()
		configFlagChanged = flag.Changed
	}
	cfg, err := appconfig.Load(appconfig.ResolvePath(configPath, configFlagChanged))
	if err != nil {
		return err
	}
	opts, err := dockerconfig.OptionsFromRootFlags(cfg, root)
	if err != nil {
		return err
	}
	docker.Configure(opts)
	return nil
}

func localContainerCompletionValues(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), completionTimeout)
	defer cancel()
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	result, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var values []string
	for _, c := range result.Items {
		name := firstContainerName(c.Names)
		if name != "" {
			values = append(values, name)
		}
		if id := shortID(c.ID); id != "" {
			values = append(values, id)
		}
	}
	return uniqueSorted(values), nil
}

func localImageCompletionValues(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), completionTimeout)
	defer cancel()
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	result, err := cli.ImageList(ctx, client.ImageListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var values []string
	for _, img := range result.Items {
		for _, tag := range img.RepoTags {
			if tag != "" && tag != "<none>:<none>" {
				values = append(values, tag)
			}
		}
		for _, digest := range img.RepoDigests {
			if digest != "" && digest != "<none>@<none>" {
				values = append(values, digest)
			}
		}
		if id := shortID(img.ID); id != "" {
			values = append(values, id)
		}
	}
	return uniqueSorted(values), nil
}

func localVolumeCompletionValues(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), completionTimeout)
	defer cancel()
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	result, err := cli.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(result.Items))
	for _, vol := range result.Items {
		if vol.Name != "" {
			values = append(values, vol.Name)
		}
	}
	return uniqueSorted(values), nil
}

func filterCompletionValues(values []string, toComplete string) []string {
	var result []string
	for _, value := range uniqueSorted(values) {
		if strings.HasPrefix(value, toComplete) {
			result = append(result, value)
		}
	}
	return result
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func firstContainerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
