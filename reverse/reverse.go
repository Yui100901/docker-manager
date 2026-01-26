package reverse

import (
	"fmt"
	"strings"

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
	)

	cmd := &cobra.Command{
		Use:   "reverse <name...>",
		Short: "逆向 Docker 容器到启动命令",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("必须提供至少一个容器名称")
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

	cmd.Flags().BoolVarP(&rerun, "rerun", "r", false, "逆向解析完成后删除原有容器并重新创建容器（谨慎使用），cmd模式下会调用docker api而不是运行命令行")
	cmd.Flags().BoolVarP(&save, "save", "s", false, "保存输出到文件")
	cmd.Flags().StringVarP(&reverseType, "reverse-type", "t", "cmd", "输出类型: cmd | compose | all")
	cmd.Flags().BoolVar(&preserveVolumes, "preserve-volumes", false, "是否保留匿名卷名称（默认关闭）")
	cmd.Flags().BoolVar(&filterDefaultEnvs, "filter-default-envs", true, "是否过滤掉 Docker 默认环境变量（默认开启）")
	cmd.Flags().BoolVar(&mergePorts, "merge-ports", true, "命令是否合并连续端口，compose无法合并（默认开启）")
	cmd.Flags().BoolVar(&prettyFormat, "pretty", false, "是否格式化输出 docker run 命令（默认关闭）")

	return cmd
}
