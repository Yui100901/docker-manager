package reverse

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	units "github.com/docker/go-units"
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

func TestCommandFormatterIncludesSecurityOptions(t *testing.T) {
	cmd := CommandFormatter{}.Format(&ContainerSpec{
		Image:         "busybox:latest",
		ContainerName: "demo",
		Privileged:    true,
		CapAdd:        []string{"NET_ADMIN", "SYS_TIME"},
		CapDrop:       []string{"MKNOD"},
		SecurityOpt:   []string{"no-new-privileges:true"},
	}, ReverseOptions{})

	got := strings.Join(cmd, " ")
	want := "docker run -d --name demo --privileged --cap-add NET_ADMIN --cap-add SYS_TIME --cap-drop MKNOD --security-opt no-new-privileges:true --__SPLIT__ busybox:latest"
	if got != want {
		t.Fatalf("CommandFormatter security options = %q, want %q", got, want)
	}
}

func TestCommandFormatterIncludesDevicesAndUlimits(t *testing.T) {
	cmd := CommandFormatter{}.Format(&ContainerSpec{
		Image:         "busybox:latest",
		ContainerName: "demo",
		Devices:       []string{"/dev/fuse:/dev/fuse:rwm"},
		Ulimits: map[string]UlimitSpec{
			"nofile": {Soft: 1024, Hard: 2048},
			"nproc":  {Soft: 256, Hard: 512},
		},
	}, ReverseOptions{})

	got := strings.Join(cmd, " ")
	want := "docker run -d --name demo --device /dev/fuse:/dev/fuse:rwm --ulimit nofile=1024:2048 --ulimit nproc=256:512 --__SPLIT__ busybox:latest"
	if got != want {
		t.Fatalf("CommandFormatter devices and ulimits = %q, want %q", got, want)
	}
}

func TestParserToSpecIncludesDeviceUlimitAndLoggingFields(t *testing.T) {
	spec := NewParser(container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name: "/demo",
			HostConfig: &container.HostConfig{
				LogConfig: container.LogConfig{
					Type: "json-file",
					Config: map[string]string{
						"max-file": "3",
						"max-size": "10m",
					},
				},
				Resources: container.Resources{
					Devices: []container.DeviceMapping{
						{
							PathOnHost:        "/dev/fuse",
							PathInContainer:   "/dev/fuse",
							CgroupPermissions: "rwm",
						},
					},
					Ulimits: []*units.Ulimit{
						{Name: "nofile", Soft: 1024, Hard: 2048},
					},
				},
			},
		},
		Config: &container.Config{
			Image: "busybox:latest",
		},
	}, ReverseOptions{}).ToSpec()

	if len(spec.Devices) != 1 || spec.Devices[0] != "/dev/fuse:/dev/fuse:rwm" {
		t.Fatalf("Devices = %#v, want /dev/fuse:/dev/fuse:rwm", spec.Devices)
	}
	if spec.Ulimits["nofile"].Soft != 1024 || spec.Ulimits["nofile"].Hard != 2048 {
		t.Fatalf("Ulimits = %#v, want nofile soft=1024 hard=2048", spec.Ulimits)
	}
	if spec.LogDriver != "json-file" {
		t.Fatalf("LogDriver = %q, want json-file", spec.LogDriver)
	}
	if spec.LogOptions["max-size"] != "10m" || spec.LogOptions["max-file"] != "3" {
		t.Fatalf("LogOptions = %#v, want max-size=10m and max-file=3", spec.LogOptions)
	}
}

func TestParserUsesRuntimePortWhenConfiguredHostPortIsEmpty(t *testing.T) {
	var logs bytes.Buffer
	oldLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldLogOutput)

	result := NewParser(container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name: "/demo",
			HostConfig: &container.HostConfig{
				PortBindings: nat.PortMap{
					"80/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: ""}},
				},
			},
		},
		Config: &container.Config{
			Image: "busybox:latest",
		},
		NetworkSettings: &container.NetworkSettings{
			NetworkSettingsBase: container.NetworkSettingsBase{
				Ports: nat.PortMap{
					"80/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "32768"}},
				},
			},
		},
	}, ReverseOptions{}).ToResult()

	run := strings.Join(result.Command, " ")
	if !strings.Contains(run, "-p 127.0.0.1:32768:80/tcp") {
		t.Fatalf("Command = %q, want runtime port mapping", run)
	}
	if len(result.Compose.Ports) != 1 || result.Compose.Ports[0] != "127.0.0.1:32768:80/tcp" {
		t.Fatalf("Compose ports = %#v, want runtime port mapping", result.Compose.Ports)
	}
	if strings.Contains(logs.String(), "解析主机端口失败") {
		t.Fatalf("unexpected host port warning: %s", logs.String())
	}
}

