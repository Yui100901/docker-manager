package reverse

import (
	"fmt"

	"github.com/spf13/cobra"
)

func NewReverseCommand() *cobra.Command {
	var (
		rerun       bool
		save        bool
		reverseType string
	)

	cmd := &cobra.Command{
		Use:   "reverse <name...>",
		Short: "逆向 Docker 容器到启动命令",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("必须提供至少一个容器名称")
			}

			results, err := reverse(args)
			if err != nil {
				return err
			}

			rt := ReverseType(reverseType)

			// 构建输出
			cmdMap, composeMap := buildOutput(results, rt)

			// 打印输出
			printOutput(cmdMap, composeMap, rt)

			// rerun
			if rerun {
				if err := rerunContainers(cmdMap, composeMap, rt); err != nil {
					return err
				}
			}

			// save
			if save {
				if err := saveOutput(cmdMap, composeMap, rt); err != nil {
					return err
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&rerun, "rerun", "r", false, "逆向解析完成后以解析出的命令重新创建容器")
	cmd.Flags().BoolVarP(&save, "save", "s", false, "保存输出到文件")
	cmd.Flags().StringVarP(&reverseType, "reverse-type", "t", "cmd", "输出类型: cmd | compose | all")

	return cmd
}
