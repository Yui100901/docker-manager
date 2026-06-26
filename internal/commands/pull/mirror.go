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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	rpt "docker-manager/internal/report"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
)

const (
	mirrorStatusSuccess = "success"
	mirrorStatusFailed  = "failed"
	mirrorStatusSkipped = "skipped"
)

type PullMirrorOptions struct {
	File           string
	Images         []string
	To             string
	OutputDir      string
	DockerConfig   string
	PlainHTTP      bool
	Concurrency    int
	Retries        int
	SkipExisting   bool
	Resume         bool
	StateFile      string
	ReportFile     string
	ProgressOutput io.Writer
	rpt.FormatOptions
}

type PullMirrorReport struct {
	GeneratedAt string             `json:"generated_at"`
	To          string             `json:"to"`
	OutputDir   string             `json:"output_dir,omitempty"`
	StateFile   string             `json:"state_file,omitempty"`
	Total       int                `json:"total"`
	Succeeded   int                `json:"succeeded"`
	Failed      int                `json:"failed"`
	Skipped     int                `json:"skipped"`
	Items       []PullMirrorResult `json:"items"`
}

type PullMirrorResult struct {
	Image      string `json:"image"`
	Target     string `json:"target,omitempty"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts,omitempty"`
	Message    string `json:"message,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type pullMirrorState struct {
	UpdatedAt string                         `json:"updated_at"`
	Items     map[string]pullMirrorStateItem `json:"items"`
}

type pullMirrorStateItem struct {
	Image      string `json:"image"`
	Target     string `json:"target,omitempty"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts,omitempty"`
	Message    string `json:"message,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type mirrorPullFunc func(image string, opts PullOptions) error
type mirrorExistsFunc func(ctx context.Context, image, target string, opts PullOptions) (bool, error)

func NewPullMirrorCommandWithDefaults(defaults func() CommandDefaults) *cobra.Command {
	var targetOS string
	var arch string
	var proxy string
	var verboseHTTP bool
	opts := PullMirrorOptions{
		OutputDir:   ".",
		Concurrency: 1,
		Retries:     1,
	}
	cmd := &cobra.Command{
		Use:   "mirror [images...]",
		Short: "批量拉取镜像并推送到目标 registry",
		Long: `批量拉取镜像并推送到目标 registry。
可通过 --file 读取镜像列表；空行和以 # 开头的行会被忽略。
每个镜像会复用 pull --to 的拉取、导入、tag、push、认证和 registry 预检流程。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			applyCommandDefaults(cmd, defaults, &proxy, &targetOS, &arch, &opts.OutputDir)
			configureHTTPLogging(verboseHTTP)
			runner, err := NewPullRunner(proxy, targetOS, arch)
			if err != nil {
				return fmt.Errorf("配置代理失败: %w", err)
			}
			runOpts := opts
			runOpts.Images = append(append([]string(nil), opts.Images...), args...)
			runOpts.ProgressOutput = cmd.OutOrStdout()
			report, err := runPullMirror(ctx, runner, runOpts)
			printErr := rpt.Print(cmd.OutOrStdout(), runOpts.Format, report, func(w io.Writer) {
				printPullMirrorReport(w, report)
			})
			if printErr != nil {
				return printErr
			}
			if runOpts.ReportFile != "" {
				if writeErr := writePullMirrorReport(runOpts.ReportFile, report); writeErr != nil {
					return writeErr
				}
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&opts.File, "file", "f", "", "镜像列表文件，空行和 # 注释会被忽略")
	cmd.Flags().StringVar(&opts.To, "to", "", "目标 registry/repository，例如 registry.local:5000 或 registry.local/mirror")
	cmd.Flags().StringVarP(&targetOS, "os", "", "linux", "目标操作系统")
	cmd.Flags().StringVarP(&arch, "arch", "a", "amd64", "目标架构")
	cmd.Flags().StringVar(&proxy, "proxy", "", "强制指定 HTTP 代理，例如 http://127.0.0.1:7890；为空时使用环境变量代理")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", ".", "临时镜像 tar 输出目录")
	cmd.Flags().StringVar(&opts.DockerConfig, "docker-config", "", "Docker config.json 路径，默认使用 DOCKER_CONFIG/config.json 或 ~/.docker/config.json")
	cmd.Flags().BoolVar(&opts.PlainHTTP, "plain-http", false, "使用 http:// 拉取/检查 registry，适用于未启用 TLS 的内网 registry")
	cmd.Flags().IntVar(&opts.Concurrency, "concurrency", opts.Concurrency, "并发同步数量")
	cmd.Flags().IntVar(&opts.Retries, "retries", opts.Retries, "单个镜像失败后的重试次数")
	cmd.Flags().BoolVar(&opts.SkipExisting, "skip-existing", false, "目标 registry 已存在同名 manifest 时跳过")
	cmd.Flags().BoolVar(&opts.Resume, "resume", false, "读取状态文件并跳过已经成功的镜像")
	cmd.Flags().StringVar(&opts.StateFile, "state-file", "", "状态文件路径，默认写入 <output-dir>/pull-mirror-state.json")
	cmd.Flags().StringVar(&opts.ReportFile, "report", "", "额外写入 JSON 汇总报告文件")
	cmd.Flags().BoolVar(&verboseHTTP, "verbose-http", false, "输出底层 HTTP 请求调试日志")
	rpt.AddFormatFlag(cmd, &opts.Format)
	_ = cmd.RegisterFlagCompletionFunc("os", completePullValues("linux", "windows"))
	_ = cmd.RegisterFlagCompletionFunc("arch", completePullValues("amd64", "arm64", "arm", "386", "ppc64le", "s390x"))
	return cmd
}

