package backup

import (
	"bytes"
	"context"
	"docker-manager/internal/docker"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

type fakeBackupDockerService struct {
	inspect         container.InspectResponse
	inspects        map[string]container.InspectResponse
	containers      []container.Summary
	network         network.Inspect
	volume          volume.Volume
	containerExists bool
	imageExists     bool
	calls           []string
	loadOutput      io.Writer
}

func (f *fakeBackupDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.calls = append(f.calls, "list-containers")
	return f.containers, nil
}

func (f *fakeBackupDockerService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	f.calls = append(f.calls, "inspect-container:"+name)
	if inspect, ok := f.inspects[name]; ok {
		return inspect, nil
	}
	return f.inspect, nil
}

func (f *fakeBackupDockerService) SaveImage(ctx context.Context, refs []string, outputFile string) error {
	f.calls = append(f.calls, "save-image:"+strings.Join(refs, ","))
	if err := os.MkdirAll(filepath.Dir(outputFile), 0755); err != nil {
		return err
	}
	return os.WriteFile(outputFile, []byte("image tar"), 0644)
}

func (f *fakeBackupDockerService) LoadImage(ctx context.Context, inputFile string, output io.Writer) error {
	f.calls = append(f.calls, "load-image:"+filepath.Base(inputFile))
	f.loadOutput = output
	return nil
}

func (f *fakeBackupDockerService) ImageExists(ctx context.Context, ref string) (bool, error) {
	f.calls = append(f.calls, "image-exists:"+ref)
	return f.imageExists, nil
}

func (f *fakeBackupDockerService) InspectNetwork(ctx context.Context, name string) (network.Inspect, error) {
	f.calls = append(f.calls, "inspect-network:"+name)
	return f.network, nil
}

func (f *fakeBackupDockerService) CreateNetwork(ctx context.Context, inspect network.Inspect) error {
	f.calls = append(f.calls, "create-network:"+inspect.Name)
	return nil
}

func (f *fakeBackupDockerService) InspectVolume(ctx context.Context, name string) (volume.Volume, error) {
	f.calls = append(f.calls, "inspect-volume:"+name)
	return f.volume, nil
}

func (f *fakeBackupDockerService) CreateVolume(ctx context.Context, vol volume.Volume) error {
	f.calls = append(f.calls, "create-volume:"+vol.Name)
	return nil
}

func (f *fakeBackupDockerService) ContainerExists(ctx context.Context, name string) (bool, error) {
	f.calls = append(f.calls, "container-exists:"+name)
	return f.containerExists, nil
}

func (f *fakeBackupDockerService) RemoveContainer(ctx context.Context, name string) error {
	f.calls = append(f.calls, "remove-container:"+name)
	return nil
}

func (f *fakeBackupDockerService) CreateContainer(ctx context.Context, inspect container.InspectResponse, name string) (string, error) {
	f.calls = append(f.calls, "create-container:"+name)
	return "restored-id", nil
}

func (f *fakeBackupDockerService) StartContainer(ctx context.Context, id string) error {
	f.calls = append(f.calls, "start-container:"+id)
	return nil
}

func TestBackupContainerWritesBundle(t *testing.T) {
	bindDir := t.TempDir()
	deviceDir := t.TempDir()
	devicePath := filepath.Join(deviceDir, "fake-device")
	if err := os.WriteFile(devicePath, []byte("device"), 0644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeBackupDockerService{
		inspect: container.InspectResponse{
			Name: "/demo",
			HostConfig: &container.HostConfig{
				Tmpfs: map[string]string{"/cache": "rw,noexec"},
				Resources: container.Resources{
					Devices: []container.DeviceMapping{
						{PathOnHost: devicePath, PathInContainer: "/dev/demo", CgroupPermissions: "rwm"},
					},
				},
			},
			Config: &container.Config{Image: "busybox:latest"},
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "demo_data", Destination: "/data", RW: true},
				{Type: mount.TypeBind, Source: bindDir, Destination: "/host", RW: true},
			},
			NetworkSettings: &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{
					"demo_net": {},
				},
			},
		},
		network: network.Inspect{Network: network.Network{Name: "demo_net", Driver: "bridge"}},
		volume:  volume.Volume{Name: "demo_data", Driver: "local"},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	dir := filepath.Join(t.TempDir(), "bundle")
	got, err := backupContainer(context.Background(), "demo", BackupOptions{
		OutputDir:    dir,
		IncludeImage: true,
	})
	if err != nil {
		t.Fatalf("backupContainer() error = %v", err)
	}
	if got != dir {
		t.Fatalf("backupContainer() dir = %q, want %q", got, dir)
	}

	var manifest BackupManifest
	readTestJSON(t, filepath.Join(dir, backupManifestName), &manifest)
	if len(manifest.Containers) != 1 {
		t.Fatalf("Containers = %#v, want one container", manifest.Containers)
	}
	entry := manifest.Containers[0]
	if entry.ContainerName != "demo" {
		t.Fatalf("ContainerName = %q, want demo", entry.ContainerName)
	}
	if entry.ImageArchive == "" {
		t.Fatal("ImageArchive is empty")
	}
	bindMount := findBackupMount(entry.Mounts, "bind", "/host")
	if bindMount == nil {
		t.Fatalf("Mounts = %#v, want bind mount /host", entry.Mounts)
	}
	if bindMount.Source != bindDir || bindMount.Verification != "verified-local" {
		t.Fatalf("bind mount = %#v, want verified local source %q", bindMount, bindDir)
	}
	if bindMount.HostPathExists == nil || !*bindMount.HostPathExists {
		t.Fatalf("bind mount exists = %#v, want true", bindMount.HostPathExists)
	}
	if bindMount.HostPathReadable == nil || !*bindMount.HostPathReadable {
		t.Fatalf("bind mount readable = %#v, want true", bindMount.HostPathReadable)
	}
	if bindMount.HostPathWritable == nil || !*bindMount.HostPathWritable {
		t.Fatalf("bind mount writable = %#v, want true", bindMount.HostPathWritable)
	}
	if volumeMount := findBackupMount(entry.Mounts, "volume", "/data"); volumeMount == nil || volumeMount.Name != "demo_data" {
		t.Fatalf("Mounts = %#v, want named volume demo_data", entry.Mounts)
	}
	if tmpfsMount := findBackupMount(entry.Mounts, "tmpfs", "/cache"); tmpfsMount == nil || tmpfsMount.Verification != "not-applicable" {
		t.Fatalf("Mounts = %#v, want tmpfs /cache", entry.Mounts)
	}
	if len(entry.Devices) != 1 {
		t.Fatalf("Devices = %#v, want one device", entry.Devices)
	}
	if entry.Devices[0].Type != "device" || entry.Devices[0].PathOnHost != devicePath || entry.Devices[0].PathInContainer != "/dev/demo" {
		t.Fatalf("Device = %#v, want manifest device dependency", entry.Devices[0])
	}
	if entry.Devices[0].Verification != "verified-local" || entry.Devices[0].HostPathExists == nil || !*entry.Devices[0].HostPathExists {
		t.Fatalf("Device verification = %#v, want verified local existing path", entry.Devices[0])
	}
	for _, rel := range []string{
		entry.InspectFile,
		entry.ComposeFile,
		entry.ImageArchive,
		entry.Networks[0].File,
		entry.Volumes[0].File,
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("expected backup file %s: %v", rel, err)
		}
	}
	if !hasCall(fake.calls, "save-image:busybox:latest") {
		t.Fatalf("calls = %#v, want save-image", fake.calls)
	}
}