func TestParserRedactsEnvAndLabelsWhenRequested(t *testing.T) {
	result := NewParser(container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{
			Image: "busybox:latest",
			Env:   []string{"PASSWORD=secret", "MODE=prod"},
			Labels: map[string]string{
				"api_token": "token-value",
				"owner":     "team-a",
			},
		},
	}, ReverseOptions{RedactSecrets: true}).ToResult()

	run := strings.Join(result.Command, " ")
	if strings.Contains(run, "secret") || strings.Contains(run, "token-value") {
		t.Fatalf("Command leaked secret: %s", run)
	}
	if !strings.Contains(run, "PASSWORD=<redacted>") || !strings.Contains(run, "MODE=prod") {
		t.Fatalf("Command = %s, want redacted password and visible non-secret env", run)
	}
	if result.Compose.Labels["api_token"] != "<redacted>" || result.Compose.Labels["owner"] != "team-a" {
		t.Fatalf("Compose labels = %#v, want redacted api_token only", result.Compose.Labels)
	}
	if strings.Join(result.Compose.Environment, ",") != "PASSWORD=<redacted>,MODE=prod" {
		t.Fatalf("Compose environment = %#v, want redacted password", result.Compose.Environment)
	}
}

func TestCommandFormatterIncludesLoggingOptions(t *testing.T) {
	cmd := CommandFormatter{}.Format(&ContainerSpec{
		Image:         "busybox:latest",
		ContainerName: "demo",
		LogDriver:     "json-file",
		LogOptions: map[string]string{
			"max-file": "3",
			"max-size": "10m",
		},
	}, ReverseOptions{})

	got := strings.Join(cmd, " ")
	want := "docker run -d --name demo --log-driver json-file --log-opt max-file=3 --log-opt max-size=10m --__SPLIT__ busybox:latest"
	if got != want {
		t.Fatalf("CommandFormatter logging options = %q, want %q", got, want)
	}
}

func TestComposeFormatterFormat(t *testing.T) {
	service := ComposeFormatter{}.Format(&ContainerSpec{
		Image:         "busybox:latest",
		ContainerName: "demo",
		Labels: map[string]string{
			"com.example.role": "worker",
		},
		DNS:         []string{"1.1.1.1"},
		DNSSearch:   []string{"svc.local"},
		ExtraHosts:  []string{"api.local:10.0.0.8"},
		Privileged:  true,
		CapAdd:      []string{"NET_ADMIN"},
		CapDrop:     []string{"MKNOD"},
		SecurityOpt: []string{"no-new-privileges:true"},
		Devices:     []string{"/dev/fuse:/dev/fuse:rwm"},
		Ulimits: map[string]UlimitSpec{
			"nofile": {Soft: 1024, Hard: 2048},
		},
		LogDriver: "json-file",
		LogOptions: map[string]string{
			"max-file": "3",
			"max-size": "10m",
		},
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
	if !service.Privileged {
		t.Fatal("Privileged = false, want true")
	}
	if len(service.CapAdd) != 1 || service.CapAdd[0] != "NET_ADMIN" {
		t.Fatalf("CapAdd = %#v, want NET_ADMIN", service.CapAdd)
	}
	if len(service.CapDrop) != 1 || service.CapDrop[0] != "MKNOD" {
		t.Fatalf("CapDrop = %#v, want MKNOD", service.CapDrop)
	}
	if len(service.SecurityOpt) != 1 || service.SecurityOpt[0] != "no-new-privileges:true" {
		t.Fatalf("SecurityOpt = %#v, want no-new-privileges:true", service.SecurityOpt)
	}
	if len(service.Devices) != 1 || service.Devices[0] != "/dev/fuse:/dev/fuse:rwm" {
		t.Fatalf("Devices = %#v, want /dev/fuse:/dev/fuse:rwm", service.Devices)
	}
	if service.Ulimits["nofile"].Soft != 1024 || service.Ulimits["nofile"].Hard != 2048 {
		t.Fatalf("Ulimits = %#v, want nofile soft=1024 hard=2048", service.Ulimits)
	}
	if service.Logging == nil {
		t.Fatal("Logging = nil, want logging config")
	}
	if service.Logging.Driver != "json-file" {
		t.Fatalf("Logging.Driver = %q, want json-file", service.Logging.Driver)
	}
	if service.Logging.Options["max-size"] != "10m" || service.Logging.Options["max-file"] != "3" {
		t.Fatalf("Logging.Options = %#v, want max-size=10m and max-file=3", service.Logging.Options)
	}
}
