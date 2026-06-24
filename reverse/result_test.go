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

func TestMergePortRanges(t *testing.T) {
	got := mergePortRanges([]PortBindingSpec{
		{HostPort: 8080, ContPort: 80, Proto: "tcp"},
		{HostPort: 8081, ContPort: 81, Proto: "tcp"},
		{HostPort: 8083, ContPort: 83, Proto: "tcp"},
	})
	want := []string{"8080-8081:80-81/tcp", "8083:83/tcp"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mergePortRanges() = %#v, want %#v", got, want)
	}
}

func TestCommandFormatterIncludesLabels(t *testing.T) {
	cmd := CommandFormatter{}.Format(&ContainerSpec{
		Image:         "busybox:latest",
		ContainerName: "demo",
		Labels: map[string]string{
			"com.example.role": "worker node",
			"owner":            "team-a",
		},
	}, ReverseOptions{})

	got := strings.Join(cmd, " ")
	want := "docker run -d --name demo --label com.example.role=worker node --label owner=team-a --__SPLIT__ busybox:latest"
	if got != want {
		t.Fatalf("CommandFormatter labels = %q, want %q", got, want)
	}
}

func TestCommandFormatterIncludesNetworkResolution(t *testing.T) {
	cmd := CommandFormatter{}.Format(&ContainerSpec{
		Image:         "busybox:latest",
		ContainerName: "demo",
		DNS:           []string{"1.1.1.1", "8.8.8.8"},
		DNSSearch:     []string{"svc.local"},
		ExtraHosts:    []string{"api.local:10.0.0.8"},
	}, ReverseOptions{})

	got := strings.Join(cmd, " ")
	want := "docker run -d --name demo --dns 1.1.1.1 --dns 8.8.8.8 --dns-search svc.local --add-host api.local:10.0.0.8 --__SPLIT__ busybox:latest"
	if got != want {
		t.Fatalf("CommandFormatter network resolution = %q, want %q", got, want)
	}
}

func TestComposeFormatterFormat(t *testing.T) {
	service := ComposeFormatter{}.Format(&ContainerSpec{
		Image:         "busybox:latest",
		ContainerName: "demo",
		Labels: map[string]string{
			"com.example.role": "worker",
		},
		DNS:           []string{"1.1.1.1"},
		DNSSearch:     []string{"svc.local"},
		ExtraHosts:    []string{"api.local:10.0.0.8"},
		RestartPolicy: "on-failure:3",
		Envs:          []string{"GREETING=hello"},
		Mounts:        []string{"/tmp:/host_tmp:ro"},
		PortBindings: []PortBindingSpec{
			{HostPort: 8080, ContPort: 80, Proto: "tcp"},
		},
		Cmd:         []string{"sh", "-c", "sleep 300"},
		NetworkMode: "bridge",
	})

	if service.Image != "busybox:latest" {
		t.Fatalf("Image = %q, want busybox:latest", service.Image)
	}
	if service.Restart != "on-failure" {
		t.Fatalf("Restart = %q, want on-failure", service.Restart)
	}
	if len(service.Ports) != 1 || service.Ports[0] != "8080:80/tcp" {
		t.Fatalf("Ports = %#v, want 8080:80/tcp", service.Ports)
	}
	if len(service.Command) != 3 || service.Command[2] != "sleep 300" {
		t.Fatalf("Command = %#v, want shell command", service.Command)
	}
	if service.Labels["com.example.role"] != "worker" {
		t.Fatalf("Labels = %#v, want com.example.role=worker", service.Labels)
	}
	if len(service.DNS) != 1 || service.DNS[0] != "1.1.1.1" {
		t.Fatalf("DNS = %#v, want 1.1.1.1", service.DNS)
	}
	if len(service.DNSSearch) != 1 || service.DNSSearch[0] != "svc.local" {
		t.Fatalf("DNSSearch = %#v, want svc.local", service.DNSSearch)
	}
	if len(service.ExtraHosts) != 1 || service.ExtraHosts[0] != "api.local:10.0.0.8" {
		t.Fatalf("ExtraHosts = %#v, want api.local:10.0.0.8", service.ExtraHosts)
	}
}