func TestBackupMountRefsMarksBindSourceUnverifiedForRemoteDocker(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	docker.Configure(docker.Options{Host: "tcp://docker.example:2375"})

	refs := backupMountRefs(container.InspectResponse{
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: "/srv/data", Destination: "/data"},
		},
	})
	bindMount := findBackupMount(refs, "bind", "/data")
	if bindMount == nil {
		t.Fatalf("Mounts = %#v, want bind mount", refs)
	}
	if bindMount.Verification != "unverified-remote" {
		t.Fatalf("Verification = %q, want unverified-remote", bindMount.Verification)
	}
	if bindMount.HostPathExists != nil || bindMount.HostPathReadable != nil || bindMount.HostPathWritable != nil {
		t.Fatalf("remote bind mount path checks = %#v, want no local path booleans", bindMount)
	}
	if !strings.Contains(bindMount.Warning, "Docker daemon host") {
		t.Fatalf("Warning = %q, want remote daemon host warning", bindMount.Warning)
	}
}

func TestBackupMountRefsMarksNamedPipeUnverifiedLocal(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	docker.Configure(docker.Options{Host: "npipe:////./pipe/docker_engine"})

	refs := backupMountRefs(container.InspectResponse{
		Mounts: []container.MountPoint{
			{Type: mount.TypeNamedPipe, Source: `\\.\pipe\docker_engine`, Destination: `\\.\pipe\docker_engine`},
		},
	})
	npipeMount := findBackupMount(refs, "npipe", `\\.\pipe\docker_engine`)
	if npipeMount == nil {
		t.Fatalf("Mounts = %#v, want npipe mount", refs)
	}
	if npipeMount.Verification != "unverified-local" {
		t.Fatalf("Verification = %q, want unverified-local", npipeMount.Verification)
	}
	if npipeMount.HostPathExists != nil || npipeMount.HostPathReadable != nil || npipeMount.HostPathWritable != nil {
		t.Fatalf("npipe path checks = %#v, want no filesystem booleans", npipeMount)
	}
}

func TestBackupContainerReturnsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := backupContainer(ctx, "demo", BackupOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("backupContainer() error = %v, want context.Canceled", err)
	}
}

func TestBackupContainerWritesOfflineBundleArtifacts(t *testing.T) {
	fake := &fakeBackupDockerService{
		inspect: container.InspectResponse{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
			Config:     &container.Config{Image: "busybox:latest"},
		},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	root := t.TempDir()
	dir := filepath.Join(root, "bundle")
	archive := filepath.Join(root, "demo-offline.tar.gz")
	if _, err := backupContainer(context.Background(), "demo", BackupOptions{
		OutputDir:    dir,
		IncludeImage: true,
		Bundle:       true,
		BundleOutput: archive,
	}); err != nil {
		t.Fatalf("backupContainer() error = %v", err)
	}

	for _, name := range []string{backupReadmeName, backupRestoreName, backupChecksumName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected bundle artifact %s: %v", name, err)
		}
	}
	var manifest BackupManifest
	readTestJSON(t, filepath.Join(dir, backupManifestName), &manifest)
	if manifest.Tool.Version == "" || manifest.SourcePlatform == "" {
		t.Fatalf("manifest metadata = %#v, want tool and source platform", manifest)
	}
	readme, err := os.ReadFile(filepath.Join(dir, backupReadmeName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Backup metadata", "Source platform", "Prerequisites", "Checksum verification", "`dm restore` verifies `checksums.txt` by default"} {
		if !strings.Contains(string(readme), want) {
			t.Fatalf("README = %q, want %q", string(readme), want)
		}
	}
	restoreScript, err := os.ReadFile(filepath.Join(dir, backupRestoreName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dm version", "Docker daemon must be reachable", "checksums.txt"} {
		if !strings.Contains(string(restoreScript), want) {
			t.Fatalf("restore.sh = %q, want %q", string(restoreScript), want)
		}
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("expected archive %s: %v", archive, err)
	}
	checksums, err := os.ReadFile(filepath.Join(dir, backupChecksumName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checksums), backupManifestName) || strings.Contains(string(checksums), backupChecksumName) {
		t.Fatalf("checksums = %q, want manifest and no checksums self-entry", string(checksums))
	}

	extracted := filepath.Join(root, "extracted")
	if err := os.MkdirAll(extracted, 0755); err != nil {
		t.Fatal(err)
	}
	if err := extractBackupArchive(archive, extracted); err != nil {
		t.Fatalf("extractBackupArchive() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(extracted, backupManifestName)); err != nil {
		t.Fatalf("archive missing manifest: %v", err)
	}
}

func TestBackupCommandBundleOutputFlagWritesArchive(t *testing.T) {
	fake := &fakeBackupDockerService{
		containers: []container.Summary{
			{ID: "demo-id", Names: []string{"/demo"}, Image: "busybox:latest"},
		},
		inspect: container.InspectResponse{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
			Config:     &container.Config{Image: "busybox:latest"},
		},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	root := t.TempDir()
	archive := filepath.Join(root, "demo.tar.gz")
	cmd := NewBackupCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"demo", "--bundle", "--output-dir", filepath.Join(root, "backup"), "--bundle-output", archive})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("expected archive %s: %v", archive, err)
	}
}

func TestBackupCommandOnlyExposesBundleOutputFlag(t *testing.T) {
	cmd := NewBackupCommand()
	if flag := cmd.Flags().Lookup("output"); flag != nil {
		t.Fatal("output compatibility flag should be removed")
	}
	if flag := cmd.Flags().Lookup("bundle-output"); flag == nil {
		t.Fatal("bundle-output flag missing")
	}
}

func TestBackupCommandDoesNotExposeContainerSubcommand(t *testing.T) {
	cmd := NewBackupCommand()
	for _, sub := range cmd.Commands() {
		if sub.Name() == "container" {
			t.Fatal("backup command should not expose a container subcommand")
		}
	}
}

func TestBackupContainerDryRunPrintsPlanWithoutWritingFiles(t *testing.T) {
	fake := &fakeBackupDockerService{
		inspect: container.InspectResponse{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
			Config:     &container.Config{Image: "busybox:latest"},
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "demo_data", Destination: "/data"},
			},
			NetworkSettings: &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{"demo_net": {}},
			},
		},
		network: network.Inspect{Network: network.Network{Name: "demo_net", Driver: "bridge"}},
		volume:  volume.Volume{Name: "demo_data", Driver: "local"},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	dir := filepath.Join(t.TempDir(), "dry-run")
	var out bytes.Buffer
	got, err := backupContainer(context.Background(), "demo", BackupOptions{
		OutputDir:    dir,
		IncludeImage: true,
		Bundle:       true,
		DryRun:       true,
		Output:       &out,
	})
	if err != nil {
		t.Fatalf("backupContainer() error = %v", err)
	}
	if got != dir {
		t.Fatalf("backupContainer() dir = %q, want %q", got, dir)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created output dir: %v", err)
	}
	if hasCallPrefix(fake.calls, "save-image:") {
		t.Fatalf("calls = %#v, dry-run should not save image", fake.calls)
	}
	for _, want := range []string{"inspect-network:demo_net", "inspect-volume:demo_data"} {
		if !hasCall(fake.calls, want) {
			t.Fatalf("calls = %#v, want metadata validation %s", fake.calls, want)
		}
	}
	gotOutput := out.String()
	for _, want := range []string{"备份 dry-run 计划", "manifest.json", "checksums.txt", "demo_net", "demo_data", "不会写入文件"} {
		if !strings.Contains(gotOutput, want) {
			t.Fatalf("output = %q, want %q", gotOutput, want)
		}
	}
}

