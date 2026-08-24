package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"docker-manager/internal/docker"

	"github.com/spf13/cobra"
)

func TestNewCommandGeneratesSupportedShellsAndRejectsInvalidInput(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			root := &cobra.Command{Use: "dm"}
			completionCommand := NewCommand()
			root.AddCommand(completionCommand)
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs([]string{"completion", shell})
			if err := root.Execute(); err != nil {
				t.Fatalf("completion %s error = %v", shell, err)
			}
			if output.Len() < 20 {
				t.Fatalf("completion %s output is unexpectedly short: %q", shell, output.String())
			}
		})
	}

	root := &cobra.Command{Use: "dm"}
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.AddCommand(NewCommand())
	root.SetArgs([]string{"completion", "tcsh"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "不支持的 shell") {
		t.Fatalf("completion tcsh error = %v", err)
	}

	root = &cobra.Command{Use: "dm"}
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.AddCommand(NewCommand())
	root.SetArgs([]string{"completion"})
	if err := root.Execute(); err == nil {
		t.Fatal("completion without shell error = nil")
	}
}

func TestDockerBackedCompletionUsesRealSDKAgainstFakeAPI(t *testing.T) {
	var containerRequests atomic.Int32
	var imageRequests atomic.Int32
	var volumeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.49")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && path == "/containers/json":
			containerRequests.Add(1)
			if all := r.URL.Query().Get("all"); all != "1" {
				t.Errorf("container list all = %q, want 1", all)
			}
			_, _ = w.Write([]byte(`[
				{"Id":"abcdef1234567890","Names":["/worker"]},
				{"Id":"1234567890abcdef","Names":["/api"]},
				{"Id":"","Names":[]}
			]`))
		case r.Method == http.MethodGet && path == "/images/json":
			imageRequests.Add(1)
			if all := r.URL.Query().Get("all"); all != "1" {
				t.Errorf("image list all = %q, want 1", all)
			}
			_, _ = w.Write([]byte(`[
				{"Id":"sha256:abcdef1234567890","RepoTags":["repo/app:v1","repo/app:v1","<none>:<none>"],"RepoDigests":["repo/app@sha256:deadbeef","<none>@<none>"]},
				{"Id":"short-id","RepoTags":[],"RepoDigests":[]}
			]`))
		case r.Method == http.MethodGet && path == "/volumes":
			volumeRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Volumes":  []map[string]string{{"Name": "zeta"}, {"Name": "alpha"}, {"Name": "alpha"}, {"Name": ""}},
				"Warnings": []string{},
			})
		default:
			t.Errorf("unexpected Docker API request: %s %s", r.Method, r.URL.String())
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	t.Cleanup(func() { docker.Configure(docker.Options{}) })

	root := completionTestRootForServer(t, server)
	containers, directive := LocalContainers(root, nil, "a")
	if directive != cobra.ShellCompDirectiveNoFileComp || !reflect.DeepEqual(containers, []string{"abcdef123456", "api"}) {
		t.Fatalf("LocalContainers() = %#v, %v", containers, directive)
	}

	images, directive := LocalImages(root, nil, "repo/")
	if directive != cobra.ShellCompDirectiveNoFileComp || !reflect.DeepEqual(images, []string{"repo/app:v1", "repo/app@sha256:deadbeef"}) {
		t.Fatalf("LocalImages() = %#v, %v", images, directive)
	}

	volumes, directive := LocalVolumes(root, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp || !reflect.DeepEqual(volumes, []string{"alpha", "zeta"}) {
		t.Fatalf("LocalVolumes() = %#v, %v", volumes, directive)
	}
	if containerRequests.Load() != 1 || imageRequests.Load() != 1 || volumeRequests.Load() != 1 {
		t.Fatalf("Docker API request counts = containers:%d images:%d volumes:%d", containerRequests.Load(), imageRequests.Load(), volumeRequests.Load())
	}
}

func TestDockerBackedCompletionReturnsErrorDirective(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"message":"daemon unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	root := completionTestRootForServer(t, server)

	for _, complete := range []struct {
		name string
		fn   cobra.CompletionFunc
	}{
		{name: "containers", fn: LocalContainers},
		{name: "images", fn: LocalImages},
		{name: "volumes", fn: LocalVolumes},
	} {
		t.Run(complete.name, func(t *testing.T) {
			values, directive := complete.fn(root, nil, "")
			if values != nil || directive != cobra.ShellCompDirectiveError {
				t.Fatalf("completion error result = %#v, %v", values, directive)
			}
		})
	}
}

func TestPrepareDockerCompletionErrorsAndNilCommand(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	if err := prepareDockerCompletion(nil); err != nil {
		t.Fatalf("prepareDockerCompletion(nil) error = %v", err)
	}

	root := newCompletionTestRoot()
	invalidConfig := t.TempDir() + "/invalid.yaml"
	if err := root.PersistentFlags().Set("config", invalidConfig); err != nil {
		t.Fatal(err)
	}
	if err := root.PersistentFlags().Set("docker-host", "http://unsupported.example"); err != nil {
		t.Fatal(err)
	}
	if err := prepareDockerCompletion(root); err != nil {
		t.Fatalf("prepareDockerCompletion() configures options lazily, got %v", err)
	}
	if _, err := docker.NewMobyClient(); err == nil || !strings.Contains(err.Error(), "unsupported Docker host scheme") {
		t.Fatalf("NewMobyClient() error = %v, want unsupported host", err)
	}
}

func TestCompletionValueHelpersCoverEmptyTrimAndIDEdges(t *testing.T) {
	if got := uniqueSorted([]string{" beta ", "", "alpha", "beta", " alpha "}); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("uniqueSorted() = %#v", got)
	}
	if got := filterCompletionValues([]string{"beta", "alpha"}, "missing"); got != nil {
		t.Fatalf("filterCompletionValues(no match) = %#v, want nil", got)
	}
	if got := firstContainerName(nil); got != "" {
		t.Fatalf("firstContainerName(nil) = %q", got)
	}
	if got := firstContainerName([]string{"/api", "/alias"}); got != "api" {
		t.Fatalf("firstContainerName() = %q, want api", got)
	}
	for input, want := range map[string]string{
		"sha256:abcdef1234567890": "abcdef123456",
		"abcdef":                  "abcdef",
		"sha256:":                 "",
	} {
		if got := shortID(input); got != want {
			t.Errorf("shortID(%q) = %q, want %q", input, got, want)
		}
	}
	todoContext := context.TODO()
	if got := ctxOrBackground(todoContext); got != todoContext {
		t.Fatal("ctxOrBackground(TODO) did not preserve context")
	}
	wantContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	if got := ctxOrBackground(wantContext); got != wantContext {
		t.Fatal("ctxOrBackground(non-nil) did not preserve context")
	}
}

func completionTestRootForServer(t *testing.T, server *httptest.Server) *cobra.Command {
	t.Helper()
	root := newCompletionTestRoot()
	configPath := t.TempDir() + "/missing.yaml"
	host := "tcp://" + strings.TrimPrefix(server.URL, "http://")
	for name, value := range map[string]string{
		"config":             configPath,
		"docker-host":        host,
		"docker-api-version": "1.49",
	} {
		if err := root.PersistentFlags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	return root
}