func runPullMirror(ctx context.Context, runner *PullRunner, opts PullMirrorOptions) (PullMirrorReport, error) {
	return runPullMirrorWithDeps(ctx, opts, runner.getImage, runner.targetManifestExists)
}

func runPullMirrorWithDeps(ctx context.Context, opts PullMirrorOptions, pull mirrorPullFunc, exists mirrorExistsFunc) (PullMirrorReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	images, err := loadPullMirrorImages(opts.Images, opts.File)
	if err != nil {
		return PullMirrorReport{}, err
	}
	if opts.To == "" {
		return PullMirrorReport{}, fmt.Errorf("pull mirror 必须指定 --to")
	}
	if len(images) == 0 {
		return PullMirrorReport{}, fmt.Errorf("pull mirror 需要至少一个镜像，可通过位置参数或 --file 指定")
	}
	if len(images) > 1 && isTaggedImageRef(opts.To) {
		return PullMirrorReport{}, fmt.Errorf("--to 使用完整镜像名时只能同步单个镜像；批量同步请使用 registry 或 namespace 前缀")
	}
	if opts.Concurrency <= 0 {
		return PullMirrorReport{}, fmt.Errorf("--concurrency 必须大于 0")
	}
	if opts.Retries < 0 {
		return PullMirrorReport{}, fmt.Errorf("--retries 不能小于 0")
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "."
	}
	if opts.StateFile == "" {
		opts.StateFile = filepath.Join(opts.OutputDir, "pull-mirror-state.json")
	}
	progressOutput := opts.ProgressOutput
	if progressOutput == nil {
		progressOutput = io.Discard
	}

	state := pullMirrorState{Items: map[string]pullMirrorStateItem{}}
	if opts.Resume {
		loaded, err := readPullMirrorState(opts.StateFile)
		if err != nil {
			return PullMirrorReport{}, err
		}
		state = loaded
	}
	if state.Items == nil {
		state.Items = map[string]pullMirrorStateItem{}
	}
	resumeState := pullMirrorState{Items: copyPullMirrorStateItems(state.Items)}

	report := PullMirrorReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		To:          opts.To,
		OutputDir:   opts.OutputDir,
		StateFile:   opts.StateFile,
		Total:       len(images),
		Items:       make([]PullMirrorResult, len(images)),
	}

	var mu sync.Mutex
	jobs := make(chan int)
	workerCount := opts.Concurrency
	if workerCount > len(images) {
		workerCount = len(images)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				result := runPullMirrorItem(ctx, images[idx], opts, resumeState, pull, exists, progressOutput)
				mu.Lock()
				report.Items[idx] = result
				updatePullMirrorReportCounts(&report)
				updatePullMirrorStateItem(state, result)
				if err := writePullMirrorState(opts.StateFile, state); err != nil && result.Status != mirrorStatusFailed {
					report.Items[idx].Status = mirrorStatusFailed
					report.Items[idx].Message = "写入状态文件失败: " + err.Error()
				}
				mu.Unlock()
			}
		}()
	}
	for idx := range images {
		if err := ctx.Err(); err != nil {
			break
		}
		jobs <- idx
	}
	close(jobs)
	wg.Wait()

	updatePullMirrorReportCounts(&report)
	if report.Failed > 0 {
		return report, fmt.Errorf("pull mirror 完成但存在失败项: total=%d success=%d skipped=%d failed=%d", report.Total, report.Succeeded, report.Skipped, report.Failed)
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	return report, nil
}

