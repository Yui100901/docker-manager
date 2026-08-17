package pull

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Yui100901/MyGo/network/http_utils"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestConfigBlobFileName(t *testing.T) {
	validDigest := digest.FromBytes([]byte("config"))
	validDescriptor := ocispec.Descriptor{Digest: validDigest, Size: 6}
	got, err := configBlobFileName(validDescriptor)
	if err != nil {
		t.Fatalf("configBlobFileName() error = %v", err)
	}
	if want := validDigest.Encoded() + ".json"; got != want {
		t.Fatalf("configBlobFileName() = %q, want %q", got, want)
	}

	invalidDigests := []string{
		"",
		"sha:" + strings.Repeat("a", 64),
		"sha256:abc",
		"sha256:" + strings.Repeat("A", 64),
		"sha512:" + strings.Repeat("a", 128),
		"sha256:../outside",
		`sha256:..\outside`,
		"sha256:/tmp/outside",
		`sha256:C:\outside`,
		`sha256:\\server\share\outside`,
	}
	for _, value := range invalidDigests {
		t.Run(value, func(t *testing.T) {
			_, err := configBlobFileName(ocispec.Descriptor{Digest: digest.Digest(value), Size: 1})
			if err == nil {
				t.Fatal("configBlobFileName() error = nil, want invalid digest error")
			}
		})
	}

	for _, size := range []int64{-1, maxConfigBlobSize + 1} {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			_, err := configBlobFileName(ocispec.Descriptor{Digest: validDigest, Size: size})
			if err == nil {
				t.Fatal("configBlobFileName() error = nil, want invalid size error")
			}
		})
	}
}

