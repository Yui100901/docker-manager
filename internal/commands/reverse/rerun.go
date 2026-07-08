package reverse

import (
	"context"
	"docker-manager/internal/commandflags"
	"docker-manager/internal/docker"
	"fmt"
	"io"
	"time"

	"docker-manager/internal/completion"

	"github.com/spf13/cobra"
)

func NewRerunCommand() *cobra.Command {
	var (
		dryRun  bool
		confirm bool
		running bool
		filters []string
	)

	cmd := &cobra.Command{
		Use:   "rerun [container-filter...]",
		Short: "基于 Docker inspect 停止、删除并重建容器",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && len(filters) == 0 && !running {
				return fmt.Errorf("rerun 是破坏性操作，必须提供容器名称/筛选条件，或显式使用 --running")
			}
			if !dryRun && !confirm {
				return fmt.Errorf("%s；如确认执行，请添加 --confirm；如仅审计，请使用 --dry-run", destructiveDockerMessage("rerun 会停止、删除并重建容器"))
			}
			targetFilters := append(append([]string(nil), filters...), args...)
			ctx := cmd.Context()
			targets, err := resolveReverseContainerTargetsContext(ctx, targetFilters, running)
			if err != nil {
				return err
			}
			if !dryRun {
				printDestructiveDockerTarget(cmd.OutOrStdout())
			}
			return rerunContainers(ctx, targets, rerunOptions{
				DryRun: dryRun,
				Output: cmd.OutOrStdout(),
			})
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "仅打印将要执行的重建动作，不修改 Docker")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "确认执行停止、删除并重建容器操作")
	commandflags.AddContainerFilterFlags(cmd, &running, &filters, "仅筛选正在运行的容器")
	return cmd
}

func destructiveDockerMessage(action string) string {
	if docker.IsRemoteEndpoint() {
		return fmt.Sprintf("%s；目标 Docker: %s", action, docker.Endpoint())
	}
	return action
}

func printDestructiveDockerTarget(w io.Writer) {
	if docker.IsRemoteEndpoint() {
		fmt.Fprintf(w, "Target Docker: %s\n", docker.Endpoint())
	}
}

type rerunOptions struct {
	DryRun bool
	Output io.Writer
}

func rerunContainers(ctx context.Context, names []string, opts rerunOptions) error {
	output := opts.Output
	if output == nil {
		output = io.Discard
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureContainerManager(); err != nil {
		return err
	}
	var firstErr error
	backupDir := inspectBackupDir(time.Now())
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if opts.DryRun {
			fmt.Fprintf(output, "Dry run: backup inspect for %s to %s\n", name, inspectBackupPath(backupDir, name))
			fmt.Fprintf(output, "Dry run: stop, remove and recreate container %s via Docker API\n", name)
			continue
		}
		backupPath, err := backupContainerInspectContext(ctx, name, backupDir)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("备份容器 %s inspect 失败: %w", name, err)
			}
			continue
		}
		fmt.Fprintf(output, "Backup inspect %s to %s\n", name, backupPath)

		containerID, err := containerManager.RecreateContainerContext(ctx, name, name)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("重建容器 %s 失败: %w", name, err)
			}
			continue
		}
		fmt.Fprintf(output, "Recreate container %s id %s\n", name, containerID)
	}
	return firstErr
}
