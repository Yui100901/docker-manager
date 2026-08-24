package pull

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Yui100901/MyGo/network/http_utils"
	"github.com/klauspost/compress/zstd"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestBearerTokenResponseLimitRejectsChunkedBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write(bytes.Repeat([]byte("x"), 257))
	}))
	defer server.Close()
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	info := &ImageInfo{Registry: "registry.example", Repository: "team", Image: "app", Tag: "latest"}
	_, err := runner.fetchBearerToken(context.Background(), authChallenge{Params: map[string]string{
		"realm": server.URL + "/token",
	}}, info, pullRegistryCredential{}, PullOptions{
		AuthRealmAllowlist: []string{server.URL},
		Limits:             pullLimitsForTest(256),
	})
	if !errors.Is(err, errRegistryResponseTooLarge) {
		t.Fatalf("fetchBearerToken() error = %v, want response-too-large", err)
	}
}

func TestValidateManifestLayersEnforcesCountPerLayerAndTotal(t *testing.T) {
	descriptor := func(value string, size int64) ocispec.Descriptor {
		return ocispec.Descriptor{Digest: digest.FromString(value), Size: size}
	}
	base := pullLimitsForTest(1024)
	tests := []struct {
		name      string
		layers    []ocispec.Descriptor
		configure func(*PullResourceLimits)
		want      string
	}{
		{
			name:   "max layers",
			layers: []ocispec.Descriptor{descriptor("one", 1), descriptor("two", 1)},
			configure: func(limits *PullResourceLimits) {
				limits.MaxLayers = 1
			},
			want: "层数量",
		},
		{
			name:   "per layer",
			layers: []ocispec.Descriptor{descriptor("one", 5)},
			configure: func(limits *PullResourceLimits) {
				limits.LayerBytes = 4
			},
			want: "镜像层大小",
		},
		{
			name:   "compressed total",
			layers: []ocispec.Descriptor{descriptor("one", 3), descriptor("two", 3)},
			configure: func(limits *PullResourceLimits) {
				limits.TotalLayerBytes = 5
			},
			want: "压缩层总大小",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := base
			tt.configure(&limits)
			err := validateManifestLayers(&ocispec.Manifest{Layers: tt.layers}, limits)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateManifestLayers() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDownloadLayerRejectsActualBodyOverDescriptorAndCleansLayerDir(t *testing.T) {
	body := []byte("four")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write(body)
	}))
	defer server.Close()
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	tempDir := t.TempDir()
	descriptor := ocispec.Descriptor{
		Digest: digest.FromBytes([]byte("abc")),
		Size:   3,
	}
	info := &ImageInfo{Registry: strings.TrimPrefix(server.URL, "http://"), Repository: "team", Image: "app", Tag: "latest"}
	err := runner.downloadLayer(context.Background(), info, descriptor, nil, PullOptions{
		PlainHTTP: true,
		Limits:    pullLimitsForTest(1024),
	}, tempDir)
	if !errors.Is(err, errRegistryResponseTooLarge) {
		t.Fatalf("downloadLayer() error = %v, want response-too-large", err)
	}
	layerDir := filepath.Join(tempDir, sha256Hash(descriptor.Digest.String()))
	if _, statErr := os.Stat(layerDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("layer directory remains after failure: %v", statErr)
	}
}

func TestExpandedLayerLimitsRejectGzipAndZstdBombsAndCleanPartial(t *testing.T) {
	content := bytes.Repeat([]byte("expanded"), 1024)
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			dir := t.TempDir()
			blobPath := filepath.Join(dir, "layer.blob")
			tarPath := filepath.Join(dir, "layer.tar")
			writeCompressedLayerForTest(t, encoding, blobPath, content)
			budget, err := newPullResourceBudget(pullLimitsForTest(1024), 0)
			if err != nil {
				t.Fatal(err)
			}
			mediaType := ocispec.MediaTypeImageLayerGzip
			if encoding == "zstd" {
				mediaType = ocispec.MediaTypeImageLayer + "+zstd"
			}
			_, err = materializeLayerTarWithBudget(context.Background(), blobPath, tarPath, mediaType, 1024, budget)
			if err == nil || !strings.Contains(err.Error(), "展开后大小超过上限") {
				t.Fatalf("materializeLayerTarWithBudget() error = %v, want expanded limit", err)
			}
			for _, path := range []string{tarPath, tarPath + ".part"} {
				if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("partial output remains at %s: %v", path, statErr)
				}
			}
		})
	}
}

