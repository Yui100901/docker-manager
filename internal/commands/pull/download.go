package pull

import (
	"compress/gzip"
	"context"
	"errors"
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

	"github.com/klauspost/compress/zstd"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

const (
	maxLayerTarBytes     int64  = 512 << 30
	maxZstdDecoderMemory uint64 = 256 << 20
	minZstdDecoderMemory uint64 = 8 << 20
)

func (r *PullRunner) downloadLayers(ctx context.Context, info *ImageInfo, manifest *ocispec.Manifest, auth *pullRegistryAuth, opts PullOptions, tempDir string) error {
	if err := validateManifestLayers(manifest, opts.Limits); err != nil {
		return err
	}
	if opts.resourceBudget == nil {
		initialBytes, err := workspaceRegularBytes(ctx, tempDir)
		if err != nil {
			return err
		}
		budget, err := newPullResourceBudget(opts.Limits, initialBytes)
		if err != nil {
			return err
		}
		opts.resourceBudget = budget
	}
	layers := uniqueLayerDescriptors(manifest.Layers)
	err := downloadLayersWithFunc(ctx, layers, func(workerCtx context.Context, layer ocispec.Descriptor) error {
		return r.downloadLayer(workerCtx, info, layer, auth, opts, tempDir)
	})
	if err != nil {
		return fmt.Errorf("层下载失败: %w", err)
	}
	return nil
}

func downloadLayersWithFunc(ctx context.Context, layers []ocispec.Descriptor, download func(context.Context, ocispec.Descriptor) error) error {
	g, ctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, maxLayerConcurrency)
	var scheduleErr error

schedule:
	for _, layer := range layers {
		l := layer

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			scheduleErr = ctx.Err()
			break schedule
		}
		g.Go(func() error {
			defer func() { <-sem }()
			return download(ctx, l)
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}
	return scheduleErr
}

func uniqueLayerDescriptors(layers []ocispec.Descriptor) []ocispec.Descriptor {
	seen := make(map[string]struct{}, len(layers))
	result := make([]ocispec.Descriptor, 0, len(layers))
	for _, layer := range layers {
		key := layer.Digest.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, layer)
	}
	return result
}

func (r *PullRunner) downloadConfig(ctx context.Context, info *ImageInfo, manifest *ocispec.Manifest, auth *pullRegistryAuth, opts PullOptions, tempDir string) error {
	limits := effectivePullResourceLimits(opts.Limits)
	configFileName, err := configBlobFileNameWithLimit(manifest.Config, limits.ConfigBytes)
	if err != nil {
		return err
	}

	configURL := registryAPIURL(opts, info, "blobs", manifest.Config.Digest.String())
	data, _, err := r.fetchRegistryBytesWithRetryLimit(ctx, configURL, nil, nil, info, opts, auth, boundedReadLimit(manifest.Config.Size, limits.ConfigBytes))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyDescriptorBytes(data, manifest.Config, "镜像 config"); err != nil {
		return err
	}
	return writeFileWithinRoot(tempDir, configFileName, data, 0644)
}

