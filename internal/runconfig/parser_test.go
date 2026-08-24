package runconfig

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"docker-manager/internal/sensitive"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

func TestParserBuildsCompleteSpec(t *testing.T) {
	inspect := container.InspectResponse{
		Name: "/demo",
		Config: &container.Config{
			Image:      "example/demo:latest",
			User:       "1000:1000",
			Env:        []string{"PATH=/usr/bin", "MODE=prod", "PASSWORD=secret"},
			Labels:     map[string]string{"owner": "team-a", "api_token": "secret-token"},
			Cmd:        []string{"serve", "--foreground"},
			Entrypoint: []string{"/bin/sh", "-c"},
			WorkingDir: "/work",
		},
		HostConfig: &container.HostConfig{
			DNS:             []netip.Addr{netip.MustParseAddr("1.1.1.1"), {}},
			DNSSearch:       []string{"svc.local"},
			ExtraHosts:      []string{"api.local:10.0.0.8"},
			CapAdd:          []string{"NET_ADMIN"},
			CapDrop:         []string{"MKNOD"},
			SecurityOpt:     []string{"no-new-privileges:true"},
			Privileged:      true,
			PublishAllPorts: true,
			AutoRemove:      true,
			RestartPolicy: container.RestartPolicy{
				Name:              "on-failure",
				MaximumRetryCount: 3,
			},
			NetworkMode: "bridge",
			LogConfig: container.LogConfig{
				Type:   "json-file",
				Config: map[string]string{"max-size": "10m"},
			},
			Resources: container.Resources{
				Devices: []container.DeviceMapping{
					{PathOnHost: "/dev/fuse", PathInContainer: "/dev/fuse", CgroupPermissions: "rwm"},
					{PathOnHost: "/dev/kvm"},
				},
				Ulimits: []*container.Ulimit{
					{Name: "nofile", Soft: 1024, Hard: 2048}, nil, {Name: ""},
				},
			},
			PortBindings: network.PortMap{
				network.MustParsePort("80/tcp"): {{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}},
			},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "demo-data", Destination: "/data"},
			{Type: mount.TypeBind, Source: "/srv/config", Destination: "/config", Mode: "ro"},
			{Type: mount.TypeTmpfs, Source: "tmpfs", Destination: "/tmp"},
		},
	}

	spec := NewParser(inspect, ReverseOptions{
		PreserveVolumes:   true,
		FilterDefaultEnvs: true,
		RedactProfile:     "basic",
	}).ToSpec()

	if spec.Image != "example/demo:latest" || spec.ContainerName != "demo" || spec.RestartPolicy != "on-failure:3" {
		t.Fatalf("identity fields = %#v", spec)
	}
	if got, want := spec.Envs, []string{"MODE=prod", "PASSWORD=<redacted>"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Envs = %#v, want %#v", got, want)
	}
	if spec.Labels["api_token"] != sensitive.RedactedValue || spec.Labels["owner"] != "team-a" {
		t.Fatalf("Labels = %#v", spec.Labels)
	}
	if got, want := spec.Mounts, []string{"demo-data:/data", "/srv/config:/config:ro", "tmpfs:/tmp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Mounts = %#v, want %#v", got, want)
	}
	if got, want := spec.Devices, []string{"/dev/fuse:/dev/fuse:rwm", "/dev/kvm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Devices = %#v, want %#v", got, want)
	}
	if spec.Ulimits["nofile"] != (UlimitSpec{Soft: 1024, Hard: 2048}) {
		t.Fatalf("Ulimits = %#v", spec.Ulimits)
	}
	if got := spec.PortBindings; len(got) != 1 || got[0] != (PortBindingSpec{HostPort: 8080, ContPort: 80, Proto: "tcp"}) {
		t.Fatalf("PortBindings = %#v", got)
	}
	if got := spec.DNS; !reflect.DeepEqual(got, []string{"1.1.1.1"}) {
		t.Fatalf("DNS = %#v", got)
	}

	inspect.Config.Labels["owner"] = "changed"
	inspect.HostConfig.LogConfig.Config["max-size"] = "changed"
	if spec.Labels["owner"] != "team-a" || spec.LogOptions["max-size"] != "10m" {
		t.Fatal("ToSpec returned maps aliased to inspect input")
	}
}

