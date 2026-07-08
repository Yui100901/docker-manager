package diagnostics

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	"docker-manager/internal/parallel"
	rpt "docker-manager/internal/report"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/spf13/cobra"
)

func NewImageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "镜像分析工具",
	}
	return cmd
}

func NewImageTreeCommand() *cobra.Command {
	opts := ImageTreeOptions{Top: 5}
	cmd := &cobra.Command{
		Use:   "tree <image>",
		Short: "展示镜像层、大小和构建历史",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := runImageTree(cmd.Context(), args[0], opts)
			if err != nil {
				return fmt.Errorf("生成镜像层报告失败: %w", err)
			}
			return rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printImageTreeReport(w, report, opts)
			})
		},
		ValidArgsFunction: completion.LocalImages,
	}
	cmd.Flags().BoolVar(&opts.NoTrunc, "no-trunc", false, "显示完整 layer ID 和构建命令")
	cmd.Flags().IntVar(&opts.Top, "top", opts.Top, "显示最大的前 N 个 layer，0 表示不显示")
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runImageTree(ctx context.Context, imageRef string, opts ImageTreeOptions) (ImageTreeReport, error) {
	svc, err := newImageTreeDockerService()
	if err != nil {
		return ImageTreeReport{}, err
	}
	inspect, err := svc.ImageInspect(ctx, imageRef)
	if err != nil {
		return ImageTreeReport{}, fmt.Errorf("inspect image %s: %w", imageRef, err)
	}
	history, err := svc.ImageHistory(ctx, imageRef)
	if err != nil {
		return ImageTreeReport{}, fmt.Errorf("history image %s: %w", imageRef, err)
	}
	images, err := svc.ImageList(ctx)
	if err != nil {
		return ImageTreeReport{}, fmt.Errorf("list images: %w", err)
	}
	containers, err := svc.ListContainers(ctx, true)
	if err != nil {
		return ImageTreeReport{}, fmt.Errorf("list containers: %w", err)
	}
	containerInspects, err := inspectImageTreeContainers(ctx, svc, containers)
	if err != nil {
		return ImageTreeReport{}, err
	}
	report := buildImageTreeReport(imageRef, inspect, history, opts)
	enrichImageTreeUsage(&report, images, containers, containerInspects)
	return report, nil
}

