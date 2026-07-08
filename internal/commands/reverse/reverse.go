package reverse

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/completion"
	"docker-manager/internal/sensitive"
	"docker-manager/internal/targets"

	"github.com/moby/moby/api/types/container"
	"github.com/spf13/cobra"
)

func NewReverseCommand() *cobra.Command {
	var (
		save            bool
		reverseType     string
		preserveVolumes bool
		noMergePorts    bool
		noDefaultEnvs   bool
		prettyFormat    bool
		running         bool
		redactSecrets   bool
		redactProfile   string
		filters         []string
	)

	cmd := &cobra.Command{
		Use:   "reverse [container-filter...]",
		Short: "逆向 Docker 容器到启动命令",
		RunE: func(cmd *cobra.Command, args []string) error {
			if running && !cmd.Flags().Changed("reverse-type") {
				reverseType = string(ReverseCompose)
			}

			// 校验输出类型
			rt := ReverseType(reverseType)
			switch rt {
			case ReverseCmd, ReverseCompose, ReverseAll:
				// ok
			default:
				return fmt.Errorf("无效的输出类型: %s (必须是 cmd | compose | all)", reverseType)
			}

			// 传递选项
			if _, err := sensitive.NormalizeProfile(redactProfile, redactSecrets); err != nil {
				return err
			}
			effectiveFilterDefaultEnvs := true
			if noDefaultEnvs {
				effectiveFilterDefaultEnvs = false
			}
			effectiveMergePorts := true
			if noMergePorts {
				effectiveMergePorts = false
			}
			opts := ReverseOptions{
				PreserveVolumes:   preserveVolumes,
				FilterDefaultEnvs: effectiveFilterDefaultEnvs,
				PrettyFormat:      prettyFormat,
				MergePorts:        effectiveMergePorts,
				Save:              save,
				ReverseType:       rt,
				RedactSecrets:     redactSecrets,
				RedactProfile:     redactProfile,
			}

			targetFilters := append(append([]string(nil), filters...), args...)
			ctx := cmd.Context()
			targets, err := resolveReverseContainerTargetsContext(ctx, targetFilters, running)
			if err != nil {
				return err
			}

			if comment := reverseTargetSelectionComment(len(targets), running, targetFilters); comment != "" {
				fmt.Fprintln(cmd.OutOrStdout(), comment)
			}

			reverseResult, err := reverseWithOptions(ctx, targets, opts)
			if err != nil {
				return err
			}

			// 打印输出
			reverseResult.Print(cmd.OutOrStdout())

			// 保存输出
			if save {
				if err := reverseResult.saveOutput(); err != nil {
					return fmt.Errorf("保存输出失败: %w", err)
				}
			}

			return nil
		},
		ValidArgsFunction: completion.LocalContainers,
	}

	cmd.Flags().BoolVarP(&save, "save", "s", false, "保存输出到文件")
	cmd.Flags().StringVarP(&reverseType, "reverse-type", "t", "cmd", "输出类型: cmd | compose | all")
	cmd.Flags().BoolVar(&preserveVolumes, "preserve-volumes", false, "是否保留匿名卷名称（默认关闭）")
	cmd.Flags().BoolVar(&noDefaultEnvs, "no-default-envs", false, "不过滤 Docker 默认环境变量")
	cmd.Flags().BoolVar(&noMergePorts, "no-merge-ports", false, "不合并连续端口")
	cmd.Flags().BoolVar(&prettyFormat, "pretty", false, "是否格式化输出 docker run 命令（默认关闭）")
	commandflags.AddContainerFilterFlags(cmd, &running, &filters, "仅筛选正在运行的容器；未指定 --reverse-type 时默认输出 compose")
	commandflags.AddRedactFlags(cmd, &redactSecrets, &redactProfile, "脱敏 env/label 中疑似敏感字段，便于分享输出")
	_ = cmd.RegisterFlagCompletionFunc("reverse-type", completeReverseTypes)

	return cmd
}

func reverseTargetSelectionComment(count int, running bool, filters []string) string {
	if len(filters) > 0 {
		return ""
	}
	if running {
		return fmt.Sprintf("# 目标: --running 筛选运行中容器 %d 个", count)
	}
	return fmt.Sprintf("# 目标: 未指定容器筛选，默认解析全部本地容器 %d 个", count)
}