func TestParserUsesRuntimePortsAndSkipsInvalidBindings(t *testing.T) {
	port := network.MustParsePort("80/tcp")
	inspect := container.InspectResponse{
		Name:   "demo",
		Config: &container.Config{Image: "busybox"},
		HostConfig: &container.HostConfig{PortBindings: network.PortMap{
			port: {
				{HostIP: netip.MustParseAddr("127.0.0.1")},
				{HostPort: "invalid"},
			},
		}},
		NetworkSettings: &container.NetworkSettings{Ports: network.PortMap{
			port: {
				{HostPort: ""},
				{HostPort: "bad"},
				{HostPort: "32768"},
			},
		}},
	}

	result := NewParser(inspect, ReverseOptions{}).ToResult()
	if len(result.Compose.Ports) != 1 || result.Compose.Ports[0] != "127.0.0.1:32768:80/tcp" {
		t.Fatalf("Compose.Ports = %#v", result.Compose.Ports)
	}
	if !strings.Contains(strings.Join(result.Command, " "), "-p 127.0.0.1:32768:80/tcp") {
		t.Fatalf("Command = %#v", result.Command)
	}

	inspect.NetworkSettings = nil
	if got := NewParser(inspect, ReverseOptions{}).ToSpec().PortBindings; len(got) != 0 {
		t.Fatalf("PortBindings without runtime state = %#v", got)
	}
}

func TestCommandFormatterCoversOptionsAndStableMaps(t *testing.T) {
	spec := &ContainerSpec{
		Image:           "busybox:latest",
		ContainerName:   "demo",
		Labels:          map[string]string{"z": "last", "a": "first"},
		DNS:             []string{"1.1.1.1"},
		DNSSearch:       []string{"svc.local"},
		ExtraHosts:      []string{"api:10.0.0.8"},
		CapAdd:          []string{"NET_ADMIN"},
		CapDrop:         []string{"MKNOD"},
		SecurityOpt:     []string{"seccomp=unconfined"},
		Devices:         []string{"/dev/fuse:/dev/fuse:rwm"},
		Ulimits:         map[string]UlimitSpec{"nproc": {Soft: 256, Hard: 512}, "nofile": {Soft: 1024, Hard: 2048}},
		LogDriver:       "json-file",
		LogOptions:      map[string]string{"max-size": "10m", "max-file": "3"},
		Privileged:      true,
		PublishAllPorts: true,
		AutoRemove:      true,
		RestartPolicy:   "unless-stopped",
		User:            "1000",
		Envs:            []string{"MODE=prod"},
		Mounts:          []string{"data:/data"},
		PortBindings: []PortBindingSpec{
			{HostIP: "127.0.0.1", HostPort: 8080, ContPort: 80, Proto: "tcp"},
			{HostIP: "127.0.0.1", HostPort: 8081, ContPort: 81, Proto: "tcp"},
		},
		Cmd:         []string{"serve"},
		Entrypoint:  []string{"/bin/sh", "-c"},
		WorkingDir:  "/work",
		NetworkMode: "host",
	}

	got := strings.Join(CommandFormatter{}.Format(spec, ReverseOptions{MergePorts: true}), " ")
	for _, want := range []string{
		"--privileged", " -P ", "--rm", "--restart unless-stopped", "-u 1000",
		"--entrypoint /bin/sh", "-w /work", "--network host",
		"--label a=first --label z=last", "--dns 1.1.1.1", "--dns-search svc.local",
		"--add-host api:10.0.0.8", "--cap-add NET_ADMIN", "--cap-drop MKNOD",
		"--security-opt seccomp=unconfined", "--device /dev/fuse:/dev/fuse:rwm",
		"--ulimit nofile=1024:2048 --ulimit nproc=256:512",
		"--log-driver json-file --log-opt max-file=3 --log-opt max-size=10m",
		"-e MODE=prod", "-v data:/data", "-p 127.0.0.1:8080-8081:80-81/tcp",
		CommandSplitMarker + " busybox:latest -c serve",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("command = %q, want fragment %q", got, want)
		}
	}

	plain := strings.Join(CommandFormatter{}.Format(&ContainerSpec{
		Image:         "busybox",
		ContainerName: "plain",
		PortBindings: []PortBindingSpec{
			{HostPort: 8080, ContPort: 80, Proto: "tcp"},
			{HostIP: "::1", HostPort: 8443, ContPort: 443, Proto: "tcp"},
		},
	}, ReverseOptions{}), " ")
	if !strings.Contains(plain, "-p 8080:80/tcp") || !strings.Contains(plain, "-p ::1:8443:443/tcp") {
		t.Fatalf("unmerged command = %q", plain)
	}
}

