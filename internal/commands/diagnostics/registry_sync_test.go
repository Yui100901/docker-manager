package diagnostics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/commands/pull"
)

func TestRegistrySyncTagSelected(t *testing.T) {
	rules := RegistrySyncTagRules{
		Include: []string{"1.2.*", "latest"},
		Exclude: []string{"*-alpine"},
	}
	tests := []struct {
		tag  string
		want bool
	}{
		{tag: "1.2.3", want: true},
		{tag: "latest", want: true},
		{tag: "1.2.3-alpine", want: false},
		{tag: "2.0.0", want: false},
	}
	for _, tt := range tests {
		if got := registrySyncTagSelected(tt.tag, rules); got != tt.want {
			t.Fatalf("registrySyncTagSelected(%q) = %v, want %v", tt.tag, got, tt.want)
		}
	}
}

func TestSelectRegistrySyncTagsKeepLatestSemver(t *testing.T) {
	decisions := selectRegistrySyncTags([]string{"latest", "1.2.0", "1.10.0", "1.0.0", "v2.0.0"}, RegistrySyncTagRules{
		Include:    []string{"*", "latest"},
		KeepLatest: 2,
	})
	var selected []string
	skipped := map[string]string{}
	for _, decision := range decisions {
		if decision.Selected {
			selected = append(selected, decision.Tag)
			continue
		}
		skipped[decision.Tag] = decision.Reason
	}
	if strings.Join(selected, ",") != "v2.0.0,1.10.0" {
		t.Fatalf("selected = %#v, want newest semver tags", selected)
	}
	if skipped["1.2.0"] != "keep_latest" || skipped["latest"] != "keep_latest" {
		t.Fatalf("skipped = %#v, want keep_latest reasons", skipped)
	}
}

func TestSelectRegistrySyncTagsSortAndLimit(t *testing.T) {
	decisions := selectRegistrySyncTags([]string{"b", "c", "a"}, RegistrySyncTagRules{
		Sort:  "name-desc",
		Limit: 2,
	})
	var selected []string
	for _, decision := range decisions {
		if decision.Selected {
			selected = append(selected, decision.Tag)
		}
	}
	if strings.Join(selected, ",") != "c,b" {
		t.Fatalf("selected = %#v, want name desc limited", selected)
	}
}

func TestRegistrySyncApplyFlagOnlyOnRegistryCommand(t *testing.T) {
	registryCmd := NewRegistryCommand()
	syncCmd, _, err := registryCmd.Find([]string{"sync"})
	if err != nil {
		t.Fatalf("Find(sync) error = %v", err)
	}
	if syncCmd == nil || syncCmd.Name() != "sync" {
		t.Fatalf("sync command = %#v, want sync", syncCmd)
	}
	if flag := syncCmd.Flags().Lookup("apply"); flag == nil {
		t.Fatal("dm registry sync missing --apply")
	}
	reportSync := NewRegistrySyncReportCommand()
	if flag := reportSync.Flags().Lookup("apply"); flag != nil {
		t.Fatal("dm report registry-sync should not expose --apply")
	}
}

func TestRunRegistrySyncDryRunPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/team/app/tags/list" {
			t.Fatalf("path = %s, want /v2/team/app/tags/list", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"team/app","tags":["1.0.0","1.1.0","1.1.0-alpine","latest"]}`))
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "sync.yaml")
	host := strings.TrimPrefix(server.URL, "http://")
	data := []byte(`
mirrors:
  - source: http://` + host + `/team/app
    targets:
      - registry.local/team/app
      - http://backup.local/team/app
    tags:
      include: ["1.1.*", "latest"]
      exclude: ["*-alpine"]
    platforms:
      - linux/amd64