func TestExpandedBudgetIsSharedAcrossLayerWorkers(t *testing.T) {
	limits := pullLimitsForTest(1024)
	limits.TotalExpandedBytes = 1024
	budget, err := newPullResourceBudget(limits, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.reserve(800, 800); err != nil {
		t.Fatal(err)
	}
	if err := budget.reserve(300, 300); err == nil || !strings.Contains(err.Error(), "展开总大小") {
		t.Fatalf("second reserve error = %v, want shared expanded budget rejection", err)
	}
	budget.release(800, 800)
	if err := budget.reserve(300, 300); err != nil {
		t.Fatalf("budget was not released: %v", err)
	}
}

func TestExpandedBudgetIsConcurrencySafe(t *testing.T) {
	limits := pullLimitsForTest(1024)
	limits.TotalExpandedBytes = 32
	limits.TemporaryBytes = 32
	budget, err := newPullResourceBudget(limits, 0)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if budget.reserve(1, 1) == nil {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := accepted.Load(); got != 32 {
		t.Fatalf("concurrent accepted reservations = %d, want 32", got)
	}
}

func TestPackageTemporaryBudgetIncludesWorkspaceAndStagedTar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "layer.tar"), bytes.Repeat([]byte("x"), 1024), 0600); err != nil {
		t.Fatal(err)
	}
	err := validatePackageTemporaryBudget(context.Background(), dir, 2048)
	if err == nil || !strings.Contains(err.Error(), "打包临时文件峰值") {
		t.Fatalf("validatePackageTemporaryBudget() error = %v, want peak rejection", err)
	}
}

func TestGetImageTotalTimeoutCancelsSlowManifestBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	limits := pullLimitsForTest(1024)
	limits.TotalTimeout = 40 * time.Millisecond
	started := time.Now()
	err := runner.getImage(strings.TrimPrefix(server.URL, "http://")+"/team/app:latest", PullOptions{
		PlainHTTP: true,
		OutputDir: t.TempDir(),
		Limits:    limits,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("getImage() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("total timeout returned too slowly: %s", elapsed)
	}
}

func TestVerifyFileDigestWithContextReturnsCanceled(t *testing.T) {
	content := bytes.Repeat([]byte("digest"), 1024)
	path := filepath.Join(t.TempDir(), "layer.blob")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := verifyFileDigestWithContext(ctx, path, digest.FromBytes(content))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("verifyFileDigestWithContext() error = %v, want canceled", err)
	}
}

func TestDownloadLayersWaitsForWorkersAfterFirstFailure(t *testing.T) {
	layers := make([]ocispec.Descriptor, maxLayerConcurrency+1)
	for i := range layers {
		layers[i] = ocispec.Descriptor{Digest: digest.FromString(string(rune('a' + i)))}
	}
	barrier := make(chan struct{})
	var closeBarrier sync.Once
	var started atomic.Int32
	var active atomic.Int32
	wantErr := errors.New("worker failed")
	startedAt := time.Now()
	err := downloadLayersWithFunc(context.Background(), layers, func(ctx context.Context, layer ocispec.Descriptor) error {
		active.Add(1)
		defer active.Add(-1)
		if started.Add(1) == maxLayerConcurrency {
			closeBarrier.Do(func() { close(barrier) })
		}
		<-barrier
		if layer.Digest == layers[0].Digest {
			return wantErr
		}
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		return ctx.Err()
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("downloadLayersWithFunc() error = %v, want worker error", err)
	}
	if active.Load() != 0 {
		t.Fatalf("downloadLayersWithFunc() returned with %d active workers", active.Load())
	}
	if elapsed := time.Since(startedAt); elapsed < 40*time.Millisecond {
		t.Fatalf("downloadLayersWithFunc() did not wait for worker cleanup: %s", elapsed)
	}
}

func TestTargetManifestExistsAppliesManifestBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write(bytes.Repeat([]byte("x"), 65))
	}))
	defer server.Close()
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	host := strings.TrimPrefix(server.URL, "http://")
	limits := pullLimitsForTest(64)
	_, err := runner.targetManifestExists(context.Background(), "source/app:latest", host+"/team/app:latest", PullOptions{
		To:     server.URL,
		Limits: limits,
	})
	if !errors.Is(err, errRegistryResponseTooLarge) {
		t.Fatalf("targetManifestExists() error = %v, want response-too-large", err)
	}
}

