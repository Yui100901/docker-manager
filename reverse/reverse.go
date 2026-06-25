package reverse

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/spf13/cobra"
)

func NewReverseCommand() *cobra.Command {
	var (
		rerun             bool
		save              bool
		reverseType       string
		preserveVolumes   bool
		mergePorts        bool
		filterDefaultEnvs bool
		prettyFormat      bool
		dryRun            bool
		confirm           bool
		running           bool
		redactSecrets     bool
	)

	cmd := &cobra.Command{
		Use:   "reverse [name...]",
		Short: "逆向 Docker 容器到启动命令",
		RunE: func(cmd *cobra.Command, args []string) error {
			if running && len(args) > 0 {
				return fmt.Errorf("不能同时指定容器名称和 --running")
			}
			if len(args) == 0 && !running {
				return fmt.Errorf("必须提供至少一个容器名称")
			}
			if rerun && !dryRun && !confirm {
				return fmt.Errorf("reverse --rerun 会停止、删除并重建容器；如确认执行，请添加 --confirm；如仅审计，请使用 --dry-run")
			}
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
			opts := ReverseOptions{
				PreserveVolumes:   preserveVolumes,
				FilterDefaultEnvs: filterDefaultEnvs,
				PrettyFormat:      prettyFormat,
				MergePorts:        mergePorts,
				Rerun:             rerun,
				Save:              save,
				ReverseType:       rt,
				DryRun:            dryRun,
				Confirm:           confirm,
				RedactSecrets:     redactSecrets,
			}

			if running {
				names, err := listRunningContainerNames()
				if err != nil {
					return err
				}
				if len(names) == 0 {
					return fmt.Errorf("没有正在运行的容器")
				}
				args = names
			} else {
				names, err := expandContainerNamePatterns(args)
				if err != nil {
					return err
				}
				args = names
			}

			reverseResult, err := reverseWithOptions(args, opts)
			if err != nil {
				return err
			}

			// 打印输出
			reverseResult.Print()

			// 保存输出
			if save {
				if err := reverseResult.saveOutput(); err != nil {
					return fmt.Errorf("保存输出失败: %w", err)
				}
			}

			// 重新运行容器
			if rerun {
				if err := reverseResult.rerunContainers(); err != nil {
					return fmt.Errorf("重新运行容器失败: %w", err)
				}
			}

			return nil
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if err := ensureContainerManager(); err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			containers, err := containerManager.ListAll()
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}

			var suggestions []string
			for _, c := range containers {
				if len(c.Names) > 0 {
					name := strings.TrimPrefix(c.Names[0], "/")
					if strings.HasPrefix(name, toComplete) {
						suggestions = append(suggestions, name)
					}
				}
			}

			return suggestions, cobra.ShellCompDirectiveNoFileComp
		},
	}

	cmd.Flags().BoolVarP(&rerun, "rerun", "r", false, "逆向解析完成后删除原有容器并重新创建容器（谨慎使用），cmd 模式下会调用 Docker API 而不是运行命令行")
	cmd.Flags().BoolVarP(&save, "save", "s", false, "保存输出到文件")
	cmd.Flags().StringVarP(&reverseType, "reverse-type", "t", "cmd", "输出类型: cmd | compose | all")
	cmd.Flags().BoolVar(&preserveVolumes, "preserve-volumes", false, "是否保留匿名卷名称（默认关闭）")
	cmd.Flags().BoolVar(&filterDefaultEnvs, "filter-default-envs", true, "是否过滤掉 Docker 默认环境变量（默认开启）")
	cmd.Flags().BoolVar(&mergePorts, "merge-ports", true, "命令是否合并连续端口，compose 无法合并（默认开启）")
	cmd.Flags().BoolVar(&prettyFormat, "pretty", false, "是否格式化输出 docker run 命令（默认关闭）")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "仅打印将要执行的操作，不实际重新运行容器（用于审计/确认）")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "确认执行 --rerun 的停止、删除并重建容器操作")
	cmd.Flags().BoolVar(&running, "running", false, "反向解析当前正在运行的所有容器；未指定 --reverse-type 时默认输出 compose")
	cmd.Flags().BoolVar(&redactSecrets, "redact-secrets", false, "脱敏 env/label 中疑似敏感字段，便于分享输出")
	_ = cmd.RegisterFlagCompletionFunc("reverse-type", completeReverseTypes)

	return cmd
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
	if err := ensureContainerManager(); err != nil {
		return nil, err
	}
	containers, err := containerManager.ListAll()
	if err != nil {
		return nil, err
	}
	return runningContainerNames(containers), nil
}

func runningContainerNames(containers []container.Summary) []string {
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if name == "" {
			name = c.ID
		}
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func expandContainerNamePatterns(args []string) ([]string, error) {
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
	containers, err := containerManager.ListAll()
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
	for _, candidate := range reverseContainerCandidates(c) {
		if wildcardMatch(pattern, candidate) {
			return true
		}
	}
	return false
}

func reverseContainerCandidates(c container.Summary) []string {
	candidates := []string{c.ID}
	if len(c.ID) > 12 {
		candidates = append(candidates, c.ID[:12])
	}
	for _, name := range c.Names {
		candidates = append(candidates, strings.TrimPrefix(name, "/"))
	}
	if c.Image != "" {
		candidates = append(candidates, c.Image)
	}
	return candidates
}

func reverseContainerDisplayName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID
}

func wildcardMatch(pattern, value string) bool {
	re, err := regexp.Compile("^" + wildcardToRegex(pattern) + "$")
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

func wildcardToRegex(pattern string) string {
	var sb strings.Builder
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteByte('.')
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return sb.String()
}