func TestCreateManifestFileUsesValidatedConfigName(t *testing.T) {
	descriptor := ocispec.Descriptor{Digest: digest.FromBytes([]byte("{}")), Size: 2}
	dir := t.TempDir()
	manifest := &ocispec.Manifest{Config: descriptor}
	info := &ImageInfo{Repository: "team", Image: "app", Tag: "v1"}

	if err := createManifestFile(info, manifest, dir); err != nil {
		t.Fatalf("createManifestFile() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var saved []ImageManifest
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(saved) != 1 || saved[0].Config != descriptor.Digest.Encoded()+".json" {
		t.Fatalf("saved manifest = %#v, want validated config filename", saved)
	}

	badDir := t.TempDir()
	manifest.Config.Digest = digest.Digest("sha256:../../outside")
	if err := createManifestFile(info, manifest, badDir); err == nil {
		t.Fatal("createManifestFile() error = nil, want invalid digest error")
	}
	if _, err := os.Stat(filepath.Join(badDir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest.json exists after rejected digest or stat failed: %v", err)
	}
}

func TestDownloadConfigVerifiesDescriptor(t *testing.T) {
	body := []byte(`{"architecture":"amd64"}`)
	validDigest := digest.FromBytes(body)
	tests := []struct {
		name          string
		descriptor    ocispec.Descriptor
		wantErr       bool
		wantRequests  int32
		wantDigestErr bool
		preserveFile  bool
	}{
		{
			name:         "valid",
			descriptor:   ocispec.Descriptor{Digest: validDigest, Size: int64(len(body))},
			wantRequests: 1,
		},
		{
			name:          "digest mismatch",
			descriptor:    ocispec.Descriptor{Digest: digest.FromBytes([]byte("other")), Size: int64(len(body))},
			wantErr:       true,
			wantRequests:  1,
			wantDigestErr: true,
			preserveFile:  true,
		},
		{
			name:         "size smaller",
			descriptor:   ocispec.Descriptor{Digest: validDigest, Size: int64(len(body) - 1)},
			wantErr:      true,
			wantRequests: 1,
			preserveFile: true,
		},
		{
			name:         "size larger",
			descriptor:   ocispec.Descriptor{Digest: validDigest, Size: int64(len(body) + 1)},
			wantErr:      true,
			wantRequests: 1,
			preserveFile: true,
		},
		{
			name:       "negative size",
			descriptor: ocispec.Descriptor{Digest: validDigest, Size: -1},
			wantErr:    true,
		},
		{
			name:       "declared size exceeds limit",
			descriptor: ocispec.Descriptor{Digest: validDigest, Size: maxConfigBlobSize + 1},
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			wantBlobPath := "/v2/team/app/blobs/" + tt.descriptor.Digest.String()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != wantBlobPath {
					t.Errorf("config blob path = %q, want %q", r.URL.Path, wantBlobPath)
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()

			runner := newTestPullRunner()
			runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
			info := testRegistryImageInfo(server.URL)
			dir := t.TempDir()
			manifest := &ocispec.Manifest{Config: tt.descriptor}
			name, nameErr := configBlobFileName(tt.descriptor)
			if tt.preserveFile && nameErr == nil {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("preserve-me"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := runner.downloadConfig(context.Background(), info, manifest, nil, PullOptions{PlainHTTP: true}, dir)
			if tt.wantErr && err == nil {
				t.Fatal("downloadConfig() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("downloadConfig() error = %v", err)
			}
			if tt.wantDigestErr && (err == nil || !strings.Contains(err.Error(), "digest 校验失败")) {
				t.Fatalf("downloadConfig() error = %v, want digest mismatch", err)
			}
			if got := requests.Load(); got != tt.wantRequests {
				t.Fatalf("HTTP requests = %d, want %d", got, tt.wantRequests)
			}

			if tt.wantErr {
				if nameErr == nil {
					path := filepath.Join(dir, name)
					if tt.preserveFile {
						data, readErr := os.ReadFile(path)
						if readErr != nil || string(data) != "preserve-me" {
							t.Fatalf("existing config changed: data=%q err=%v", data, readErr)
						}
					} else if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("config file exists after failure or stat failed: %v", statErr)
					}
					if _, statErr := os.Stat(partialDownloadPath(path)); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("partial config file exists after failure or stat failed: %v", statErr)
					}
				}
				return
			}
			data, readErr := os.ReadFile(filepath.Join(dir, name))
			if readErr != nil {
				t.Fatalf("ReadFile() error = %v", readErr)
			}
			if !bytes.Equal(data, body) {
				t.Fatalf("config content = %q, want %q", data, body)
			}
		})
	}
}

func TestDownloadConfigRejectsActualBodyOverLimit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), int(maxConfigBlobSize)+1)
	descriptor := ocispec.Descriptor{Digest: digest.FromBytes([]byte("x")), Size: 1}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	dir := t.TempDir()
	err := runner.downloadConfig(
		context.Background(),
		testRegistryImageInfo(server.URL),
		&ocispec.Manifest{Config: descriptor},
		nil,
		PullOptions{PlainHTTP: true},
		dir,
	)
	if !errors.Is(err, errRegistryResponseTooLarge) {
		t.Fatalf("downloadConfig() error = %v, want response-too-large error", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1 without retry", got)
	}
	name, nameErr := configBlobFileName(descriptor)
	if nameErr != nil {
		t.Fatal(nameErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, name)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("config file exists after oversized response or stat failed: %v", statErr)
	}
}

func TestDownloadConfigTraversalCannotOverwriteOutsideFile(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "workspace")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	sentinelPath := filepath.Join(parent, "outside.json")
	if err := os.WriteFile(sentinelPath, []byte("preserve-me"), 0600); err != nil {
		t.Fatal(err)
	}

	maliciousDigests := []string{
		"sha256:../outside",
		`sha256:..\outside`,
		"sha256:/outside",
		`sha256:C:\outside`,
	}
	for _, value := range maliciousDigests {
		t.Run(value, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = w.Write([]byte("attacker-data"))
			}))
			defer server.Close()

			runner := newTestPullRunner()
			runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
			descriptor := ocispec.Descriptor{Digest: digest.Digest(value), Size: 13}
			err := runner.downloadConfig(
				context.Background(),
				testRegistryImageInfo(server.URL),
				&ocispec.Manifest{Config: descriptor},
				nil,
				PullOptions{PlainHTTP: true},
				dir,
			)
			if err == nil {
				t.Fatal("downloadConfig() error = nil, want invalid digest error")
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("HTTP requests = %d, want 0", got)
			}
			data, readErr := os.ReadFile(sentinelPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(data) != "preserve-me" {
				t.Fatalf("outside file = %q, want preserved content", data)
			}
		})
	}
}

