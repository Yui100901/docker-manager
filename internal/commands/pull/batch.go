package pull

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/parallel"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	pullBatchStatusSuccess = "success"
	pullBatchStatusFailed  = "failed"
	pullBatchStatusSkipped = "skipped"
)

type PullBatchOptions struct {
	File           string
	Images         []string
	To             string
	OutputDir      string
	Load           bool
	DockerConfig   string
	PlainHTTP      bool
	Concurrency    int
	Retries        int
	SkipExisting   bool
	Resume         bool
	StateFile      string
	ReportFile     string
	ProgressOutput io.Writer
	commandflags.FormatOptions
}

type PullBatchReport struct {
	GeneratedAt string            `json:"generated_at"`
	To          string            `json:"to"`
	OutputDir   string            `json:"output_dir,omitempty"`
	StateFile   string            `json:"state_file,omitempty"`
	Total       int               `json:"total"`
	Succeeded   int               `json:"succeeded"`
	Failed      int               `json:"failed"`
	Skipped     int               `json:"skipped"`
	Items       []PullBatchResult `json:"items"`
}

type PullBatchResult struct {
	Image      string `json:"image"`
	Target     string `json:"target,omitempty"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts,omitempty"`
	Message    string `json:"message,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type pullBatchState struct {
	UpdatedAt string                        `json:"updated_at"`
	Items     map[string]pullBatchStateItem `json:"items"`
}

type pullBatchStateItem struct {
	Image      string `json:"image"`
	Target     string `json:"target,omitempty"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts,omitempty"`
	Message    string `json:"message,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type pullBatchFunc func(image string, opts PullOptions) error
type pullBatchExistsFunc func(ctx context.Context, image, target string, opts PullOptions) (bool, error)

func runPullBatch(ctx context.Context, runner *PullRunner, opts PullBatchOptions) (PullBatchReport, error) {
	return runPullBatchWithDeps(ctx, opts, runner.getImage, runner.targetManifestExists)
}

func runPullBatchWithDeps(ctx context.Context, opts PullBatchOptions, pull pullBatchFunc, exists pullBatchExistsFunc) (PullBatchReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	images, err := loadPullBatchImages(opts.Images, opts.File)
	if err != nil {
		return PullBatchReport{}, err
	}
	if len(images) == 0 {
		return PullBatchReport{}, fmt.Errorf("pull 需要至少一个镜像，可通过位置参数或 --file 指定")
	}
	if opts.To != "" && len(images) > 1 && isTaggedImageRef(opts.To) {
		return PullBatchReport{}, fmt.Errorf("--to 使用完整镜像名时只能同步单个镜像；批量同步请使用 registry 或 namespace 前缀")
	}
	if opts.SkipExisting && opts.To == "" {
		return PullBatchReport{}, fmt.Errorf("--skip-existing 需要配合 --to 使用")
	}
	if opts.Concurrency <= 0 {
		return PullBatchReport{}, fmt.Errorf("--concurrency 必须大于 0")
	}
	if opts.Retries < 0 {
		return PullBatchReport{}, fmt.Errorf("--retries 不能小于 0")
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "."
	}
	if opts.StateFile == "" {
		opts.StateFile = filepath.Join(opts.OutputDir, "pull-state.json")
	}
	progressOutput := opts.ProgressOutput
	if progressOutput == nil {
		progressOutput = io.Discard
	}

	state := pullBatchState{Items: map[string]pullBatchStateItem{}}
	if opts.Resume {
		loaded, err := readPullBatchState(opts.StateFile)
		if err != nil {
			return PullBatchReport{}, err
		}
		state = loaded
	}
	if state.Items == nil {
		state.Items = map[string]pullBatchStateItem{}
	}
	resumeState := pullBatchState{Items: copyPullBatchStateItems(state.Items)}

	report := PullBatchReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		To:          opts.To,
		OutputDir:   opts.OutputDir,
		StateFile:   opts.StateFile,
		Total:       len(images),
		Items:       make([]PullBatchResult, len(images)),
	}

	var mu sync.Mutex
	parallel.ForEachIndex(ctx, len(images), opts.Concurrency, func(ctx context.Context, idx int) {
		result := runPullBatchItem(ctx, images[idx], opts, resumeState, pull, exists, progressOutput)
		mu.Lock()
		defer mu.Unlock()
		report.Items[idx] = result
		updatePullBatchReportCounts(&report)
		updatePullBatchStateItem(state, result)
		if err := writePullBatchState(opts.StateFile, state); err != nil && result.Status != pullBatchStatusFailed {
			report.Items[idx].Status = pullBatchStatusFailed
			report.Items[idx].Message = "写入状态文件失败: " + err.Error()
		}
	})

	updatePullBatchReportCounts(&report)
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("pull 批量完成但存在失败项: total=%d success=%d skipped=%d failed=%d", report.Total, report.Succeeded, report.Skipped, report.Failed)
	}
	return report, nil
}

func updatePullBatchStateItem(state pullBatchState, result PullBatchResult) {
	if result.Status == pullBatchStatusSkipped {
		if existing, ok := state.Items[result.Image]; ok && existing.Status == pullBatchStatusSuccess && existing.Target == result.Target {
			return
		}
	}
	state.Items[result.Image] = pullBatchStateItem{
		Image:      result.Image,
		Target:     result.Target,
		Status:     result.Status,
		Attempts:   result.Attempts,
		Message:    result.Message,
		StartedAt:  result.StartedAt,
		FinishedAt: result.FinishedAt,
	}
}

func runPullBatchItem(ctx context.Context, imageName string, opts PullBatchOptions, state pullBatchState, pull pullBatchFunc, exists pullBatchExistsFunc, progressOutput io.Writer) PullBatchResult {
	startedAt := time.Now().Format(time.RFC3339)
	result := PullBatchResult{Image: imageName, Status: pullBatchStatusFailed, StartedAt: startedAt}
	target := ""
	if opts.To != "" {
		resolved, err := resolvePullBatchTarget(imageName, opts.To)
		if err != nil {
			result.Message = err.Error()
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
		target = resolved
	}
	result.Target = target
	if opts.Resume {
		if item, ok := state.Items[imageName]; ok && item.Status == pullBatchStatusSuccess && item.Target == target {
			result.Status = pullBatchStatusSkipped
			result.Message = "状态文件中已成功，跳过"
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
	}
	pullOpts := PullOptions{
		Context:        ctx,
		OutputDir:      opts.OutputDir,
		Load:           opts.Load,
		To:             opts.To,
		DockerConfig:   opts.DockerConfig,
		PlainHTTP:      opts.PlainHTTP,
		ProgressOutput: progressOutput,
	}
	if opts.SkipExisting {
		found, err := exists(ctx, imageName, target, pullOpts)
		if err != nil {
			result.Message = "检查目标 manifest 失败: " + err.Error()
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
		if found {
			result.Status = pullBatchStatusSkipped
			result.Message = "目标 registry 已存在，跳过"
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
	}

	var lastErr error
	maxAttempts := opts.Retries + 1
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result.Attempts = attempt
		if err := ctx.Err(); err != nil {
			result.Message = err.Error()
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
		if err := pull(imageName, pullOpts); err != nil {
			if errors.Is(err, context.Canceled) {
				result.Message = err.Error()
				result.FinishedAt = time.Now().Format(time.RFC3339)
				return result
			}
			lastErr = err
			continue
		}
		result.Status = pullBatchStatusSuccess
		result.Message = "同步成功"
		result.FinishedAt = time.Now().Format(time.RFC3339)
		return result
	}
	if lastErr != nil {
		result.Message = lastErr.Error()
	}
	result.FinishedAt = time.Now().Format(time.RFC3339)
	return result
}

func (r *PullRunner) targetManifestExists(ctx context.Context, imageName, target string, opts PullOptions) (bool, error) {
	info, err := parseImageInfo(target)
	if err != nil {
		return false, err
	}
	headers := map[string]string{
		"Accept": strings.Join([]string{
			dockerManifestV2,
			dockerManifestListV2,
			ocispec.MediaTypeImageManifest,
			ocispec.MediaTypeImageIndex,
		}, ", "),
	}
	targetOpts := opts
	targetOpts.PlainHTTP = pushTargetUsesPlainHTTP(opts)
	_, _, err = r.fetchRegistryBytesOnce(ctx, registryAPIURL(targetOpts, info, "manifests", getReference(info)), headers, nil, info, targetOpts, nil)
	if err == nil {
		return true, nil
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func resolvePullBatchTarget(imageName, to string) (string, error) {
	info, err := parseImageInfo(imageName)
	if err != nil {
		return "", fmt.Errorf("镜像名称解析失败: %w", err)
	}
	return resolvePushTarget(info, to)
}

func loadPullBatchImages(args []string, file string) ([]string, error) {
	var values []string
	values = append(values, args...)
	if file != "" {
		fromFile, err := readPullBatchImageFile(file)
		if err != nil {
			return nil, err
		}
		values = append(values, fromFile...)
	}
	return uniquePullBatchImages(values), nil
}

func readPullBatchImageFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取镜像列表失败: %w", err)
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "警告: 关闭镜像列表文件 %s 失败: %v\n", path, cerr)
		}
	}()
	var images []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		images = append(images, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取镜像列表失败: %w", err)
	}
	return images, nil
}

func uniquePullBatchImages(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.HasPrefix(value, "#") || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func readPullBatchState(path string) (pullBatchState, error) {
	state := pullBatchState{Items: map[string]pullBatchStateItem{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, fmt.Errorf("读取状态文件失败: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("解析状态文件失败: %w", err)
	}
	if state.Items == nil {
		state.Items = map[string]pullBatchStateItem{}
	}
	return state, nil
}

func copyPullBatchStateItems(items map[string]pullBatchStateItem) map[string]pullBatchStateItem {
	copied := map[string]pullBatchStateItem{}
	for key, value := range items {
		copied[key] = value
	}
	return copied
}

func writePullBatchState(path string, state pullBatchState) error {
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicJSON(path, data)
}

func writePullBatchReport(path string, report PullBatchReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicJSON(path, data)
}

func writeAtomicJSON(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Rename(tmp, path)
}

func updatePullBatchReportCounts(report *PullBatchReport) {
	report.Succeeded = 0
	report.Failed = 0
	report.Skipped = 0
	for _, item := range report.Items {
		switch item.Status {
		case pullBatchStatusSuccess:
			report.Succeeded++
		case pullBatchStatusSkipped:
			report.Skipped++
		case pullBatchStatusFailed:
			report.Failed++
		}
	}
}

func printPullBatchReport(w io.Writer, report PullBatchReport) {
	_, _ = fmt.Fprintf(w, "Pull summary: total=%d success=%d skipped=%d failed=%d\n", report.Total, report.Succeeded, report.Skipped, report.Failed)
	if report.To != "" {
		_, _ = fmt.Fprintf(w, "Target: %s\n", report.To)
	}
	if report.StateFile != "" {
		_, _ = fmt.Fprintf(w, "State file: %s\n", report.StateFile)
	}
	for _, item := range report.Items {
		_, _ = fmt.Fprintf(w, "- [%s] %s", item.Status, item.Image)
		if item.Target != "" {
			_, _ = fmt.Fprintf(w, " -> %s", item.Target)
		}
		if item.Attempts > 0 {
			_, _ = fmt.Fprintf(w, " attempts=%d", item.Attempts)
		}
		if item.Message != "" {
			_, _ = fmt.Fprintf(w, " (%s)", item.Message)
		}
		_, _ = fmt.Fprintln(w)
	}
}
