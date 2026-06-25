package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"docker-manager/docker"

	"github.com/docker/docker/api/types/container"
	imageapi "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/spf13/cobra"
)

const completionTimeout = 2 * time.Second

func newCompletionCommand() *cobra.Command {
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
		ValidArgsFunction: completeFixedValues("bash", "zsh", "fish", "powershell"),
	}
	return cmd
}

func completeFixedValues(values ...string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return filterCompletionValues(values, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

func completeLocalContainers(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	values, err := localContainerCompletionValues(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return filterCompletionValues(values, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeLocalImages(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	values, err := localImageCompletionValues(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return filterCompletionValues(values, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeLocalVolumes(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	values, err := localVolumeCompletionValues(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return filterCompletionValues(values, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func localContainerCompletionValues(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), completionTimeout)
	defer cancel()
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var values []string
	for _, c := range containers {
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
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	images, err := cli.ImageList(ctx, imageapi.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var values []string
	for _, img := range images {
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
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	volumes, err := cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(volumes.Volumes))
	for _, vol := range volumes.Volumes {
		if vol != nil && vol.Name != "" {
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