`)
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	report, err := runRegistrySync(context.Background(), RegistrySyncOptions{
		Config:      configPath,
		PlainHTTP:   true,
		Timeout:     5 * time.Second,
		DryRun:      true,
		FailOnError: true,
	})
	if err != nil {
		t.Fatalf("runRegistrySync() error = %v", err)
	}
	if report.Summary.TagsListed != 4 {
		t.Fatalf("TagsListed = %d, want 4", report.Summary.TagsListed)
	}
	if report.Summary.Planned != 4 {
		t.Fatalf("Planned = %d, want 4", report.Summary.Planned)
	}
	if report.Summary.Skipped != 2 {
		t.Fatalf("Skipped = %d, want 2", report.Summary.Skipped)
	}
	if report.Summary.Failed != 0 {
		t.Fatalf("Failed = %d, want 0", report.Summary.Failed)
	}
	var foundBackup bool
	for _, item := range report.Items {
		if item.Target == "backup.local/team/app:latest" && item.Status == "planned" {
			foundBackup = true
		}
	}
	if !foundBackup {
		t.Fatalf("items = %#v, want backup target latest planned", report.Items)
	}
}

func TestRunRegistrySyncRecordsMirrorFailure(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sync.yaml")
	if err := os.WriteFile(configPath, []byte(`
mirrors:
  - source: ""
    targets:
      - registry.local/team/app
`), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := runRegistrySync(context.Background(), RegistrySyncOptions{
		Config:  configPath,
		Timeout: time.Second,
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("runRegistrySync() error = %v", err)
	}
	if report.Summary.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", report.Summary.Failed)
	}
	if len(report.Mirrors) != 1 || report.Mirrors[0].Status != "failed" {
		t.Fatalf("Mirrors = %#v, want failed mirror", report.Mirrors)
	}
}

func TestRunRegistrySyncApplyExecutesPlannedItems(t *testing.T) {
	server := registrySyncTagServer(t, []string{"1.0.0", "1.1.0"})
	defer server.Close()
	configPath := writeRegistrySyncConfig(t, server.URL, `
    tags:
      include: ["1.1.*"]
`)

	var calls []registrySyncPullCall
	restore := stubRegistrySyncRunner(t, &fakeRegistrySyncRunner{
		pull: func(imageName string, opts pull.PullOptions) error {
			calls = append(calls, registrySyncPullCall{image: imageName, to: opts.To, outputDir: opts.OutputDir})
			return nil
		},
	})
	defer restore()

	report, err := runRegistrySync(context.Background(), RegistrySyncOptions{
		Config:      configPath,
		PlainHTTP:   true,
		Timeout:     5 * time.Second,
		DryRun:      false,
		OutputDir:   t.TempDir(),
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("runRegistrySync() error = %v", err)
	}
	if report.Summary.Succeeded != 1 || report.Summary.Skipped != 1 || report.Summary.Failed != 0 {
		t.Fatalf("summary = %#v, want one success and one skipped tag", report.Summary)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want one pull call", calls)
	}
	wantSource := strings.TrimPrefix(server.URL, "http://") + "/team/app:1.1.0"
	if calls[0].image != wantSource {
		t.Fatalf("image = %q, want source tag 1.1.0", calls[0].image)
	}
	if calls[0].to != "http://registry.local/team/app:1.1.0" {
		t.Fatalf("to = %q, want tagged http target", calls[0].to)
	}
}

func TestRunRegistrySyncApplyRecordsAttemptDetails(t *testing.T) {
	server := registrySyncTagServer(t, []string{"latest"})
	defer server.Close()
	configPath := writeRegistrySyncConfig(t, server.URL, "")

	attempts := 0
	restore := stubRegistrySyncRunner(t, &fakeRegistrySyncRunner{
		pull: func(imageName string, opts pull.PullOptions) error {
			attempts++
			if attempts == 1 {
				return errors.New("temporary push failure")
			}
			return nil
		},
	})
	defer restore()

	report, err := runRegistrySync(context.Background(), RegistrySyncOptions{
		Config:      configPath,
		PlainHTTP:   true,
		Timeout:     5 * time.Second,
		DryRun:      false,
		OutputDir:   t.TempDir(),
		Concurrency: 1,
		Retries:     1,
	})
	if err != nil {
		t.Fatalf("runRegistrySync() error = %v", err)
	}
	if report.Summary.Succeeded != 1 || report.Items[0].Attempts != 2 {
		t.Fatalf("report = %#v, want success after two attempts", report)
	}
	details := report.Items[0].AttemptDetails
	if len(details) != 2 || details[0].Status != registrySyncStatusFailed || details[1].Status != registrySyncStatusSuccess {
		t.Fatalf("attempt details = %#v, want failed then success", details)
	}
	if !strings.Contains(details[0].Message, "temporary push failure") {
		t.Fatalf("first attempt message = %q, want temporary failure", details[0].Message)
	}
}

func TestRunRegistrySyncApplySkipExisting(t *testing.T) {
	server := registrySyncTagServer(t, []string{"latest"})
	defer server.Close()
	configPath := writeRegistrySyncConfig(t, server.URL, "")

	pullCalled := false
	restore := stubRegistrySyncRunner(t, &fakeRegistrySyncRunner{
		exists: func(ctx context.Context, imageName, target string, opts pull.PullOptions) (bool, error) {
			if target != "registry.local/team/app:latest" {
				t.Fatalf("target = %q, want normalized target", target)
			}
			return true, nil
		},
		pull: func(imageName string, opts pull.PullOptions) error {
			pullCalled = true
			return nil
		},
	})
	defer restore()

	report, err := runRegistrySync(context.Background(), RegistrySyncOptions{
		Config:       configPath,
		PlainHTTP:    true,
		Timeout:      5 * time.Second,
		DryRun:       false,
		OutputDir:    t.TempDir(),
		Concurrency:  1,
		SkipExisting: true,
	})
	if err != nil {
		t.Fatalf("runRegistrySync() error = %v", err)
	}
	if pullCalled {
		t.Fatal("PullImage called even though target exists")
	}
	if report.Summary.Skipped != 1 || report.Items[0].Reason != "target exists" {
		t.Fatalf("report = %#v, want target exists skipped", report)
	}
}

func TestRunRegistrySyncApplyRejectsMultiplePlatforms(t *testing.T) {
	server := registrySyncTagServer(t, []string{"latest"})
	defer server.Close()
	configPath := writeRegistrySyncConfig(t, server.URL, `
    platforms:
      - linux/amd64
      - linux/arm64