func TestDownloadConfigDoesNotOverwriteExistingFileOrSymlink(t *testing.T) {
	body := []byte(`{"architecture":"amd64"}`)
	descriptor := ocispec.Descriptor{Digest: digest.FromBytes(body), Size: int64(len(body))}
	name, err := configBlobFileName(descriptor)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("regular file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("preserve-me"), 0600); err != nil {
			t.Fatal(err)
		}
		runner, info, closeServer := configDownloadTestRunner(t, body)
		defer closeServer()
		err := runner.downloadConfig(context.Background(), info, &ocispec.Manifest{Config: descriptor}, nil, PullOptions{PlainHTTP: true}, dir)
		if err == nil {
			t.Fatal("downloadConfig() error = nil, want exclusive-create error")
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || string(data) != "preserve-me" {
			t.Fatalf("existing file changed: data=%q err=%v", data, readErr)
		}
	})

	t.Run("symlink outside root", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "workspace")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		sentinelPath := filepath.Join(parent, "sentinel.json")
		if err := os.WriteFile(sentinelPath, []byte("preserve-me"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sentinelPath, filepath.Join(dir, name)); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}

		runner, info, closeServer := configDownloadTestRunner(t, body)
		defer closeServer()
		err := runner.downloadConfig(context.Background(), info, &ocispec.Manifest{Config: descriptor}, nil, PullOptions{PlainHTTP: true}, dir)
		if err == nil {
			t.Fatal("downloadConfig() error = nil, want symlink rejection")
		}
		data, readErr := os.ReadFile(sentinelPath)
		if readErr != nil || string(data) != "preserve-me" {
			t.Fatalf("symlink target changed: data=%q err=%v", data, readErr)
		}
	})
}

func TestWriteFileWithinRootRejectsSymlinkRoot(t *testing.T) {
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	if err := os.Mkdir(realRoot, 0700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(parent, "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := writeFileWithinRoot(linkRoot, "config.json", []byte("data"), 0600); err == nil {
		t.Fatal("writeFileWithinRoot() error = nil, want symlink-root rejection")
	}
	if _, err := os.Stat(filepath.Join(realRoot, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file was written through symlink root or stat failed: %v", err)
	}
}

func TestFetchManifestVerifiesDigestReference(t *testing.T) {
	manifestBody := testManifestBytes(t, []byte("{}"))
	tests := []struct {
		name         string
		reference    string
		responseBody []byte
		wantErr      bool
		wantRequests int32
	}{
		{
			name:         "matching raw bytes",
			reference:    digest.FromBytes(manifestBody).String(),
			responseBody: manifestBody,
			wantRequests: 1,
		},
		{
			name:         "mismatched raw bytes",
			reference:    digest.FromBytes([]byte("different manifest")).String(),
			responseBody: manifestBody,
			wantErr:      true,
			wantRequests: 1,
		},
		{
			name:         "invalid reference rejected before request",
			reference:    "sha256:../manifest",
			responseBody: manifestBody,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				wantPath := "/v2/team/app/manifests/" + tt.reference
				if r.URL.Path != wantPath {
					t.Errorf("manifest path = %q, want %q", r.URL.Path, wantPath)
				}
				w.Header().Set("Docker-Content-Digest", tt.reference)
				_, _ = w.Write(tt.responseBody)
			}))
			defer server.Close()

			runner := newTestPullRunner()
			runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
			info := testRegistryImageInfo(server.URL)
			info.Digest = tt.reference
			_, _, err := runner.fetchManifest(context.Background(), info, PullOptions{PlainHTTP: true})
			if tt.wantErr && err == nil {
				t.Fatal("fetchManifest() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("fetchManifest() error = %v", err)
			}
			if got := requests.Load(); got != tt.wantRequests {
				t.Fatalf("HTTP requests = %d, want %d", got, tt.wantRequests)
			}
		})
	}
}

