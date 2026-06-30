package pull

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"docker-manager/internal/textfmt"

	"github.com/Yui100901/MyGo/file_utils"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

func (r *PullRunner) downloadLayers(ctx context.Context, info *ImageInfo, manifest *ocispec.Manifest, auth *pullRegistryAuth, opts PullOptions, tempDir string) error {
	g, ctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, maxLayerConcurrency)

	for _, layer := range manifest.Layers {
		l := layer

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.Go(func() error {
			defer func() { <-sem }()
			return r.downloadLayer(ctx, info, l, auth, opts, tempDir)
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("层下载失败: %w", err)
	}
	return nil
}

func (r *PullRunner) downloadConfig(ctx context.Context, info *ImageInfo, manifest *ocispec.Manifest, auth *pullRegistryAuth, opts PullOptions, tempDir string) error {
	configURL := registryAPIURL(opts, info, "blobs", string(manifest.Config.Digest))
	digest := strings.TrimPrefix(string(manifest.Config.Digest), "sha256:")
	if digest == string(manifest.Config.Digest) {
		digest = strings.TrimPrefix(digest, "sha:")
	}
	configPath := filepath.Join(tempDir, digest+".json")

	_, err := r.saveRegistryFileWithRetry(ctx, configURL, nil, nil, info, opts, auth, configPath)
	return err
}

func (r *PullRunner) downloadLayer(ctx context.Context, info *ImageInfo, layer ocispec.Descriptor, auth *pullRegistryAuth, opts PullOptions, tempDir string) error {
	layerURL := registryAPIURL(opts, info, "blobs", string(layer.Digest))
	layerID := sha256Hash(string(layer.Digest))
	layerDir := filepath.Join(tempDir, layerID)
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return fmt.Errorf("创建层目录失败: %w", err)
	}

	gzPath := filepath.Join(layerDir, "layer.tar.gz")

	if _, err := r.saveRegistryFileWithRetry(ctx, layerURL, nil, nil, info, opts, auth, gzPath); err != nil {
		return fmt.Errorf("下载层失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyFileDigest(gzPath, layer.Digest); err != nil {
		return fmt.Errorf("校验层 digest 失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file_utils.DecompressGzip(gzPath, filepath.Join(layerDir, "layer.tar")); err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}

	return os.Remove(gzPath)
}

type httpStatusError struct {
	StatusCode int
	Status     string
	Header     http.Header
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.StatusCode, e.Status)
}

func (r *PullRunner) saveRegistryFileWithRetry(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth, outputPath string) (*pullRegistryAuth, error) {
	var lastErr error
	currentAuth := auth
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := ctx.Err(); err != nil {
			_ = removePartialDownload(outputPath)
			return currentAuth, err
		}
		nextAuth, err := r.saveRegistryFileOnce(ctx, rawURL, headers, query, info, opts, currentAuth, outputPath)
		if err == nil {
			return nextAuth, nil
		}
		currentAuth = nextAuth
		_ = removePartialDownload(outputPath)
		lastErr = err
		log.Printf("保存 %s 到 %s 失败（尝试 %d/%d）: %v，稍后重试...", rawURL, outputPath, i+1, maxHTTPRetries, err)
		if err := sleepWithContext(ctx, backoff); err != nil {
			_ = removePartialDownload(outputPath)
			return currentAuth, err
		}
		backoff *= 2
	}
	return currentAuth, lastErr
}

func (r *PullRunner) saveRegistryFileOnce(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth, outputPath string) (*pullRegistryAuth, error) {
	err := r.httpSaveToFileWithStatus(ctx, rawURL, authHeaders(headers, auth), query, outputPath, opts.ProgressOutput)
	if err == nil {
		return auth, nil
	}
	statusErr, ok := err.(*httpStatusError)
	if !ok || statusErr.StatusCode != http.StatusUnauthorized {
		return auth, err
	}
	nextAuth, err := r.resolveRegistryAuth(ctx, statusErr.Header.Get("WWW-Authenticate"), info, opts)
	if err != nil {
		return auth, err
	}
	err = r.httpSaveToFileWithStatus(ctx, rawURL, authHeaders(headers, nextAuth), query, outputPath, opts.ProgressOutput)
	return nextAuth, err
}

func (r *PullRunner) httpSaveToFile(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, outputPath string) error {
	return r.httpSaveToFileWithStatus(ctx, rawURL, headers, query, outputPath, io.Discard)
}

func (r *PullRunner) httpSaveToFileWithStatus(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, outputPath string, progressOutput io.Writer) error {
	req, err := buildGETRequest(ctx, rawURL, headers, query)
	if err != nil {
		return err
	}
	resp, err := r.httpClient.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Printf("警告: 关闭 HTTP response body 失败: %v", cerr)
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &httpStatusError{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone()}
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return err
	}

	partPath := partialDownloadPath(outputPath)
	file, err := os.Create(partPath)
	if err != nil {
		return err
	}
	reader := newDownloadProgressReader(resp.Body, progressOutput, downloadProgressLabel(rawURL, outputPath), resp.ContentLength)
	_, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(partPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(partPath)
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(partPath)
		return err
	}
	_ = os.Remove(outputPath)
	return os.Rename(partPath, outputPath)
}