func (r *PullRunner) downloadLayer(ctx context.Context, info *ImageInfo, layer ocispec.Descriptor, auth *pullRegistryAuth, opts PullOptions, tempDir string) error {
	limits := effectivePullResourceLimits(opts.Limits)
	if err := validateLayerDescriptor(layer, limits); err != nil {
		return err
	}
	budget := opts.resourceBudget
	if budget == nil {
		var err error
		budget, err = newPullResourceBudget(limits, 0)
		if err != nil {
			return err
		}
	}
	if err := budget.reserve(0, layer.Size); err != nil {
		return err
	}
	compressedReserved := true
	var expandedReserved int64
	success := false
	layerURL := registryAPIURL(opts, info, "blobs", string(layer.Digest))
	layerID := sha256Hash(string(layer.Digest))
	layerDir := filepath.Join(tempDir, layerID)
	defer func() {
		if success {
			return
		}
		_ = os.RemoveAll(layerDir)
		if expandedReserved > 0 {
			budget.release(expandedReserved, expandedReserved)
		}
		if compressedReserved {
			budget.release(0, layer.Size)
		}
	}()
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return fmt.Errorf("创建层目录失败: %w", err)
	}

	blobPath := filepath.Join(layerDir, "layer.blob")
	tarPath := filepath.Join(layerDir, "layer.tar")

	if _, err := r.saveRegistryFileWithRetry(ctx, layerURL, nil, nil, info, opts, auth, blobPath, boundedReadLimit(layer.Size, limits.LayerBytes)); err != nil {
		return fmt.Errorf("下载层失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyFileDigestWithContext(ctx, blobPath, layer.Digest); err != nil {
		return fmt.Errorf("校验层 digest 失败: %w", err)
	}
	if fileInfo, err := os.Stat(blobPath); err != nil {
		return err
	} else if fileInfo.Size() != layer.Size {
		return fmt.Errorf("镜像层大小校验失败: 期望 %d，实际 %d", layer.Size, fileInfo.Size())
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	materialized, err := materializeLayerTarWithBudget(ctx, blobPath, tarPath, layer.MediaType, limits.ExpandedLayerBytes, budget)
	if err != nil {
		return err
	}
	expandedReserved = materialized.expandedBytes
	if !materialized.reusedBlob {
		budget.release(0, layer.Size)
	}
	compressedReserved = false
	success = true

	return nil
}

func materializeLayerTar(blobPath, tarPath, mediaType string) error {
	return materializeLayerTarWithContext(context.Background(), blobPath, tarPath, mediaType, maxLayerTarBytes)
}

func materializeLayerTarWithContext(ctx context.Context, blobPath, tarPath, mediaType string, expandedLimit int64) error {
	_, err := materializeLayerTarWithBudget(ctx, blobPath, tarPath, mediaType, expandedLimit, nil)
	return err
}

type materializedLayer struct {
	expandedBytes int64
	reusedBlob    bool
}

func materializeLayerTarWithBudget(ctx context.Context, blobPath, tarPath, mediaType string, expandedLimit int64, budget *pullResourceBudget) (materializedLayer, error) {
	if expandedLimit <= 0 {
		return materializedLayer{}, fmt.Errorf("层展开大小上限必须大于 0")
	}
	isGzip, err := fileHasGzipHeader(blobPath)
	if err != nil {
		return materializedLayer{}, fmt.Errorf("读取层文件失败: %w", err)
	}
	isZstd, err := fileHasZstdHeader(blobPath)
	if err != nil {
		return materializedLayer{}, fmt.Errorf("读取层文件失败: %w", err)
	}
	if isGzip || isGzipLayerMediaType(mediaType) {
		written, err := decompressGzipFileWithBudget(ctx, blobPath, tarPath, expandedLimit, budget)
		if err != nil {
			return materializedLayer{}, fmt.Errorf("解压失败: %w", err)
		}
		if err := os.Remove(blobPath); err != nil {
			_ = os.Remove(tarPath)
			budget.release(written, written)
			return materializedLayer{}, err
		}
		return materializedLayer{expandedBytes: written}, nil
	}
	if isZstd || isZstdLayerMediaType(mediaType) {
		written, err := decompressZstdFileWithBudget(ctx, blobPath, tarPath, expandedLimit, budget)
		if err != nil {
			return materializedLayer{}, fmt.Errorf("解压失败: %w", err)
		}
		if err := os.Remove(blobPath); err != nil {
			_ = os.Remove(tarPath)
			budget.release(written, written)
			return materializedLayer{}, err
		}
		return materializedLayer{expandedBytes: written}, nil
	}
	info, err := os.Lstat(blobPath)
	if err != nil {
		return materializedLayer{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return materializedLayer{}, fmt.Errorf("未压缩层必须是普通文件")
	}
	if info.Size() > expandedLimit {
		return materializedLayer{}, fmt.Errorf("层展开后大小 %d 超过上限 %d", info.Size(), expandedLimit)
	}
	if err := ctx.Err(); err != nil {
		return materializedLayer{}, err
	}
	if err := budget.reserve(info.Size(), 0); err != nil {
		return materializedLayer{}, err
	}
	keepReservation := false
	defer func() {
		if !keepReservation {
			budget.release(info.Size(), 0)
		}
	}()
	_ = os.Remove(tarPath)
	if err := os.Rename(blobPath, tarPath); err != nil {
		return materializedLayer{}, fmt.Errorf("保存未压缩层失败: %w", err)
	}
	keepReservation = true
	return materializedLayer{expandedBytes: info.Size(), reusedBlob: true}, nil
}

func fileHasGzipHeader(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			log.Printf("警告: 关闭文件 %s 失败: %v", path, cerr)
		}
	}()
	var header [2]byte
	n, err := io.ReadFull(file, header[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, err
	}
	return n == 2 && header[0] == 0x1f && header[1] == 0x8b, nil
}

func fileHasZstdHeader(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			log.Printf("警告: 关闭文件 %s 失败: %v", path, cerr)
		}
	}()
	var header [4]byte
	n, err := io.ReadFull(file, header[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, err
	}
	return n == 4 && header == [4]byte{0x28, 0xb5, 0x2f, 0xfd}, nil
}