func TestBatchItemTotalTimeoutIncludesSkipExistingPreflight(t *testing.T) {
	limits := pullLimitsForTest(1024)
	limits.TotalTimeout = 40 * time.Millisecond
	pullCalled := false
	result := runPullBatchItem(context.Background(), "team/app:latest", PullBatchOptions{
		To:           "registry.example/mirror",
		SkipExisting: true,
		Limits:       limits,
	}, pullBatchState{}, func(string, PullOptions) error {
		pullCalled = true
		return nil
	}, func(ctx context.Context, _, _ string, _ PullOptions) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}, nil)
	if pullCalled {
		t.Fatal("pull ran after timed-out skip-existing preflight")
	}
	if !strings.Contains(result.Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("result message = %q, want deadline exceeded", result.Message)
	}
}

func TestConfiguredPullResourceLimitsRejectHardCeilingOverflow(t *testing.T) {
	limits := defaultPullResourceLimits()
	limits.TotalLayerBytes = maxTotalLayerBytes + 1
	if err := validateConfiguredPullResourceLimits(limits); err == nil || !strings.Contains(err.Error(), "--max-total-layer-bytes") {
		t.Fatalf("validateConfiguredPullResourceLimits() error = %v", err)
	}
	limits = defaultPullResourceLimits()
	limits.TotalExpandedBytes = maxTotalExpandedLayerBytes + 1
	if err := validateConfiguredPullResourceLimits(limits); err == nil || !strings.Contains(err.Error(), "--max-total-expanded-bytes") {
		t.Fatalf("validateConfiguredPullResourceLimits() error = %v", err)
	}
	limits = defaultPullResourceLimits()
	limits.TemporaryBytes = maxTemporaryBytes + 1
	if err := validateConfiguredPullResourceLimits(limits); err == nil || !strings.Contains(err.Error(), "--max-temporary-bytes") {
		t.Fatalf("validateConfiguredPullResourceLimits() error = %v", err)
	}
	limits = defaultPullResourceLimits()
	limits.TotalTimeout = maxPullTotalTimeout + time.Second
	if err := validateConfiguredPullResourceLimits(limits); err == nil || !strings.Contains(err.Error(), "--total-timeout") {
		t.Fatalf("validateConfiguredPullResourceLimits() error = %v", err)
	}
}

func TestPullResourceLimitZeroValueCompatibilityAndExplicitZeroRejection(t *testing.T) {
	if err := validatePullResourceLimits(PullResourceLimits{}); err != nil {
		t.Fatalf("zero-value programmatic limits rejected: %v", err)
	}
	limits := defaultPullResourceLimits()
	limits.TotalLayerBytes = 0
	if err := validateConfiguredPullResourceLimits(limits); err == nil || !strings.Contains(err.Error(), "--max-total-layer-bytes") {
		t.Fatalf("explicit zero configured limit error = %v", err)
	}
}

func TestPullCommandExposesAggregateResourceLimitFlags(t *testing.T) {
	cmd := NewPullCommand()
	for _, name := range []string{"max-total-layer-bytes", "max-total-expanded-bytes", "max-temporary-bytes"} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("missing --%s", name)
		}
		if flag.DefValue == "0" {
			t.Fatalf("--%s has unsafe zero default", name)
		}
	}
}

func pullLimitsForTest(maxBytes int64) PullResourceLimits {
	limits := defaultPullResourceLimits()
	limits.TokenBytes = maxBytes
	limits.ManifestBytes = maxBytes
	limits.ConfigBytes = maxBytes
	limits.LayerBytes = maxBytes
	limits.ExpandedLayerBytes = maxBytes
	limits.TotalLayerBytes = maxBytes * 16
	limits.TotalExpandedBytes = maxBytes * 16
	limits.TemporaryBytes = maxBytes * 64
	limits.MaxLayers = 16
	limits.TotalTimeout = time.Second
	return limits
}

func writeCompressedLayerForTest(t *testing.T, encoding, path string, content []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	switch encoding {
	case "gzip":
		writer := gzip.NewWriter(file)
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	case "zstd":
		writer, err := zstd.NewWriter(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown encoding %q", encoding)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
