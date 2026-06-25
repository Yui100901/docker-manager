package diagnostics

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"docker-manager/docker"
	"docker-manager/internal/completion"
	rpt "docker-manager/internal/report"

	imageapi "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

type imageTreeDockerService interface {
	ImageInspect(ctx context.Context, imageRef string) (imageapi.InspectResponse, error)
	ImageHistory(ctx context.Context, imageRef string) ([]imageapi.HistoryResponseItem, error)
}

var newImageTreeDockerService = func() (imageTreeDockerService, error) {
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerImageTreeService{cli: cli}, nil
}

type dockerImageTreeService struct {
	cli *client.Client
}

type ImageTreeOptions struct {
	NoTrunc bool
	Top     int
	rpt.FormatOptions
}

type ImageTreeReport struct {
	ImageRef      string           `json:"image_ref"`
	ID            string           `json:"id"`
	RepoTags      []string         `json:"repo_tags,omitempty"`
	RepoDigests   []string         `json:"repo_digests,omitempty"`
	Platform      string           `json:"platform,omitempty"`
	Created       string           `json:"created,omitempty"`
	Size          int64            `json:"size"`
	RootFSType    string           `json:"rootfs_type,omitempty"`
	RootFSLayers  []string         `json:"rootfs_layers,omitempty"`
	HistorySize   int64            `json:"history_size"`
	LayerCount    int              `json:"layer_count"`
	MetadataCount int              `json:"metadata_count"`
	History       []ImageLayerInfo `json:"history"`
	LargestLayers []ImageLayerInfo `json:"largest_layers,omitempty"`
}

type ImageLayerInfo struct {
	Index       int      `json:"index"`
	ID          string   `json:"id"`
	Created     string   `json:"created,omitempty"`
	CreatedBy   string   `json:"created_by,omitempty"`
	Size        int64    `json:"size"`
	SizePercent float64  `json:"size_percent,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Comment     string   `json:"comment,omitempty"`
	Metadata    bool     `json:"metadata"`
}

func NewImageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "镜像分析工具",
	}
	cmd.AddCommand(newImageTreeCommand())
	return cmd
}

func newImageTreeCommand() *cobra.Command {
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
	rpt.AddFormatFlag(cmd, &opts.Format)
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
	return buildImageTreeReport(imageRef, inspect, history, opts), nil
}

func buildImageTreeReport(imageRef string, inspect imageapi.InspectResponse, history []imageapi.HistoryResponseItem, opts ImageTreeOptions) ImageTreeReport {
	report := ImageTreeReport{
		ImageRef:     imageRef,
		ID:           shortID(inspect.ID),
		RepoTags:     sortedStrings(inspect.RepoTags),
		RepoDigests:  sortedStrings(inspect.RepoDigests),
		Platform:     imagePlatform(inspect),
		Created:      inspect.Created,
		Size:         inspect.Size,
		RootFSType:   inspect.RootFS.Type,
		RootFSLayers: append([]string(nil), inspect.RootFS.Layers...),
	}

	ordered := append([]imageapi.HistoryResponseItem(nil), history...)
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
	fmt.Fprintf(w, "ID: %s\n", displayLayerText(report.ID, opts.NoTrunc, 20))
	fmt.Fprintf(w, "平台: %s\n", report.Platform)
	fmt.Fprintf(w, "大小: %s history_size=%s 文件系统层=%d history 条目=%d 元数据条目=%d\n", humanBytes(uint64FromInt64(report.Size)), humanBytes(uint64FromInt64(report.HistorySize)), len(report.RootFSLayers), len(report.History), report.MetadataCount)
	if len(report.RepoTags) > 0 {
		fmt.Fprintf(w, "Tag: %s\n", strings.Join(report.RepoTags, ", "))
	}
	if len(report.RepoDigests) > 0 {
		fmt.Fprintf(w, "Digest: %s\n", strings.Join(report.RepoDigests, ", "))
	}

	if opts.Top > 0 && len(report.LargestLayers) > 0 {
		fmt.Fprintf(w, "\n最大 layer:\n")
		for _, layer := range report.LargestLayers {
			fmt.Fprintf(w, "  - #%d %s %.1f%% %s\n", layer.Index, humanBytes(uint64FromInt64(layer.Size)), layer.SizePercent, displayLayerText(layer.CreatedBy, opts.NoTrunc, 120))
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
		fmt.Fprintf(w, "  %2d. [%s] %s %5.1f%% id=%s\n", layer.Index, kind, humanBytes(uint64FromInt64(layer.Size)), layer.SizePercent, displayLayerText(layer.ID, opts.NoTrunc, 20))
		fmt.Fprintf(w, "      %s\n", displayLayerText(layer.CreatedBy, opts.NoTrunc, 120))
	}
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

func reverseHistory(history []imageapi.HistoryResponseItem) {
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

func isMetadataLayer(item imageapi.HistoryResponseItem) bool {
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

func imagePlatform(inspect imageapi.InspectResponse) string {
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

func (s *dockerImageTreeService) ImageInspect(ctx context.Context, imageRef string) (imageapi.InspectResponse, error) {
	return s.cli.ImageInspect(ctx, imageRef)
}

func (s *dockerImageTreeService) ImageHistory(ctx context.Context, imageRef string) ([]imageapi.HistoryResponseItem, error) {
	return s.cli.ImageHistory(ctx, imageRef)
}