func decompressGzipFile(srcPath, dstPath string) error {
	return decompressGzipFileWithLimit(context.Background(), srcPath, dstPath, maxLayerTarBytes)
}

func decompressGzipFileWithLimit(ctx context.Context, srcPath, dstPath string, expandedLimit int64) error {
	_, err := decompressGzipFileWithBudget(ctx, srcPath, dstPath, expandedLimit, nil)
	return err
}

func decompressGzipFileWithBudget(ctx context.Context, srcPath, dstPath string, expandedLimit int64, budget *pullResourceBudget) (int64, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := src.Close(); cerr != nil {
			log.Printf("警告: 关闭文件 %s 失败: %v", srcPath, cerr)
		}
	}()
	gzr, err := gzip.NewReader(src)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := gzr.Close(); cerr != nil {
			log.Printf("警告: 关闭 gzip reader %s 失败: %v", srcPath, cerr)
		}
	}()

	return writeExpandedLayerWithBudget(ctx, dstPath, gzr, expandedLimit, budget)
}

func decompressZstdFile(srcPath, dstPath string) error {
	return decompressZstdFileWithLimit(context.Background(), srcPath, dstPath, maxLayerTarBytes)
}

func decompressZstdFileWithLimit(ctx context.Context, srcPath, dstPath string, expandedLimit int64) error {
	_, err := decompressZstdFileWithBudget(ctx, srcPath, dstPath, expandedLimit, nil)
	return err
}

func decompressZstdFileWithBudget(ctx context.Context, srcPath, dstPath string, expandedLimit int64, budget *pullResourceBudget) (int64, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := src.Close(); cerr != nil {
			log.Printf("警告: 关闭文件 %s 失败: %v", srcPath, cerr)
		}
	}()
	zr, err := zstd.NewReader(src,
		zstd.WithDecoderMaxMemory(zstdDecoderMemoryLimit(expandedLimit)),
		zstd.WithDecoderConcurrency(1),
	)
	if err != nil {
		return 0, err
	}
	defer zr.Close()

	return writeExpandedLayerWithBudget(ctx, dstPath, zr, expandedLimit, budget)
}

func zstdDecoderMemoryLimit(expandedLimit int64) uint64 {
	if expandedLimit <= 0 {
		return minZstdDecoderMemory
	}
	limit := uint64(expandedLimit)
	if limit < minZstdDecoderMemory {
		return minZstdDecoderMemory
	}
	if limit > maxZstdDecoderMemory {
		return maxZstdDecoderMemory
	}
	return limit
}

func writeExpandedLayerWithLimit(ctx context.Context, dstPath string, src io.Reader, expandedLimit int64) (resultErr error) {
	_, err := writeExpandedLayerWithBudget(ctx, dstPath, src, expandedLimit, nil)
	return err
}

