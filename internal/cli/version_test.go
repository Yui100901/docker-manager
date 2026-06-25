package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCurrentVersionInfoUsesBuildVariables(t *testing.T) {
	restore := setVersionVars("1.2.3", "abc123", "2026-06-25T00:00:00Z")
	defer restore()

	info := currentVersionInfo()
	if info.Version != "1.2.3" || info.Commit != "abc123" || info.BuildDate != "2026-06-25T00:00:00Z" {
		t.Fatalf("VersionInfo = %#v, want injected values", info)
	}
	if info.GoVersion == "" || info.GOOS == "" || info.GOARCH == "" {
		t.Fatalf("VersionInfo runtime fields = %#v, want non-empty", info)
	}
}

func TestPrintVersionInfoText(t *testing.T) {
	var out bytes.Buffer
	printVersionInfo(&out, VersionInfo{
		Version:   "1.2.3",
		Commit:    "abc123",
		BuildDate: "2026-06-25T00:00:00Z",
		GoVersion: "go1.24.1",
		GOOS:      "linux",
		GOARCH:    "amd64",
	})
	got := out.String()
	for _, want := range []string{"dm version", "version: 1.2.3", "commit: abc123", "platform: linux/amd64"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

func TestVersionCommandJSON(t *testing.T) {
	restore := setVersionVars("1.2.3", "abc123", "2026-06-25T00:00:00Z")
	defer restore()

	cmd := newVersionCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got VersionInfo
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v output=%q", err, out.String())
	}
	if got.Version != "1.2.3" || got.Commit != "abc123" || got.BuildDate != "2026-06-25T00:00:00Z" {
		t.Fatalf("VersionInfo = %#v, want injected values", got)
	}
}

func setVersionVars(v, c, d string) func() {
	oldVersion, oldCommit, oldBuildDate := version, commit, buildDate
	version, commit, buildDate = v, c, d
	return func() {
		version, commit, buildDate = oldVersion, oldCommit, oldBuildDate
	}
}