func TestMergePortRangesAndComposeFormatting(t *testing.T) {
	if got := MergePortRanges(nil); got != nil {
		t.Fatalf("MergePortRanges(nil) = %#v", got)
	}
	got := MergePortRanges([]PortBindingSpec{
		{HostIP: "127.0.0.1", HostPort: 8081, ContPort: 81, Proto: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 8080, ContPort: 80, Proto: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 8083, ContPort: 83, Proto: "tcp"},
	})
	want := []string{"127.0.0.1:8080-8081:80-81/tcp", "127.0.0.1:8083:83/tcp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergePortRanges() = %#v, want %#v", got, want)
	}

	spec := &ContainerSpec{
		Image:         "demo:latest",
		ContainerName: "demo",
		RestartPolicy: "on-failure:4",
		PortBindings: []PortBindingSpec{
			{HostPort: 8080, ContPort: 80, Proto: "tcp"},
			{HostIP: "127.0.0.1", HostPort: 8443, ContPort: 443, Proto: "tcp"},
		},
		LogOptions: map[string]string{"max-file": "3"},
	}
	service := ComposeFormatter{}.Format(spec)
	if service.Restart != "on-failure" || !reflect.DeepEqual(service.Ports, []string{"8080:80/tcp", "127.0.0.1:8443:443/tcp"}) {
		t.Fatalf("Compose service = %#v", service)
	}
	if service.Logging == nil || service.Logging.Options["max-file"] != "3" {
		t.Fatalf("Compose logging = %#v", service.Logging)
	}
	if logging := formatComposeLogging(&ContainerSpec{}); logging != nil {
		t.Fatalf("empty logging = %#v", logging)
	}
}

func TestParserProfilePrecedenceAndHelpers(t *testing.T) {
	previous := sensitive.DefaultProfile()
	sensitive.SetDefaultProfile(sensitive.ProfileStrict)
	t.Cleanup(func() { sensitive.SetDefaultProfile(previous) })

	inspect := container.InspectResponse{
		Name:       "/demo",
		HostConfig: &container.HostConfig{},
		Config: &container.Config{
			Image:  "busybox",
			Env:    []string{"PUBLIC_KEY=secret", "MODE=prod"},
			Labels: map[string]string{"session_id": "secret", "owner": "team-a"},
		},
	}
	inherited := NewParser(inspect, ReverseOptions{}).ToResult()
	if inherited.Name != "demo" || inherited.Compose.Environment[0] != "PUBLIC_KEY=<redacted>" || inherited.Compose.Labels["session_id"] != "<redacted>" {
		t.Fatalf("inherited strict result = %#v", inherited)
	}
	explicitNone := NewParser(inspect, ReverseOptions{RedactProfile: "none"}).ToSpec()
	if explicitNone.Envs[0] != "PUBLIC_KEY=secret" || explicitNone.Labels["session_id"] != "secret" {
		t.Fatalf("explicit none spec = %#v", explicitNone)
	}
	if _, err := normalizeRedactProfile("invalid", false); err == nil {
		t.Fatal("normalizeRedactProfile(invalid) error = nil")
	}

	if got := formatDevice("/dev/a", "/dev/b", ""); got != "/dev/a:/dev/b" {
		t.Fatalf("formatDevice() = %q", got)
	}
	if got := normalizeIP("[::]"); got != "" {
		t.Fatalf("normalizeIP([::]) = %q", got)
	}
	if got := trimContainerName("plain"); got != "plain" {
		t.Fatalf("trimContainerName() = %q", got)
	}
	if got := copyStringSlice(nil); got != nil {
		t.Fatalf("copyStringSlice(nil) = %#v", got)
	}
	if got := copyStringMap(nil); got != nil {
		t.Fatalf("copyStringMap(nil) = %#v", got)
	}
}