func updatePullMirrorStateItem(state pullMirrorState, result PullMirrorResult) {
	if result.Status == mirrorStatusSkipped {
		if existing, ok := state.Items[result.Image]; ok && existing.Status == mirrorStatusSuccess && existing.Target == result.Target {
			return
		}
	}
	state.Items[result.Image] = pullMirrorStateItem{
		Image:      result.Image,
		Target:     result.Target,
		Status:     result.Status,
		Attempts:   result.Attempts,
		Message:    result.Message,
		StartedAt:  result.StartedAt,
		FinishedAt: result.FinishedAt,
	}
}

func runPullMirrorItem(ctx context.Context, imageName string, opts PullMirrorOptions, state pullMirrorState, pull mirrorPullFunc, exists mirrorExistsFunc, progressOutput io.Writer) PullMirrorResult {
	startedAt := time.Now().Format(time.RFC3339)
	result := PullMirrorResult{Image: imageName, Status: mirrorStatusFailed, StartedAt: startedAt}
	target, err := resolveMirrorTarget(imageName, opts.To)
	if err != nil {
		result.Message = err.Error()
		result.FinishedAt = time.Now().Format(time.RFC3339)
		return result
	}
	result.Target = target
	if opts.Resume {
		if item, ok := state.Items[imageName]; ok && item.Status == mirrorStatusSuccess && item.Target == target {
			result.Status = mirrorStatusSkipped
			result.Message = "状态文件中已成功，跳过"
			result.FinishedAt = time.Now().Format(time.RFC3339)
			return result
		}
	}
	pullOpts := PullOptions{
		Context:        ctx,
		OutputDir:      opts.OutputDir,
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
			result.Status = mirrorStatusSkipped
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
			lastErr = err
			continue
		}
		result.Status = mirrorStatusSuccess
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
	_, _, err = r.fetchRegistryBytesOnce(ctx, registryAPIURL(opts, info, "manifests", getReference(info)), headers, nil, info, opts, nil)
	if err == nil {
		return true, nil
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func resolveMirrorTarget(imageName, to string) (string, error) {
	info, err := parseImageInfo(imageName)
	if err != nil {
		return "", fmt.Errorf("镜像名称解析失败: %w", err)
	}
	return resolvePushTarget(info, to)
}

func loadPullMirrorImages(args []string, file string) ([]string, error) {
	var values []string
	values = append(values, args...)
	if file != "" {
		fromFile, err := readPullMirrorImageFile(file)
		if err != nil {
			return nil, err
		}
		values = append(values, fromFile...)
	}
	return uniquePullMirrorImages(values), nil
}

func readPullMirrorImageFile(path string) ([]string, error) {
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

func uniquePullMirrorImages(values []string) []string {
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

func readPullMirrorState(path string) (pullMirrorState, error) {
	state := pullMirrorState{Items: map[string]pullMirrorStateItem{}}
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
		state.Items = map[string]pullMirrorStateItem{}
	}
	return state, nil
}

func copyPullMirrorStateItems(items map[string]pullMirrorStateItem) map[string]pullMirrorStateItem {
	copied := map[string]pullMirrorStateItem{}
	for key, value := range items {
		copied[key] = value
	}
	return copied
}

func writePullMirrorState(path string, state pullMirrorState) error {
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicJSON(path, data)
}

func writePullMirrorReport(path string, report PullMirrorReport) error {
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

func updatePullMirrorReportCounts(report *PullMirrorReport) {
	report.Succeeded = 0
	report.Failed = 0
	report.Skipped = 0
	for _, item := range report.Items {
		switch item.Status {
		case mirrorStatusSuccess:
			report.Succeeded++
		case mirrorStatusSkipped:
			report.Skipped++
		case mirrorStatusFailed:
			report.Failed++
		}
	}
}

func printPullMirrorReport(w io.Writer, report PullMirrorReport) {
	_, _ = fmt.Fprintf(w, "Pull mirror summary: total=%d success=%d skipped=%d failed=%d\n", report.Total, report.Succeeded, report.Skipped, report.Failed)
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