func TestFetchManifestVerifiesSelectedIndexManifest(t *testing.T) {
	childBody := testManifestBytes(t, []byte("{}"))
	validDescriptor := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(childBody),
		Size:      int64(len(childBody)),
		Platform:  &ocispec.Platform{OS: "linux", Architecture: "amd64"},
	}
	tests := []struct {
		name         string
		descriptor   ocispec.Descriptor
		wantErr      bool
		wantRequests int32
	}{
		{name: "valid", descriptor: validDescriptor, wantRequests: 2},
		{
			name: "digest mismatch",
			descriptor: func() ocispec.Descriptor {
				d := validDescriptor
				d.Digest = digest.FromBytes([]byte("other manifest"))
				return d
			}(),
			wantErr:      true,
			wantRequests: 2,
		},
		{
			name: "size mismatch",
			descriptor: func() ocispec.Descriptor {
				d := validDescriptor
				d.Size++
				return d
			}(),
			wantErr:      true,
			wantRequests: 2,
		},
		{
			name: "invalid digest rejected before child request",
			descriptor: func() ocispec.Descriptor {
				d := validDescriptor
				d.Digest = digest.Digest("sha256:../manifest")
				return d
			}(),
			wantErr:      true,
			wantRequests: 1,
		},
		{
			name: "declared child size exceeds limit",
			descriptor: func() ocispec.Descriptor {
				d := validDescriptor
				d.Size = maxManifestBlobSize + 1
				return d
			}(),
			wantErr:      true,
			wantRequests: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			indexBody := testIndexBytes(t, tt.descriptor)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := requests.Add(1)
				if call == 1 {
					if r.URL.Path != "/v2/team/app/manifests/v1" {
						t.Errorf("index path = %q, want /v2/team/app/manifests/v1", r.URL.Path)
					}
					_, _ = w.Write(indexBody)
					return
				}
				wantPath := "/v2/team/app/manifests/" + tt.descriptor.Digest.String()
				if r.URL.Path != wantPath {
					t.Errorf("child manifest path = %q, want %q", r.URL.Path, wantPath)
				}
				_, _ = w.Write(childBody)
			}))
			defer server.Close()

			runner := newTestPullRunner()
			runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
			_, _, err := runner.fetchManifest(context.Background(), testRegistryImageInfo(server.URL), PullOptions{PlainHTTP: true})
			if tt.wantErr && err == nil {
				t.Fatal("fetchManifest() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("fetchManifest() error = %v", err)
			}
			if got := requests.Load(); got != tt.wantRequests {
				t.Fatalf("HTTP requests = %d, want %d", got, tt.wantRequests)
			}
		})
	}
}