func inspectImageTreeContainers(ctx context.Context, svc imageTreeDockerService, containers []container.Summary) (map[string]container.InspectResponse, error) {
	results := make([]container.InspectResponse, len(containers))
	ok := make([]bool, len(containers))
	parallel.ForEachIndex(ctx, len(containers), diagnosticsInspectConcurrency, func(ctx context.Context, i int) {
		c := containers[i]
		ref := c.ID
		if ref == "" {
			ref = firstContainerName(c.Names)
		}
		if ref == "" {
			return
		}
		inspect, err := svc.InspectContainer(ctx, ref)
		if err != nil {
			return
		}
		results[i] = inspect
		ok[i] = true
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	containerInspects := make(map[string]container.InspectResponse, len(containers))
	for i, inspect := range results {
		if ok[i] {
			containerInspects[containers[i].ID] = inspect
		}
	}
	return containerInspects, nil
}

func buildImageTreeReport(imageRef string, inspect image.InspectResponse, history []image.HistoryResponseItem, opts ImageTreeOptions) ImageTreeReport {
	report := ImageTreeReport{
		DockerEndpoint: docker.Endpoint(),
		ImageRef:       imageRef,
		ID:             normalizeImageID(inspect.ID),
		RepoTags:       sortedStrings(inspect.RepoTags),
		RepoDigests:    sortedStrings(inspect.RepoDigests),
		Platform:       imagePlatform(inspect),
		Created:        inspect.Created,
		Size:           inspect.Size,
		RootFSType:     inspect.RootFS.Type,
		RootFSLayers:   append([]string(nil), inspect.RootFS.Layers...),
	}

	ordered := append([]image.HistoryResponseItem(nil), history...)
	reverseHistory(ordered)
	for i, item := range ordered {
		layer := ImageLayerInfo{
			Index:       i + 1,
			ID:          normalizeLayerID(item.ID),
			Created:     formatUnixTime(item.Created),
			CreatedBy:   cleanCreatedBy(item.CreatedBy),
			Size:        item.Size,
			SizePercent: sizePercent(item.Size, inspect.Size),
			Tags:        sortedStrings(item.Tags),
			Comment:     item.Comment,
			Metadata:    isMetadataLayer(item),
		}
		if item.Size > 0 {
			report.HistorySize += item.Size
		}
		if layer.Metadata {
			report.MetadataCount++
		} else {
			report.LayerCount++
		}
		report.History = append(report.History, layer)
	}

	report.LargestLayers = largestImageLayers(report.History, opts.Top)
	return report
}

func printImageTreeReport(w io.Writer, report ImageTreeReport, opts ImageTreeOptions) {
	fmt.Fprintf(w, "镜像层报告: %s\n", report.ImageRef)
	printDockerEndpoint(w, report.DockerEndpoint)
	fmt.Fprintf(w, "ID: %s\n", report.ID)
	fmt.Fprintf(w, "平台: %s\n", report.Platform)
	fmt.Fprintf(w, "大小: %s history_size=%s 文件系统层=%d history 条目=%d 元数据条目=%d\n", humanBytes(uint64FromInt64(report.Size)), humanBytes(uint64FromInt64(report.HistorySize)), len(report.RootFSLayers), len(report.History), report.MetadataCount)
	if len(report.RepoTags) > 0 {
		fmt.Fprintf(w, "Tag: %s\n", strings.Join(report.RepoTags, ", "))
	}
	if len(report.RepoDigests) > 0 {
		fmt.Fprintf(w, "Digest: %s\n", strings.Join(report.RepoDigests, ", "))
	}
	if len(report.LocalRefs.RepoTags) > 0 || len(report.LocalRefs.RepoDigests) > 0 {
		fmt.Fprintf(w, "Local refs: tags=%s digests=%s\n", strings.Join(report.LocalRefs.RepoTags, ", "), strings.Join(report.LocalRefs.RepoDigests, ", "))
	}
	if len(report.UsedBy) > 0 {
		fmt.Fprintf(w, "Used by containers:\n")
		for _, ref := range report.UsedBy {
			fmt.Fprintf(w, "  - %s id=%s image=%s image_id=%s state=%s status=%s\n", ref.Name, ref.ID, ref.Image, ref.ImageID, ref.State, ref.Status)
		}
	}

	if opts.Top > 0 && len(report.LargestLayers) > 0 {
		fmt.Fprintf(w, "\n最大 layer:\n")
		for _, layer := range report.LargestLayers {
			fmt.Fprintf(w, "  - #%d %s %.1f%% %s\n", layer.Index, humanBytes(uint64FromInt64(layer.Size)), layer.SizePercent, layer.CreatedBy)
		}
	}

	fmt.Fprintf(w, "\n构建历史 (base -> top):\n")
	if len(report.History) == 0 {
		fmt.Fprintln(w, "  无")
		return
	}
	for _, layer := range report.History {
		kind := "layer"
		if layer.Metadata {
			kind = "meta"
		}
		fmt.Fprintf(w, "  %2d. [%s] %s %5.1f%% id=%s\n", layer.Index, kind, humanBytes(uint64FromInt64(layer.Size)), layer.SizePercent, layer.ID)
		fmt.Fprintf(w, "      %s\n", layer.CreatedBy)
	}
}

func enrichImageTreeUsage(report *ImageTreeReport, images []image.Summary, containers []container.Summary, inspects map[string]container.InspectResponse) {
	targetID := normalizeImageID(report.ID)
	report.LocalRefs = imageLocalRefs(targetID, report.RepoTags, report.RepoDigests, images)
	report.UsedBy = imageUsedByContainers(targetID, containers, inspects)
}

func imageLocalRefs(targetID string, tags, digests []string, images []image.Summary) ImageLocalRefs {
	refs := ImageLocalRefs{
		ID:          targetID,
		RepoTags:    append([]string(nil), tags...),
		RepoDigests: append([]string(nil), digests...),
	}
	tagSet := make(map[string]bool, len(refs.RepoTags))
	for _, tag := range refs.RepoTags {
		tagSet[tag] = true
	}
	digestSet := make(map[string]bool, len(refs.RepoDigests))
	for _, digest := range refs.RepoDigests {
		digestSet[digest] = true
	}
	for _, img := range images {
		if normalizeImageID(img.ID) != targetID {
			continue
		}
		for _, tag := range img.RepoTags {
			if tag == "" || tag == "<none>:<none>" || tagSet[tag] {
				continue
			}
			refs.RepoTags = append(refs.RepoTags, tag)
			tagSet[tag] = true
		}
		for _, digest := range img.RepoDigests {
			if digest == "" || digest == "<none>@<none>" || digestSet[digest] {
				continue
			}
			refs.RepoDigests = append(refs.RepoDigests, digest)
			digestSet[digest] = true
		}
	}
	sort.Strings(refs.RepoTags)
	sort.Strings(refs.RepoDigests)
	return refs
}

func imageUsedByContainers(targetID string, containers []container.Summary, inspects map[string]container.InspectResponse) []ImageUsageRef {
	var refs []ImageUsageRef
	seen := map[string]bool{}
	for _, c := range containers {
		imageID := normalizeImageID(c.ImageID)
		if inspect, ok := inspects[c.ID]; ok && inspect.Image != "" {
			imageID = normalizeImageID(inspect.Image)
		}
		if imageID != targetID {
			continue
		}
		id := c.ID
		if id == "" {
			id = firstContainerName(c.Names)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := firstContainerName(c.Names)
		if name == "" {
			name = id
		}
		refs = append(refs, ImageUsageRef{
			ID:      id,
			Name:    name,
			Image:   c.Image,
			ImageID: imageID,
			State:   string(c.State),
			Status:  c.Status,
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Name == refs[j].Name {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Name < refs[j].Name
	})
	return refs
}

func largestImageLayers(layers []ImageLayerInfo, top int) []ImageLayerInfo {
	if top <= 0 {
		return nil
	}
	candidates := make([]ImageLayerInfo, 0, len(layers))
	for _, layer := range layers {
		if layer.Size <= 0 {
			continue
		}
		candidates = append(candidates, layer)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Size == candidates[j].Size {
			return candidates[i].Index < candidates[j].Index
		}
		return candidates[i].Size > candidates[j].Size
	})
	if len(candidates) > top {
		candidates = candidates[:top]
	}
	return candidates
}

func reverseHistory(history []image.HistoryResponseItem) {
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}
}

func normalizeLayerID(id string) string {
	if id == "" || id == "<missing>" {
		return "<missing>"
	}
	return strings.TrimPrefix(id, "sha256:")
}

func normalizeImageID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "sha256:")
}

func isMetadataLayer(item image.HistoryResponseItem) bool {
	return item.ID == "" || item.ID == "<missing>" || item.Size == 0
}

func cleanCreatedBy(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "/bin/sh -c #(nop) ")
	value = strings.TrimPrefix(value, "/bin/sh -c ")
	if value == "" {
		return "<unknown>"
	}
	return value
}

func imagePlatform(inspect image.InspectResponse) string {
	platform := inspect.Os
	if platform == "" {
		platform = "unknown"
	}
	if inspect.Architecture != "" {
		platform += "/" + inspect.Architecture
	}
	if inspect.Variant != "" {
		platform += "/" + inspect.Variant
	}
	return platform
}

func formatUnixTime(created int64) string {
	if created <= 0 {
		return ""
	}
	return time.Unix(created, 0).UTC().Format(time.RFC3339)
}

func sizePercent(size, total int64) float64 {
	if size <= 0 || total <= 0 {
		return 0
	}
	return float64(size) / float64(total) * 100
}

func displayLayerText(value string, noTrunc bool, max int) string {
	if noTrunc || max <= 0 || len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}