`)

	report, err := runRegistrySync(context.Background(), RegistrySyncOptions{
		Config:      configPath,
		PlainHTTP:   true,
		Timeout:     5 * time.Second,
		DryRun:      false,
		OutputDir:   t.TempDir(),
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("runRegistrySync() error = %v", err)
	}
	if report.Summary.Failed != 1 || !strings.Contains(report.Items[0].Reason, "manifest list") {
		t.Fatalf("report = %#v, want manifest list failure", report)
	}
}

type registrySyncPullCall struct {
	image     string
	to        string
	outputDir string
}

type fakeRegistrySyncRunner struct {
	pull   func(imageName string, opts pull.PullOptions) error
	exists func(ctx context.Context, imageName, target string, opts pull.PullOptions) (bool, error)
}

func (f *fakeRegistrySyncRunner) PullImage(imageName string, opts pull.PullOptions) error {
	if f.pull != nil {
		return f.pull(imageName, opts)
	}
	return nil
}

func (f *fakeRegistrySyncRunner) TargetManifestExists(ctx context.Context, imageName, target string, opts pull.PullOptions) (bool, error) {
	if f.exists != nil {
		return f.exists(ctx, imageName, target, opts)
	}
	return false, nil
}

func stubRegistrySyncRunner(t *testing.T, runner registrySyncPullRunner) func() {
	t.Helper()
	original := newRegistrySyncPullRunner
	newRegistrySyncPullRunner = func(proxy, targetOS, arch string, timeout time.Duration) (registrySyncPullRunner, error) {
		if runner == nil {
			return nil, errors.New("missing fake runner")
		}
		return runner, nil
	}
	return func() { newRegistrySyncPullRunner = original }
}

func registrySyncTagServer(t *testing.T, tags []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/team/app/tags/list" {
			t.Fatalf("path = %s, want /v2/team/app/tags/list", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"name":"team/app","tags":[`)
		for i, tag := range tags {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			_, _ = io.WriteString(w, `"`+tag+`"`)
		}
		_, _ = io.WriteString(w, `]}`)
	}))
}

func writeRegistrySyncConfig(t *testing.T, sourceURL string, extra string) string {
	t.Helper()
	host := strings.TrimPrefix(sourceURL, "http://")
	configPath := filepath.Join(t.TempDir(), "sync.yaml")
	data := []byte(`
mirrors:
  - source: http://` + host + `/team/app
    targets:
      - http://registry.local/team/app
` + extra)
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	return configPath
}