func TestFetchManifestRejectsOversizedResponse(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", maxManifestBlobSize+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	_, _, err := runner.fetchManifest(context.Background(), testRegistryImageInfo(server.URL), PullOptions{PlainHTTP: true})
	if !errors.Is(err, errRegistryResponseTooLarge) {
		t.Fatalf("fetchManifest() error = %v, want response-too-large error", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
}

func TestFetchManifestVerifiesDigestReferencedIndex(t *testing.T) {
	childBody := testManifestBytes(t, []byte("{}"))
	childDescriptor := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(childBody),
		Size:      int64(len(childBody)),
		Platform:  &ocispec.Platform{OS: "linux", Architecture: "amd64"},
	}
	indexBody := testIndexBytes(t, childDescriptor)

	for _, tt := range []struct {
		name      string
		reference string
		wantErr   bool
		calls     int32
	}{
		{name: "matching index", reference: digest.FromBytes(indexBody).String(), calls: 2},
		{name: "mismatched index", reference: digest.FromBytes([]byte("other index")).String(), wantErr: true, calls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					wantPath := "/v2/team/app/manifests/" + tt.reference
					if r.URL.Path != wantPath {
						t.Errorf("index path = %q, want %q", r.URL.Path, wantPath)
					}
					_, _ = w.Write(indexBody)
					return
				}
				wantPath := "/v2/team/app/manifests/" + childDescriptor.Digest.String()
				if r.URL.Path != wantPath {
					t.Errorf("child manifest path = %q, want %q", r.URL.Path, wantPath)
				}
				_, _ = w.Write(childBody)
			}))
			defer server.Close()

			runner := newTestPullRunner()
			runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
			info := testRegistryImageInfo(server.URL)
			info.Digest = tt.reference
			_, _, err := runner.fetchManifest(context.Background(), info, PullOptions{PlainHTTP: true})
			if tt.wantErr && err == nil {
				t.Fatal("fetchManifest() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("fetchManifest() error = %v", err)
			}
			if got := requests.Load(); got != tt.calls {
				t.Fatalf("HTTP requests = %d, want %d", got, tt.calls)
			}
		})
	}
}

func TestGetImagePackagesVerifiedConfig(t *testing.T) {
	configBody := []byte(`{"architecture":"amd64","os":"linux"}`)
	manifestBody := testManifestBytes(t, configBody)
	configDigest := digest.FromBytes(configBody)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/v1":
			_, _ = w.Write(manifestBody)
		case "/v2/team/app/blobs/" + configDigest.String():
			_, _ = w.Write(configBody)
		default:
			t.Errorf("unexpected registry path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	outputPath := filepath.Join(t.TempDir(), "image.tar")
	imageName := strings.TrimPrefix(server.URL, "http://") + "/team/app:v1"
	if err := runner.getImage(imageName, PullOptions{Output: outputPath, PlainHTTP: true}); err != nil {
		t.Fatalf("getImage() error = %v", err)
	}

	files := readTarFiles(t, outputPath)
	configName := configDigest.Encoded() + ".json"
	if !bytes.Equal(files[configName], configBody) {
		t.Fatalf("archived config = %q, want %q", files[configName], configBody)
	}
	var manifests []ImageManifest
	if err := json.Unmarshal(files["manifest.json"], &manifests); err != nil {
		t.Fatalf("manifest.json decode error = %v", err)
	}
	if len(manifests) != 1 || manifests[0].Config != configName {
		t.Fatalf("manifest.json = %#v, want config %q", manifests, configName)
	}
}

func configDownloadTestRunner(t *testing.T, body []byte) (*PullRunner, *ImageInfo, func()) {
	t.Helper()
	wantPath := "/v2/team/app/blobs/" + digest.FromBytes(body).String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			t.Errorf("config blob path = %q, want %q", r.URL.Path, wantPath)
		}
		_, _ = w.Write(body)
	}))
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	return runner, testRegistryImageInfo(server.URL), server.Close
}

func testRegistryImageInfo(serverURL string) *ImageInfo {
	return &ImageInfo{
		Registry:   strings.TrimPrefix(serverURL, "http://"),
		Repository: "team",
		Image:      "app",
		Tag:        "v1",
	}
}

func testManifestBytes(t *testing.T, configBody []byte) []byte {
	t.Helper()
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    digest.FromBytes(configBody),
			Size:      int64(len(configBody)),
		},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func testIndexBytes(t *testing.T, descriptor ocispec.Descriptor) []byte {
	t.Helper()
	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{descriptor},
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func readTarFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	files := make(map[string][]byte)
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = data
	}
	return files
}