func writeExpandedLayerWithBudget(ctx context.Context, dstPath string, src io.Reader, expandedLimit int64, budget *pullResourceBudget) (written int64, resultErr error) {
	if expandedLimit <= 0 {
		return 0, fmt.Errorf("层展开大小上限必须大于 0")
	}
	partPath := dstPath + ".part"
	_ = os.Remove(partPath)
	dst, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	var reserved int64
	defer func() {
		if resultErr != nil {
			_ = dst.Close()
			_ = os.Remove(partPath)
			budget.release(reserved, reserved)
		}
	}()
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			if int64(n) > expandedLimit-written {
				return written, fmt.Errorf("层展开后大小超过上限 %d", expandedLimit)
			}
			if err := budget.reserve(int64(n), int64(n)); err != nil {
				return written, err
			}
			reserved += int64(n)
			count, writeErr := dst.Write(buffer[:n])
			if writeErr != nil {
				return written, writeErr
			}
			if count != n {
				return written, io.ErrShortWrite
			}
			written += int64(n)
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return written, readErr
		}
	}
	if err := ctx.Err(); err != nil {
		return written, err
	}
	if err := dst.Sync(); err != nil {
		return written, err
	}
	if err := dst.Close(); err != nil {
		return written, err
	}
	_ = os.Remove(dstPath)
	if err := os.Rename(partPath, dstPath); err != nil {
		return written, err
	}
	return written, nil
}

func isGzipLayerMediaType(mediaType string) bool {
	return strings.HasSuffix(mediaType, "+gzip") || strings.Contains(mediaType, ".tar.gzip")
}

func isZstdLayerMediaType(mediaType string) bool {
	return strings.HasSuffix(mediaType, "+zstd") || strings.Contains(mediaType, ".tar.zstd")
}

type httpStatusError struct {
	StatusCode int
	Status     string
	Header     http.Header
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.StatusCode, e.Status)
}

func (r *PullRunner) saveRegistryFileWithRetry(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth, outputPath string, maxBytes int64) (*pullRegistryAuth, error) {
	if maxBytes <= 0 {
		maxBytes = effectivePullResourceLimits(opts.Limits).LayerBytes
	}
	var lastErr error
	currentAuth := auth
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := ctx.Err(); err != nil {
			_ = removePartialDownload(outputPath)
			return currentAuth, err
		}
		nextAuth, err := r.saveRegistryFileOnce(ctx, rawURL, headers, query, info, opts, currentAuth, outputPath, maxBytes)
		if err == nil {
			return nextAuth, nil
		}
		currentAuth = nextAuth
		_ = removePartialDownload(outputPath)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errRegistryResponseTooLarge) {
			return currentAuth, err
		}
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

func (r *PullRunner) saveRegistryFileOnce(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth, outputPath string, maxBytes int64) (*pullRegistryAuth, error) {
	err := r.httpSaveToFileWithStatusLimit(ctx, rawURL, authHeaders(headers, auth), query, outputPath, opts.ProgressOutput, maxBytes)
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
	err = r.httpSaveToFileWithStatusLimit(ctx, rawURL, authHeaders(headers, nextAuth), query, outputPath, opts.ProgressOutput, maxBytes)
	return nextAuth, err
}

func (r *PullRunner) httpSaveToFile(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, outputPath string) error {
	return r.httpSaveToFileWithStatus(ctx, rawURL, headers, query, outputPath, io.Discard)
}

func (r *PullRunner) httpSaveToFileWithStatus(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, outputPath string, progressOutput io.Writer) error {
	return r.httpSaveToFileWithStatusLimit(ctx, rawURL, headers, query, outputPath, progressOutput, 0)
}

func (r *PullRunner) httpSaveToFileWithStatusLimit(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, outputPath string, progressOutput io.Writer, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = defaultMaxLayerBytes
	}
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
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return fmt.Errorf("%w: 最大 %d 字节", errRegistryResponseTooLarge, maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return err
	}

	partPath := partialDownloadPath(outputPath)
	file, err := os.Create(partPath)
	if err != nil {
		return err
	}
	var responseReader io.Reader = resp.Body
	if maxBytes > 0 {
		responseReader = io.LimitReader(resp.Body, maxBytes+1)
	}
	reader := newDownloadProgressReader(responseReader, progressOutput, downloadProgressLabel(rawURL, outputPath), resp.ContentLength)
	written, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(partPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(partPath)
		return closeErr
	}
	if maxBytes > 0 && written > maxBytes {
		_ = os.Remove(partPath)
		return fmt.Errorf("%w: 最大 %d 字节", errRegistryResponseTooLarge, maxBytes)
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