type downloadProgressReader struct {
	reader     io.Reader
	output     io.Writer
	label      string
	total      int64
	downloaded int64
	started    time.Time
	lastReport time.Time
	enabled    bool
}

func newDownloadProgressReader(reader io.Reader, output io.Writer, label string, total int64) *downloadProgressReader {
	now := time.Now()
	return &downloadProgressReader{
		reader:     reader,
		output:     output,
		label:      label,
		total:      total,
		started:    now,
		lastReport: now,
		enabled:    output != nil && (total <= 0 || total >= 1024*1024),
	}
}

func (r *downloadProgressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.downloaded += int64(n)
		r.report(false)
	}
	if err == io.EOF {
		r.report(true)
	}
	return n, err
}

func (r *downloadProgressReader) report(final bool) {
	if !r.enabled {
		return
	}
	now := time.Now()
	if !final && now.Sub(r.lastReport) < downloadProgressInterval {
		return
	}
	r.lastReport = now
	elapsed := now.Sub(r.started).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	speed := float64(r.downloaded) / elapsed
	if final {
		progressPrintf(r.output, "下载完成 %s %s %s\n", r.label, textfmt.SignedBytes(r.downloaded), textfmt.Rate(speed))
		return
	}
	if r.total > 0 {
		percent := float64(r.downloaded) * 100 / float64(r.total)
		progressPrintf(r.output, "下载中 %s %s/%s %.1f%% %s\n", r.label, textfmt.SignedBytes(r.downloaded), textfmt.SignedBytes(r.total), percent, textfmt.Rate(speed))
		return
	}
	progressPrintf(r.output, "下载中 %s %s %s\n", r.label, textfmt.SignedBytes(r.downloaded), textfmt.Rate(speed))
}

func progressPrintf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	pullProgressMu.Lock()
	defer pullProgressMu.Unlock()
	_, _ = fmt.Fprintf(w, format, args...)
}

func downloadProgressLabel(rawURL, outputPath string) string {
	parsed, err := url.Parse(rawURL)
	if err == nil {
		path := parsed.Path
		if idx := strings.LastIndex(path, "/blobs/"); idx >= 0 {
			digestValue := strings.TrimPrefix(path[idx+len("/blobs/"):], "sha256:")
			if digestValue != "" {
				if len(digestValue) > 12 {
					digestValue = digestValue[:12]
				}
				return "sha256:" + digestValue
			}
		}
	}
	return filepath.Base(outputPath)
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func removePartialDownload(outputPath string) error {
	return os.Remove(partialDownloadPath(outputPath))
}

func partialDownloadPath(outputPath string) string {
	return outputPath + ".part"
}