func TestBackupContainersSeparateByDefault(t *testing.T) {
	fake := &fakeBackupDockerService{
		containers: []container.Summary{
			{ID: "api-id", Names: []string{"/api-1"}, Image: "demo/api:latest"},
			{ID: "worker-id", Names: []string{"/worker"}, Image: "demo/worker:latest"},
		},
		inspects: map[string]container.InspectResponse{
			"api-1": {
				Name:       "/api-1",
				HostConfig: &container.HostConfig{},
				Config:     &container.Config{Image: "demo/api:latest"},
			},
			"worker": {
				Name:       "/worker",
				HostConfig: &container.HostConfig{},
				Config:     &container.Config{Image: "demo/worker:latest"},
			},
		},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	root := filepath.Join(t.TempDir(), "batch")
	result, err := backupContainers(context.Background(), []string{"api-*", "worker"}, BackupOptions{
		OutputDir:    root,
		IncludeImage: false,
	})
	if err != nil {
		t.Fatalf("backupContainers() error = %v", err)
	}
	if len(result.Paths) != 2 {
		t.Fatalf("Paths = %#v, want 2 paths", result.Paths)
	}
	for _, rel := range []string{"api-1", "worker"} {
		if _, err := os.Stat(filepath.Join(root, rel, backupManifestName)); err != nil {
			t.Fatalf("missing %s manifest: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, backupManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("top-level manifest exists for separate backup: %v", err)
	}
}

func TestBackupCommandNoImageDisablesImageExport(t *testing.T) {
	fake := &fakeBackupDockerService{
		containers: []container.Summary{
			{ID: "demo-id", Names: []string{"/demo"}, Image: "busybox:latest"},
		},
		inspect: container.InspectResponse{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
			Config:     &container.Config{Image: "busybox:latest"},
		},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	cmd := NewBackupCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"demo", "--no-image", "--output-dir", t.TempDir()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if hasCallPrefix(fake.calls, "save-image:") {
		t.Fatalf("calls = %#v, --no-image should skip image export", fake.calls)
	}
}

func TestBackupContainersMergeWritesBatchBundle(t *testing.T) {
	fake := &fakeBackupDockerService{
		containers: []container.Summary{
			{ID: "api-id", Names: []string{"/api"}, Image: "demo/api:latest"},
			{ID: "worker-id", Names: []string{"/worker"}, Image: "demo/worker:latest"},
		},
		inspects: map[string]container.InspectResponse{
			"api": {
				Name:       "/api",
				HostConfig: &container.HostConfig{},
				Config:     &container.Config{Image: "demo/api:latest"},
			},
			"worker": {
				Name:       "/worker",
				HostConfig: &container.HostConfig{},
				Config:     &container.Config{Image: "demo/worker:latest"},
			},
		},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	root := t.TempDir()
	dir := filepath.Join(root, "merged")
	archive := filepath.Join(root, "merged.tar.gz")
	result, err := backupContainers(context.Background(), []string{"api", "worker"}, BackupOptions{
		OutputDir:    dir,
		IncludeImage: false,
		Merge:        true,
		Bundle:       true,
		BundleOutput: archive,
	})
	if err != nil {
		t.Fatalf("backupContainers() error = %v", err)
	}
	if len(result.Paths) != 1 || result.Paths[0] != dir {
		t.Fatalf("Paths = %#v, want merged dir", result.Paths)
	}
	var manifest BackupManifest
	readTestJSON(t, filepath.Join(dir, backupManifestName), &manifest)
	if len(manifest.Containers) != 2 {
		t.Fatalf("manifest = %#v, want 2 containers", manifest)
	}
	for _, rel := range []string{
		filepath.Join("containers", "api", backupManifestName),
		filepath.Join("containers", "worker", backupManifestName),
		backupReadmeName,
		backupRestoreName,
		backupChecksumName,
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Fatalf("missing merged backup file %s: %v", rel, err)
		}
	}
	extracted := filepath.Join(root, "extracted")
	if err := os.MkdirAll(extracted, 0755); err != nil {
		t.Fatal(err)
	}
	if err := extractBackupArchive(archive, extracted); err != nil {
		t.Fatalf("extractBackupArchive() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(extracted, backupManifestName)); err != nil {
		t.Fatalf("archive missing manifest: %v", err)
	}
}

func TestRestoreBackupRejectsExistingContainerWithoutReplace(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version:       1,
		ContainerName: "demo",
		InspectFile:   backupInspectName,
		ComposeFile:   backupComposeName,
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name:       "/demo",
		HostConfig: &container.HostConfig{},
		Config:     &container.Config{Image: "busybox:latest"},
	})

	fake := &fakeBackupDockerService{containerExists: true}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{})
	if err == nil {
		t.Fatal("restoreBackup() error = nil, want existing container error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("restoreBackup() error = %v, want already exists", err)
	}
	if hasCallPrefix(fake.calls, "create-container:") {
		t.Fatalf("calls = %#v, create-container should not run", fake.calls)
	}
}

func TestRestoreBackupDirReturnsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := restoreBackupDir(ctx, t.TempDir(), RestoreOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("restoreBackupDir() error = %v, want context.Canceled", err)
	}
}

func TestBackupArchiveReturnsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := createBackupArchiveWithContext(ctx, t.TempDir(), filepath.Join(t.TempDir(), "backup.tar.gz"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("createBackupArchiveWithContext() error = %v, want context.Canceled", err)
	}
}

func TestBackupChecksumsReturnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := writeChecksumsWithContext(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("writeChecksumsWithContext() error = %v, want context.Canceled", err)
	}
}

func TestRestoreBackupSupportsTarGzArchive(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bundle")
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version:       1,
		ContainerName: "demo",
		InspectFile:   backupInspectName,
		ComposeFile:   backupComposeName,
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name:       "/demo",
		HostConfig: &container.HostConfig{},
		Config:     &container.Config{Image: "busybox:latest"},
	})
	archive := filepath.Join(root, "bundle.tar.gz")
	if err := createBackupArchive(dir, archive); err != nil {
		t.Fatalf("createBackupArchive() error = %v", err)
	}

	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	if err := restoreBackup(context.Background(), archive, RestoreOptions{NoStart: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	for _, want := range []string{"container-exists:demo", "create-container:demo"} {
		if !hasCall(fake.calls, want) {
			t.Fatalf("calls = %#v, want %s", fake.calls, want)
		}
	}
	if hasCallPrefix(fake.calls, "start-container:") {
		t.Fatalf("calls = %#v, start-container should not run with NoStart", fake.calls)
	}
}

func TestRestoreBackupSupportsBatchManifest(t *testing.T) {
	root := t.TempDir()
	writeTestJSON(t, filepath.Join(root, backupManifestName), BackupManifest{
		Version:   1,
		CreatedAt: "2026-06-25T00:00:00Z",
		Containers: []BackupContainerManifest{
			{ContainerName: "api", Path: filepath.ToSlash(filepath.Join("containers", "api"))},
			{ContainerName: "worker", Path: filepath.ToSlash(filepath.Join("containers", "worker"))},
		},
	})
	for _, name := range []string{"api", "worker"} {
		dir := filepath.Join(root, "containers", name)
		writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
			Version:       1,
			ContainerName: name,
			InspectFile:   backupInspectName,
			ComposeFile:   backupComposeName,
		})
		writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
			Name:       "/" + name,
			HostConfig: &container.HostConfig{},
			Config:     &container.Config{Image: "busybox:latest"},
		})
	}
	if err := writeChecksums(root); err != nil {
		t.Fatalf("writeChecksums() error = %v", err)
	}

	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	if err := restoreBackup(context.Background(), root, RestoreOptions{NoStart: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	for _, want := range []string{
		"container-exists:api",
		"create-container:api",
		"container-exists:worker",
		"create-container:worker",
	} {
		if !hasCall(fake.calls, want) {
			t.Fatalf("calls = %#v, want %s", fake.calls, want)
		}
	}
	if hasCallPrefix(fake.calls, "start-container:") {
		t.Fatalf("calls = %#v, start-container should not run with NoStart", fake.calls)
	}
}

func TestVerifyBackupChecksumsDetectsMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, backupManifestName), []byte("manifest"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "data.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(dir); err != nil {
		t.Fatalf("writeChecksums() error = %v", err)
	}
	verified, err := verifyBackupChecksums(dir)
	if err != nil {
		t.Fatalf("verifyBackupChecksums() error = %v", err)
	}
	if !verified {
		t.Fatal("verifyBackupChecksums() verified = false, want true")
	}

	if err := os.WriteFile(filepath.Join(dir, "nested", "data.txt"), []byte("tampered"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = verifyBackupChecksums(dir)
	if err == nil {
		t.Fatal("verifyBackupChecksums() error = nil, want mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("verifyBackupChecksums() error = %v, want checksum mismatch", err)
	}
}

func TestRestoreBackupVerifiesChecksumsBeforeDockerActions(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version:       1,
		ContainerName: "demo",
		InspectFile:   backupInspectName,
		ComposeFile:   backupComposeName,
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name:       "/demo",
		HostConfig: &container.HostConfig{},
		Config:     &container.Config{Image: "busybox:latest"},
	})
	if err := writeChecksums(dir); err != nil {
		t.Fatalf("writeChecksums() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupInspectName), []byte("{}\n "), 0644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{NoStart: true})
	if err == nil {
		t.Fatal("restoreBackup() error = nil, want checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("restoreBackup() error = %v, want checksum mismatch", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %#v, want no Docker actions before checksum passes", fake.calls)
	}
}

func TestRestoreBackupCanSkipChecksumVerification(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version:       1,
		ContainerName: "demo",
		InspectFile:   backupInspectName,
		ComposeFile:   backupComposeName,
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name:       "/demo",
		HostConfig: &container.HostConfig{},
		Config:     &container.Config{Image: "busybox:latest"},
	})
	if err := writeChecksums(dir); err != nil {
		t.Fatalf("writeChecksums() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupInspectName), []byte("{}\n "), 0644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	if err := restoreBackup(context.Background(), dir, RestoreOptions{NoStart: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	if !hasCall(fake.calls, "container-exists:demo") || !hasCall(fake.calls, "create-container:demo") {
		t.Fatalf("calls = %#v, want restore to continue when checksum is skipped", fake.calls)
	}
}

func TestRestoreBackupDryRunPrintsPlanWithoutDockerMutations(t *testing.T) {
	dir := t.TempDir()
	imageArchive := filepath.ToSlash(filepath.Join("images", "busybox.tar"))
	networkFile := filepath.ToSlash(filepath.Join("networks", "demo_net.json"))
	volumeFile := filepath.ToSlash(filepath.Join("volumes", "demo_data.json"))
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version:       1,
		ContainerName: "demo",
		Image:         "busybox:latest",
		ImageArchive:  imageArchive,
		InspectFile:   backupInspectName,
		ComposeFile:   backupComposeName,
		Networks:      []BackupResourceRef{{Name: "demo_net", File: networkFile}},
		Volumes:       []BackupResourceRef{{Name: "demo_data", File: volumeFile}},
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name: "/demo",
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{
				network.MustParsePort("80/tcp"): []network.PortBinding{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}},
			},
		},
		Config: &container.Config{Image: "busybox:latest"},
	})
	writeTestJSON(t, filepath.Join(dir, filepath.FromSlash(networkFile)), network.Inspect{Network: network.Network{Name: "demo_net"}})
	writeTestJSON(t, filepath.Join(dir, filepath.FromSlash(volumeFile)), volume.Volume{Name: "demo_data"})
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(imageArchive)), []byte("image tar"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(dir); err != nil {
		t.Fatalf("writeChecksums() error = %v", err)
	}

	fake := &fakeBackupDockerService{containerExists: true}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	var out bytes.Buffer
	if err := restoreBackup(context.Background(), dir, RestoreOptions{DryRun: true, Output: &out}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	for _, forbidden := range []string{"load-image:", "create-network:", "create-volume:", "remove-container:", "create-container:", "start-container:"} {
		if hasCallPrefix(fake.calls, forbidden) {
			t.Fatalf("calls = %#v, dry-run should not call %s", fake.calls, forbidden)
		}
	}
	if !hasCall(fake.calls, "container-exists:demo") {
		t.Fatalf("calls = %#v, want existence check", fake.calls)
	}
	gotOutput := out.String()
	for _, want := range []string{"恢复 dry-run 计划", "已校验 checksums.txt", "将导入镜像归档", "demo_net", "demo_data", "0.0.0.0:8080->80/tcp", "存在覆盖冲突"} {
		if !strings.Contains(gotOutput, want) {
			t.Fatalf("output = %q, want %q", gotOutput, want)
		}
	}
}

func TestBuildRestorePlanReportPreviewsDiffsWithoutMutations(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version: 1,
		Containers: []BackupContainerManifest{{
			ContainerName: "web",
			Image:         "nginx:latest",
			ImageArchive:  filepath.ToSlash(filepath.Join("images", "nginx.tar")),
			InspectFile:   backupInspectName,
			Networks:      []BackupResourceRef{{Name: "web_net", File: filepath.ToSlash(filepath.Join("networks", "web_net.json"))}},
			Volumes:       []BackupResourceRef{{Name: "web_data", File: filepath.ToSlash(filepath.Join("volumes", "web_data.json"))}},
		}},
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name: "/web",
		Config: &container.Config{
			Image: "nginx:latest",
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{
				network.MustParsePort("80/tcp"): []network.PortBinding{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}},
			},
		},
	})
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "nginx.tar"), []byte("image"), 0644); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, filepath.Join(dir, "networks", "web_net.json"), network.Inspect{Network: network.Network{Name: "web_net", Driver: "bridge"}})
	writeTestJSON(t, filepath.Join(dir, "volumes", "web_data.json"), volume.Volume{Name: "web_data", Driver: "local"})

	fake := &fakeBackupDockerService{
		containerExists: true,
		imageExists:     false,
		containers:      []container.Summary{{ID: "existing-id", Names: []string{"/old-web"}}},
		inspects: map[string]container.InspectResponse{
			"existing-id": {
				Name: "/old-web",
				HostConfig: &container.HostConfig{
					PortBindings: network.PortMap{
						network.MustParsePort("80/tcp"): []network.PortBinding{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}},
					},
				},
			},
		},
		network: network.Inspect{Network: network.Network{Name: "web_net", Driver: "overlay"}},
		volume:  volume.Volume{Name: "web_data", Driver: "local"},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	report, err := buildRestorePlanReport(context.Background(), dir, RestoreOptions{Replace: true, NoStart: true, SkipChecksum: true})
	if err != nil {
		t.Fatalf("buildRestorePlanReport() error = %v", err)
	}
	if report.ContainerCount != 1 || len(report.Containers) != 1 {
		t.Fatalf("report = %#v, want one container", report)
	}
	plan := report.Containers[0]
	if plan.Container.Action != "replace" || plan.Image.Action != "load-archive" {
		t.Fatalf("plan actions = container:%s image:%s, want replace/load-archive", plan.Container.Action, plan.Image.Action)
	}
	if len(plan.Networks) != 1 || !plan.Networks[0].Different {
		t.Fatalf("network plan = %#v, want existing different network", plan.Networks)
	}
	if len(plan.PortConflicts) != 1 || plan.PortConflicts[0].Container != "old-web" {
		t.Fatalf("port conflicts = %#v, want old-web conflict", plan.PortConflicts)
	}
	for _, forbidden := range []string{"load-image", "create-network", "create-volume", "remove-container", "create-container", "start-container"} {
		if hasCallPrefix(fake.calls, forbidden) {
			t.Fatalf("plan calls = %#v, should not call %s", fake.calls, forbidden)
		}
	}
}