func completeReverseTypes(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	values := []string{string(ReverseCmd), string(ReverseCompose), string(ReverseAll)}
	var suggestions []string
	for _, value := range values {
		if strings.HasPrefix(value, toComplete) {
			suggestions = append(suggestions, value)
		}
	}
	return suggestions, cobra.ShellCompDirectiveNoFileComp
}

func listRunningContainerNames() ([]string, error) {
	return listRunningContainerNamesContext(context.Background())
}

func listRunningContainerNamesContext(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureContainerManager(); err != nil {
		return nil, err
	}
	containers, err := containerManager.ListAllContext(ctx)
	if err != nil {
		return nil, err
	}
	return runningContainerNames(containers), nil
}

func resolveReverseContainerTargets(filters []string, runningOnly bool) ([]string, error) {
	return resolveReverseContainerTargetsContext(context.Background(), filters, runningOnly)
}

func resolveReverseContainerTargetsContext(ctx context.Context, filters []string, runningOnly bool) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureContainerManager(); err != nil {
		return nil, err
	}
	containers, err := containerManager.ListAllContext(ctx)
	if err != nil {
		return nil, err
	}
	if runningOnly {
		containers = filterReverseRunningContainers(containers)
	}
	containers = filterReverseContainers(containers, filters)
	if len(containers) == 0 {
		switch {
		case runningOnly && len(filters) == 0:
			return nil, fmt.Errorf("没有正在运行的容器")
		case len(filters) == 0:
			return nil, fmt.Errorf("没有可解析的本地容器")
		default:
			return nil, fmt.Errorf("容器筛选条件 %q 未匹配任何容器", strings.Join(filters, ", "))
		}
	}
	return reverseContainerNames(containers), nil
}

func runningContainerNames(containers []container.Summary) []string {
	return targets.ContainerNames(targets.RunningContainers(containers))
}

func filterReverseRunningContainers(containers []container.Summary) []container.Summary {
	return targets.RunningContainers(containers)
}

func reverseContainerNames(containers []container.Summary) []string {
	return targets.ContainerNames(containers)
}

func expandContainerNamePatterns(args []string) ([]string, error) {
	return expandContainerNamePatternsContext(context.Background(), args)
}

func expandContainerNamePatternsContext(ctx context.Context, args []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hasWildcard := false
	for _, arg := range args {
		if strings.ContainsAny(arg, "*?") {
			hasWildcard = true
			break
		}
	}
	if !hasWildcard {
		return args, nil
	}
	if err := ensureContainerManager(); err != nil {
		return nil, err
	}
	containers, err := containerManager.ListAllContext(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var expanded []string
	for _, arg := range args {
		if !strings.ContainsAny(arg, "*?") {
			if !seen[arg] {
				seen[arg] = true
				expanded = append(expanded, arg)
			}
			continue
		}
		matches := matchingContainerNames(containers, arg)
		if len(matches) == 0 {
			return nil, fmt.Errorf("容器通配符 %q 未匹配任何容器", arg)
		}
		for _, name := range matches {
			if !seen[name] {
				seen[name] = true
				expanded = append(expanded, name)
			}
		}
	}
	return expanded, nil
}

func filterReverseContainers(containers []container.Summary, filters []string) []container.Summary {
	return targets.FilterContainers(containers, filters)
}

func matchingContainerNames(containers []container.Summary, pattern string) []string {
	var names []string
	for _, c := range containers {
		if !reverseContainerMatchesPattern(c, pattern) {
			continue
		}
		name := reverseContainerDisplayName(c)
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func reverseContainerMatchesPattern(c container.Summary, pattern string) bool {
	return reverseContainerMatchesFilter(c, pattern)
}

func reverseContainerMatchesAnyFilter(c container.Summary, filters []string) bool {
	return targets.ContainerMatchesFilters(c, filters)
}

func reverseContainerMatchesFilter(c container.Summary, filter string) bool {
	return targets.ContainerMatchesFilter(c, filter)
}

func reverseContainerDisplayName(c container.Summary) string {
	return targets.ContainerDisplayName(c)
}
