package reverse

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"docker-manager/internal/completion"

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
		filters           []string
	)

	cmd := &cobra.Command{
		Use:   "reverse [container-filter...]",
		Short: "逆向 Docker 容器到启动命令",
		RunE: func(cmd *cobra.Command, args []string) error {
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

			targetFilters := append(append([]string(nil), filters...), args...)
			targets, err := resolveReverseContainerTargets(targetFilters, running)
			if err != nil {
				return err
			}

			reverseResult, err := reverseWithOptions(targets, opts)
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
		ValidArgsFunction: completion.LocalContainers,
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
	cmd.Flags().BoolVar(&running, "running", false, "仅筛选正在运行的容器；未指定 --reverse-type 时默认输出 compose")
	cmd.Flags().StringArrayVarP(&filters, "filter", "f", nil, "筛选容器，支持 name:/id:/image:/state:/status:/label: 和 * ? 通配符，可重复指定")
	cmd.Flags().BoolVar(&redactSecrets, "redact-secrets", false, "脱敏 env/label 中疑似敏感字段，便于分享输出")
	_ = cmd.RegisterFlagCompletionFunc("reverse-type", completeReverseTypes)
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalContainers)

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

func resolveReverseContainerTargets(filters []string, runningOnly bool) ([]string, error) {
	if err := ensureContainerManager(); err != nil {
		return nil, err
	}
	containers, err := containerManager.ListAll()
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
	return reverseContainerNames(filterReverseRunningContainers(containers))
}

func filterReverseRunningContainers(containers []container.Summary) []container.Summary {
	var running []container.Summary
	for _, c := range containers {
		if c.State == "running" {
			running = append(running, c)
		}
	}
	return running
}

func reverseContainerNames(containers []container.Summary) []string {
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		name := reverseContainerDisplayName(c)
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

func filterReverseContainers(containers []container.Summary, filters []string) []container.Summary {
	if len(filters) == 0 {
		sort.Slice(containers, func(i, j int) bool {
			return reverseContainerDisplayName(containers[i]) < reverseContainerDisplayName(containers[j])
		})
		return containers
	}
	var filtered []container.Summary
	for _, c := range containers {
		if reverseContainerMatchesAnyFilter(c, filters) {
			filtered = append(filtered, c)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return reverseContainerDisplayName(filtered[i]) < reverseContainerDisplayName(filtered[j])
	})
	return filtered
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
	for _, filter := range filters {
		if reverseContainerMatchesFilter(c, filter) {
			return true
		}
	}
	return false
}

func reverseContainerMatchesFilter(c container.Summary, filter string) bool {
	key, pattern, keyed := splitReverseFilter(filter)
	if strings.TrimSpace(pattern) == "" {
		return false
	}
	for _, candidate := range reverseContainerCandidates(c) {
		candidateKey, candidateValue, candidateKeyed := splitReverseFilter(candidate)
		if keyed {
			if !candidateKeyed || candidateKey != key {
				continue
			}
			if reverseValueMatchesFilter(pattern, candidateValue) {
				return true
			}
			continue
		}
		if reverseValueMatchesFilter(pattern, candidate) {
			return true
		}
	}
	return false
}

func reverseContainerCandidates(c container.Summary) []string {
	cleanID := strings.TrimPrefix(c.ID, "sha256:")
	candidates := []string{c.ID, cleanID, "id:" + c.ID, "id:" + cleanID}
	if len(cleanID) > 12 {
		candidates = append(candidates, cleanID[:12], "id:"+cleanID[:12])
	}
	for _, name := range c.Names {
		name = strings.TrimPrefix(name, "/")
		candidates = append(candidates, name, "name:"+name)
	}
	if c.Image != "" {
		candidates = append(candidates, c.Image, "image:"+c.Image)
		repo, tag := splitReverseRepoTag(c.Image)
		if repo != "" {
			candidates = append(candidates, repo, "image:"+repo)
			if slash := strings.Index(repo, "/"); slash >= 0 && slash < len(repo)-1 {
				candidates = append(candidates, repo[slash+1:], "image:"+repo[slash+1:])
			}
			if slash := strings.LastIndex(repo, "/"); slash >= 0 && slash < len(repo)-1 {
				candidates = append(candidates, repo[slash+1:], "image:"+repo[slash+1:])
			}
		}
		if tag != "" {
			candidates = append(candidates, tag, "image:"+tag)
		}
	}
	if c.ImageID != "" {
		imageID := strings.TrimPrefix(c.ImageID, "sha256:")
		candidates = append(candidates, c.ImageID, imageID, "id:"+imageID)
	}
	if c.State != "" {
		candidates = append(candidates, string(c.State), "state:"+string(c.State))
	}
	if c.Status != "" {
		candidates = append(candidates, c.Status, "status:"+c.Status)
	}
	for key, value := range c.Labels {
		candidates = append(candidates, key, "label:"+key)
		if value != "" {
			candidates = append(candidates, value, key+"="+value, "label:"+key+"="+value)
		}
	}
	return uniqueReverseCandidates(candidates)
}

func reverseContainerDisplayName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID
}

func splitReverseFilter(filter string) (string, string, bool) {
	filter = strings.TrimSpace(filter)
	for _, sep := range []string{":", "="} {
		if idx := strings.Index(filter, sep); idx > 0 {
			key := strings.ToLower(strings.TrimSpace(filter[:idx]))
			if isReverseFilterKey(key) {
				return key, strings.TrimSpace(filter[idx+1:]), true
			}
		}
	}
	return "", filter, false
}

func isReverseFilterKey(key string) bool {
	switch key {
	case "name", "id", "image", "state", "status", "label":
		return true
	default:
		return false
	}
}

func reverseValueMatchesFilter(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	value = strings.TrimSpace(value)
	if pattern == "" || value == "" {
		return false
	}
	if wildcardMatch(pattern, value) || strings.EqualFold(pattern, value) || strings.HasPrefix(strings.ToLower(value), strings.ToLower(pattern)) {
		return true
	}
	if strings.ContainsAny(pattern, "*?") {
		return wildcardMatch(strings.ToLower(pattern), strings.ToLower(value))
	}
	return false
}

func splitReverseRepoTag(ref string) (string, string) {
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, ""
}

func uniqueReverseCandidates(values []string) []string {
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
	return result
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