func TestRestoreCommandExposesPlanAndFormatFlags(t *testing.T) {
	cmd := NewRestoreCommand()
	for _, name := range []string{"plan", "format"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("restore command missing --%s", name)
		}
	}
}

func TestRestoreBackupReplaceCreatesAndStartsContainer(t *testing.T) {
	dir := t.TempDir()
	imageArchive := filepath.ToSlash(filepath.Join("images", "busybox.tar"))
	networkFile := filepath.ToSlash(filepath.Join("networks", "demo_net.json"))
	volumeFile := filepath.ToSlash(filepath.Join("volumes", "demo_data.json"))
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version:       1,
		ContainerName: "demo",
		ImageArchive:  imageArchive,
		InspectFile:   backupInspectName,
		ComposeFile:   backupComposeName,
		Networks:      []BackupResourceRef{{Name: "demo_net", File: networkFile}},
		Volumes:       []BackupResourceRef{{Name: "demo_data", File: volumeFile}},
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name:       "/demo",
		HostConfig: &container.HostConfig{},
		Config:     &container.Config{Image: "busybox:latest"},
	})
	writeTestJSON(t, filepath.Join(dir, filepath.FromSlash(networkFile)), network.Inspect{Network: network.Network{Name: "demo_net"}})
	writeTestJSON(t, filepath.Join(dir, filepath.FromSlash(volumeFile)), volume.Volume{Name: "demo_data"})
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(imageArchive)), []byte("image tar"), 0644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeBackupDockerService{containerExists: true}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	var out bytes.Buffer
	if err := restoreBackup(context.Background(), dir, RestoreOptions{Replace: true, Output: &out}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	if fake.loadOutput != &out {
		t.Fatalf("LoadImage output = %#v, want restore output writer", fake.loadOutput)
	}

	wantCalls := []string{
		"load-image:busybox.tar",
		"create-network:demo_net",
		"create-volume:demo_data",
		"container-exists:demo",
		"remove-container:demo",
		"create-container:demo",
		"start-container:restored-id",
	}
	for _, want := range wantCalls {
		if !hasCall(fake.calls, want) {
			t.Fatalf("calls = %#v, want %s", fake.calls, want)
		}
	}
}

func TestSafeExtractPathRejectsTraversal(t *testing.T) {
	if _, err := safeExtractPath(t.TempDir(), "../evil"); err == nil {
		t.Fatal("safeExtractPath() error = nil, want traversal error")
	}
}

func replaceBackupServiceFactory(fake *fakeBackupDockerService) func() {
	previous := newBackupDockerService
	newBackupDockerService = func() (backupDockerService, error) {
		if fake == nil {
			return nil, errors.New("missing fake service")
		}
		return fake, nil
	}
	return func() {
		newBackupDockerService = previous
	}
}

func writeTestJSON(t *testing.T, path string, value interface{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}

func readTestJSON(t *testing.T, path string, value interface{}) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func findBackupMount(refs []BackupMountRef, mountType, destination string) *BackupMountRef {
	for i := range refs {
		if refs[i].Type == mountType && refs[i].Destination == destination {
			return &refs[i]
		}
	}
	return nil
}

func hasCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

func hasCallPrefix(calls []string, prefix string) bool {
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}
