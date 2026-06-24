package reverse

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		arg  string
		want string
	}{
		{name: "safe", arg: "repo/app:v1", want: "repo/app:v1"},
		{name: "empty", arg: "", want: "''"},
		{name: "space", arg: "my app", want: "'my app'"},
		{name: "dollar", arg: "PASSWORD=p a$$", want: "'PASSWORD=p a$$'"},
		{name: "single quote", arg: "echo 'hello'", want: "'echo '\\''hello'\\'''"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellQuote(tt.arg); got != tt.want {
				t.Fatalf("shellQuote(%q) = %q, want %q", tt.arg, got, tt.want)
			}
		})
	}
}

func TestDockerRunCommandStringRawQuotesArguments(t *testing.T) {
	result := NewReverseResult([]ParsedResult{
		{
			Name: "demo",
			Command: []string{
				"docker", "run", "-d",
				"--name", "my app",
				"-e", "PASSWORD=p a$$",
				"-v", "/host path:/container path:ro",
				CommandSplitMarker,
				"alpine:latest",
				"sh", "-c", "echo 'hello world'",
			},
		},
	}, ReverseOptions{})

	got := result.DockerRunCommandStringRaw()
	want := "# demo\n" +
		"docker run -d --name 'my app' -e 'PASSWORD=p a$$' -v '/host path:/container path:ro' alpine:latest sh -c 'echo '\\''hello world'\\'''\n\n"
	if got != want {
		t.Fatalf("DockerRunCommandStringRaw() =\n%q\nwant\n%q", got, want)
	}
}

func TestDockerRunCommandStringPrettyQuotesArguments(t *testing.T) {
	result := NewReverseResult([]ParsedResult{
		{
			Name: "demo",
			Command: []string{
				"docker", "run", "-d",
				"--name", "my app",
				"-e", "PASSWORD=p a$$",
				"-v", "/host path:/container path:ro",
				CommandSplitMarker,
				"alpine:latest",
				"sh", "-c", "echo 'hello world'",
			},
		},
	}, ReverseOptions{})

	got := result.DockerRunCommandStringPretty()
	wantParts := []string{
		"# demo\n",
		"docker run \\\n",
		"    -d \\\n",
		"    --name='my app' \\\n",
		"    -e 'PASSWORD=p a$$' \\\n",
		"    -v '/host path:/container path:ro' \\\n",
		"    alpine:latest sh -c 'echo '\\''hello world'\\'''\n\n",
	}
	want := strings.Join(wantParts, "")
	if got != want {
		t.Fatalf("DockerRunCommandStringPretty() =\n%q\nwant\n%q", got, want)
	}
}

func TestInspectBackupPathSanitizesContainerName(t *testing.T) {
	backupDir := filepath.Join("docker-inspect-backups", "20260624-123456")
	got := inspectBackupPath(backupDir, "/team app/db:1")
	want := filepath.Join(backupDir, "team_app_db_1.inspect.json")
	if got != want {
		t.Fatalf("inspectBackupPath() = %q, want %q", got, want)
	}
}

func TestInspectBackupDirUsesTimestamp(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 34, 56, 0, time.Local)
	got := inspectBackupDir(now)
	want := filepath.Join(inspectBackupRoot, "20260624-123456")
	if got != want {
		t.Fatalf("inspectBackupDir() = %q, want %q", got, want)
	}
}
