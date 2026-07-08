package diagnostics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
