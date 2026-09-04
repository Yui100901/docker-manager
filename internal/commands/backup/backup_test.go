package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/x509"
	"docker-manager/internal/docker"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

type fakeBackupDockerService struct {
	mu                    sync.Mutex
	inspect               container.InspectResponse
	inspects              map[string]container.InspectResponse
	inspectErrors         map[string]error
	containers            []container.Summary
	network               network.Inspect
	volume                volume.Volume
	containerExists       bool
	imageMissing          bool
	calls                 []string
	loadOutput            io.Writer
	loadErr               error
	createErr             error
	createIDOnError       string
	createCommitError     bool
	startErr              error
	stopErr               error
	renameErr             error
	removeErr             error
	startErrors           map[string]error
	renameErrors          map[string]error
	removeErrors          map[string]error
	startCommitError      map[string]bool
	removeCommitError     map[string]bool
	stopCommit            bool
	renameBeforeCommitErr error
	removedContainers     map[string]bool
	cancelAfterCreate     context.CancelFunc
	cancelAfterStart      context.CancelFunc
	cancelAfterSave       context.CancelFunc
	afterSave             func(string) error
	readyErr              error
	renameCommitError     map[string]bool
}

func (f *fakeBackupDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "list-containers")
	f.mu.Unlock()
	return f.containers, nil
}

func (f *fakeBackupDockerService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "inspect-container:"+name)
	f.mu.Unlock()
	if err := f.inspectErrors[name]; err != nil {
		return container.InspectResponse{}, err
	}
	if f.removedContainers[name] {
		return container.InspectResponse{}, cerrdefs.ErrNotFound
	}
	if inspect, ok := f.inspects[name]; ok {
		return inspect, nil
	}
	return f.inspect, nil
}

func (f *fakeBackupDockerService) SaveImage(ctx context.Context, refs []string, outputFile string) error {
	f.mu.Lock()
	f.calls = append(f.calls, "save-image:"+strings.Join(refs, ","))
	f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(outputFile), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(outputFile, []byte("image tar"), 0644); err != nil {
		return err
	}
	if f.cancelAfterSave != nil {
		f.cancelAfterSave()
	}
	if f.afterSave != nil {
		return f.afterSave(outputFile)
	}
	return nil
}

func (f *fakeBackupDockerService) LoadImage(ctx context.Context, inputFile string, output io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, "load-image:"+filepath.Base(inputFile))
	f.loadOutput = output
	f.mu.Unlock()
	return f.loadErr
}

func (f *fakeBackupDockerService) ImageExists(ctx context.Context, ref string) (bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "image-exists:"+ref)
	f.mu.Unlock()
	if f.imageMissing {
		return false, nil
	}
	return true, nil
}

func (f *fakeBackupDockerService) InspectNetwork(ctx context.Context, name string) (network.Inspect, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "inspect-network:"+name)
	f.mu.Unlock()
	return f.network, nil
}

func (f *fakeBackupDockerService) CreateNetwork(ctx context.Context, inspect network.Inspect) error {
	f.mu.Lock()
	f.calls = append(f.calls, "create-network:"+inspect.Name)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackupDockerService) InspectVolume(ctx context.Context, name string) (volume.Volume, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "inspect-volume:"+name)
	f.mu.Unlock()
	return f.volume, nil
}

func (f *fakeBackupDockerService) CreateVolume(ctx context.Context, vol volume.Volume) error {
	f.mu.Lock()
	f.calls = append(f.calls, "create-volume:"+vol.Name)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackupDockerService) ContainerExists(ctx context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "container-exists:"+name)
	if strings.Contains(name, "-dm-restore-candidate-") {
		_, exists := f.inspects[name]
		return exists, nil
	}
	return f.containerExists, nil
}

func (f *fakeBackupDockerService) RemoveContainer(ctx context.Context, name string) error {
	f.mu.Lock()
	f.calls = append(f.calls, "remove-container:"+name)
	f.mu.Unlock()
	if err := f.removeErrors[name]; err != nil {
		if f.removeCommitError[name] {
			f.recordRemovedContainer(name)
		}
		return err
	}
	return f.removeErr
}

func (f *fakeBackupDockerService) StopContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	f.calls = append(f.calls, "stop-container:"+id)
	f.mu.Unlock()
	if f.stopCommit {
		f.recordContainerRunningState(id, false)
	}
	return f.stopErr
}

func (f *fakeBackupDockerService) RenameContainer(ctx context.Context, id, name string) error {
	f.mu.Lock()
	f.calls = append(f.calls, "rename-container:"+id+":"+name)
	f.mu.Unlock()
	if err := f.renameErrors[name]; err != nil {
		if f.renameCommitError[name] {
			f.recordRenamedContainer(id, name)
		}
		return err
	}
	if f.renameBeforeCommitErr != nil {
		return f.renameBeforeCommitErr
	}
	f.recordRenamedContainer(id, name)
	return f.renameErr
}

func (f *fakeBackupDockerService) recordRenamedContainer(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspects == nil {
		f.inspects = make(map[string]container.InspectResponse)
	}
	inspect := f.inspects[id]
	if inspect.ID == "" {
		inspect.ID = id
	}
	inspect.Name = "/" + name
	f.inspects[id] = inspect
	f.inspects[name] = inspect
	delete(f.removedContainers, id)
	delete(f.removedContainers, name)
}

func (f *fakeBackupDockerService) CreateContainer(ctx context.Context, inspect container.InspectResponse, name string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "create-container:"+name)
	f.mu.Unlock()
	if f.cancelAfterCreate != nil {
		f.cancelAfterCreate()
	}
	if f.createErr != nil {
		if f.createCommitError {
			id := f.createIDOnError
			if id == "" {
				id = "restored-id"
			}
			f.recordCreatedContainer(id, name, inspect)
		}
		return f.createIDOnError, f.createErr
	}
	f.recordCreatedContainer("restored-id", name, inspect)
	return "restored-id", nil
}

func (f *fakeBackupDockerService) recordCreatedContainer(id, name string, inspect container.InspectResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspects == nil {
		f.inspects = make(map[string]container.InspectResponse)
	}
	inspect.ID = id
	inspect.Name = "/" + name
	f.inspects[id] = inspect
	f.inspects[name] = inspect
	delete(f.removedContainers, id)
	delete(f.removedContainers, name)
}

func (f *fakeBackupDockerService) StartContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	f.calls = append(f.calls, "start-container:"+id)
	f.mu.Unlock()
	if f.cancelAfterStart != nil {
		f.cancelAfterStart()
	}
	if err := f.startErrors[id]; err != nil {
		if f.startCommitError[id] {
			f.recordContainerRunningState(id, true)
		}
		return err
	}
	return f.startErr
}

func (f *fakeBackupDockerService) recordRemovedContainer(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removedContainers == nil {
		f.removedContainers = make(map[string]bool)
	}
	f.removedContainers[id] = true
	for key, inspect := range f.inspects {
		if key == id || inspect.ID == id {
			f.removedContainers[key] = true
			delete(f.inspects, key)
		}
	}
	if f.inspect.ID == id {
		f.inspect = container.InspectResponse{}
	}
}

func (f *fakeBackupDockerService) recordContainerRunningState(id string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspects == nil {
		f.inspects = make(map[string]container.InspectResponse)
	}
	updated := false
	for key, inspect := range f.inspects {
		if key != id && inspect.ID != id {
			continue
		}
		if inspect.ID == "" {
			inspect.ID = id
		}
		if inspect.State == nil {
			inspect.State = &container.State{}
		}
		inspect.State.Running = running
		f.inspects[key] = inspect
		f.inspects[id] = inspect
		updated = true
	}
	if !updated {
		f.inspects[id] = container.InspectResponse{ID: id, State: &container.State{Running: running}}
	}
}

func (f *fakeBackupDockerService) WaitContainerReady(ctx context.Context, id string, requireHealthy bool) error {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("wait-container:%s:healthy=%v", id, requireHealthy))
	f.mu.Unlock()
	return f.readyErr
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
	for _, want := range []string{"Backup metadata", "Source platform", "Prerequisites", "Checksum verification", "Signature verification", "A dry-run may still produce a plan", "independently verifying the backup package content integrity", "--confirm"} {
		if !strings.Contains(string(readme), want) {
			t.Fatalf("README = %q, want %q", string(readme), want)
		}
	}
	restoreScript, err := os.ReadFile(filepath.Join(dir, backupRestoreName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dm version", "Docker daemon must be reachable", "checksums.txt", "independently verifying backup package content integrity", "without --confirm"} {
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

	fake := &fakeBackupDockerService{
		containerExists: true,
		network:         network.Inspect{Network: network.Network{Name: "demo_net"}},
		volume:          volume.Volume{Name: "demo_data"},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, SkipChecksum: true})
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

	if err := restoreBackup(context.Background(), archive, RestoreOptions{NoStart: true, Confirm: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	if !hasCall(fake.calls, "container-exists:demo") {
		t.Fatalf("calls = %#v, want target preflight", fake.calls)
	}
	assertRestoreCandidateCommitted(t, fake.calls, "demo")
	if hasCallPrefix(fake.calls, "start-container:") {
		t.Fatalf("calls = %#v, start-container should not run with NoStart", fake.calls)
	}
}

func TestRestoreBackupSupportsEncryptedArchive(t *testing.T) {
	root := t.TempDir()
	passFile := filepath.Join(root, "pass.txt")
	if err := os.WriteFile(passFile, []byte("secret-pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "bundle")
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version: 1,
		Containers: []BackupContainerManifest{{
			ContainerName: "demo",
			InspectFile:   backupInspectName,
		}},
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	archive := filepath.Join(root, "bundle.tar.gz.enc")
	if err := createBackupArchiveWithOptions(context.Background(), dir, archive, backupArchiveOptions{Encrypt: true, PassphraseFile: passFile}); err != nil {
		t.Fatalf("createBackupArchiveWithOptions() error = %v", err)
	}
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	if err := restoreBackup(context.Background(), archive, RestoreOptions{NoStart: true, Confirm: true, SkipChecksum: true, PassphraseFile: passFile}); err != nil {
		t.Fatalf("restoreBackup() encrypted archive error = %v", err)
	}
	assertRestoreCandidateCommitted(t, fake.calls, "demo")
}

func TestRestoreBackupSupportsSplitArchive(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bundle")
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version: 1,
		Containers: []BackupContainerManifest{{
			ContainerName: "demo",
			InspectFile:   backupInspectName,
		}},
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	archive := filepath.Join(root, "bundle.tar.gz")
	if err := createBackupArchiveWithOptions(context.Background(), dir, archive, backupArchiveOptions{SplitSize: 128}); err != nil {
		t.Fatalf("createBackupArchiveWithOptions() split error = %v", err)
	}
	if _, err := os.Stat(splitPartPath(archive, 1)); err != nil {
		t.Fatalf("missing first split part: %v", err)
	}
	if _, err := os.Stat(splitPartPath(archive, 2)); err != nil {
		t.Fatalf("missing second split part: %v", err)
	}
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	if err := restoreBackup(context.Background(), splitPartPath(archive, 1), RestoreOptions{NoStart: true, Confirm: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup() split archive error = %v", err)
	}
	assertRestoreCandidateCommitted(t, fake.calls, "demo")
}

func TestBackupCommandEncryptRequiresPassphraseFile(t *testing.T) {
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	cmd := NewBackupCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"demo", "--bundle", "--encrypt", "--output-dir", filepath.Join(t.TempDir(), "backup")})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want passphrase error")
	}
	if !strings.Contains(err.Error(), "--passphrase-file") {
		t.Fatalf("error = %v, want passphrase-file", err)
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

	if err := restoreBackup(context.Background(), root, RestoreOptions{NoStart: true, Confirm: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	for _, want := range []string{
		"container-exists:api",
		"container-exists:worker",
	} {
		if !hasCall(fake.calls, want) {
			t.Fatalf("calls = %#v, want %s", fake.calls, want)
		}
	}
	assertRestoreCandidateCommitted(t, fake.calls, "api")
	assertRestoreCandidateCommitted(t, fake.calls, "worker")
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
	if err := os.WriteFile(filepath.Join(dir, backupInspectName), []byte("{\"Name\":\"/demo\",\"Config\":{\"Image\":\"busybox:latest\"},\"HostConfig\":{}}\n "), 0644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{NoStart: true, Confirm: true})
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
	if err := os.WriteFile(filepath.Join(dir, backupInspectName), []byte("{\"Name\":\"/demo\",\"Config\":{\"Image\":\"busybox:latest\"},\"HostConfig\":{}}\n "), 0644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	if err := restoreBackup(context.Background(), dir, RestoreOptions{NoStart: true, Confirm: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	if !hasCall(fake.calls, "container-exists:demo") {
		t.Fatalf("calls = %#v, want restore to continue when checksum is skipped", fake.calls)
	}
	assertRestoreCandidateCommitted(t, fake.calls, "demo")
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

	fake := &fakeBackupDockerService{
		containerExists: true,
		network:         network.Inspect{Network: network.Network{Name: "demo_net"}},
		volume:          volume.Volume{Name: "demo_data"},
	}
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
		imageMissing:    true,
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

func TestRestoreCommandDryRunJSONPrintsPlanReport(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version: 1,
		Containers: []BackupContainerManifest{{
			ContainerName: "web",
			Image:         "nginx:latest",
			InspectFile:   backupInspectName,
		}},
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		Name:   "/web",
		Config: &container.Config{Image: "nginx:latest"},
	})

	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	cmd := NewRestoreCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{dir, "--dry-run", "--format", "json", "--skip-checksum"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var report RestorePlanReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal() error = %v output=%q", err, out.String())
	}
	if report.Source != dir || report.ContainerCount != 1 || len(report.Containers) != 1 {
		t.Fatalf("report = %#v, want one-container restore plan", report)
	}
	for _, forbidden := range []string{"load-image:", "create-network:", "create-volume:", "remove-container:", "create-container:", "start-container:"} {
		if hasCallPrefix(fake.calls, forbidden) {
			t.Fatalf("calls = %#v, dry-run JSON plan should not call %s", fake.calls, forbidden)
		}
	}
}

func TestRestoreCommandUsesDryRunFormatForPlanOutput(t *testing.T) {
	cmd := NewRestoreCommand()
	if flag := cmd.Flags().Lookup("plan"); flag != nil {
		t.Fatal("restore command should use --dry-run with --format instead of --plan")
	}
	for _, name := range []string{"dry-run", "format"} {
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

	fake := &fakeBackupDockerService{
		containerExists: true,
		network:         network.Inspect{Network: network.Network{Name: "demo_net"}},
		volume:          volume.Volume{Name: "demo_data"},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	var out bytes.Buffer
	if err := restoreBackup(context.Background(), dir, RestoreOptions{Replace: true, Confirm: true, SkipChecksum: true, Output: &out}); err != nil {
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
		"start-container:restored-id",
		"rename-container:restored-id:demo",
	}
	for _, want := range wantCalls {
		if !hasCall(fake.calls, want) {
			t.Fatalf("calls = %#v, want %s", fake.calls, want)
		}
	}
	if !hasCallPrefix(fake.calls, "create-container:demo-dm-restore-candidate-") {
		t.Fatalf("calls = %#v, want candidate container creation", fake.calls)
	}
}

func TestRestoreCommandDefaultsToPlanWithoutConfirm(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	cmd := NewRestoreCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{dir, "--skip-checksum"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if hasCallPrefix(fake.calls, "create-container:") {
		t.Fatalf("calls = %#v, default restore must not mutate Docker", fake.calls)
	}
	for _, want := range []string{"未提供 --confirm", "恢复 dry-run 计划"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output = %q, want %q", output.String(), want)
		}
	}
}

func TestRestoreBackupRejectsMutationWithoutConfirm(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{NoStart: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("restoreBackup() error = %v, want explicit confirmation error", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %#v, unconfirmed restore must not access Docker", fake.calls)
	}
}

func TestRestoreUnsafeHostConfigCanOnlyPreviewUnlessExplicitlyAllowed(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:   "/dangerous",
		Config: &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{
			Privileged:  true,
			NetworkMode: "host",
			PidMode:     "host",
			Binds:       []string{"/etc:/host-etc:ro"},
			CapAdd:      []string{"SYS_ADMIN"},
			SecurityOpt: []string{"seccomp=unconfined"},
			Resources: container.Resources{
				Devices: []container.DeviceMapping{{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rwm"}},
			},
		},
	})
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	var plan bytes.Buffer
	if err := restoreBackup(context.Background(), dir, RestoreOptions{DryRun: true, SkipChecksum: true, Output: &plan}); err != nil {
		t.Fatalf("dry-run error = %v", err)
	}
	for _, want := range []string{"privileged=true", "host network namespace", "host bind mount", "SYS_ADMIN", "seccomp=unconfined", "host device mappings", "默认阻止实际恢复"} {
		if !strings.Contains(plan.String(), want) {
			t.Fatalf("plan = %q, want %q", plan.String(), want)
		}
	}
	if hasCallPrefix(fake.calls, "create-container:") {
		t.Fatalf("calls = %#v, unsafe preview must not mutate Docker", fake.calls)
	}

	fake.calls = nil
	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "unsafe restore configuration") {
		t.Fatalf("restoreBackup() error = %v, want unsafe HostConfig rejection", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %#v, unsafe restore must fail before Docker access", fake.calls)
	}

	if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, AllowUnsafeHostConfig: true, NoStart: true, SkipChecksum: true}); err != nil {
		t.Fatalf("explicitly allowed restore error = %v", err)
	}
	assertRestoreCandidateCommitted(t, fake.calls, "dangerous")
}

func TestBackupSignatureAuthenticatesChecksumFile(t *testing.T) {
	backupDir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/signed",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	if err := writeChecksums(backupDir); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	privatePath := filepath.Join(keyDir, "signing.pem")
	publicPath := filepath.Join(keyDir, "trusted.pem")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0644); err != nil {
		t.Fatal(err)
	}
	if err := signBackupChecksumsWithContext(context.Background(), backupDir, privatePath); err != nil {
		t.Fatalf("signBackupChecksumsWithContext() error = %v", err)
	}
	status, err := verifyBackupSignatureWithContext(context.Background(), backupDir, publicPath)
	if err != nil || !strings.Contains(status, "已验证") {
		t.Fatalf("verify status=%q error=%v", status, err)
	}
	insidePublicKey := filepath.Join(backupDir, "untrusted-public.pem")
	publicPEM, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(insidePublicKey, publicPEM, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBackupSignatureWithContext(context.Background(), backupDir, insidePublicKey); err == nil || !strings.Contains(err.Error(), "outside the backup root") {
		t.Fatalf("verify with in-bundle trust anchor error = %v, want outside-root rejection", err)
	}

	checksumPath := filepath.Join(backupDir, backupChecksumName)
	checksums, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checksumPath, append(checksums, []byte("0  injected\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	err = restoreBackup(context.Background(), backupDir, RestoreOptions{DryRun: true, TrustedPublicKey: publicPath})
	if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("restoreBackup() error = %v, want signature failure", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %#v, invalid signature must fail before Docker access", fake.calls)
	}
}

func TestBackupBundleSigningRejectsPrivateKeyInsideOutput(t *testing.T) {
	fake := &fakeBackupDockerService{
		inspect: container.InspectResponse{
			Name:       "/demo",
			Config:     &container.Config{Image: "busybox:latest"},
			HostConfig: &container.HostConfig{},
		},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	outputDir := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	_, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(outputDir, "do-not-archive.pem")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	_, err = backupContainer(context.Background(), "demo", BackupOptions{
		OutputDir:    outputDir,
		Bundle:       true,
		BundleOutput: archivePath,
		SigningKey:   privatePath,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the backup root") {
		t.Fatalf("backupContainer() error = %v, want signing-key boundary error", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, backupChecksumName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checksums should not be generated around an in-root private key: %v", err)
	}
	if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive should not be generated around an in-root private key: %v", err)
	}
}

func TestRestoreReplaceStagesOldContainerUntilNewContainerStarts(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	fake := runningExistingRestoreService("demo", "old-id")
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	requireCallOrder(t, fake.calls,
		"container-exists:demo",
		"inspect-container:demo",
		"stop-container:old-id",
		"rename-container:old-id:demo-dm-restore-rollback-",
		"create-container:demo",
		"start-container:restored-id",
		"remove-container:old-id",
	)
}

func TestRestoreReplaceNoStartDoesNotStartEitherContainer(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	fake := runningExistingRestoreService("demo", "old-id")
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, NoStart: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	if hasCallPrefix(fake.calls, "start-container:") {
		t.Fatalf("calls = %#v, --no-start must not start the new or staged container after success", fake.calls)
	}
	requireCallOrder(t, fake.calls, "stop-container:old-id", "rename-container:old-id:", "create-container:demo", "remove-container:old-id")
}

func TestRestoreReplaceRollsBackCreateFailure(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	fake := runningExistingRestoreService("demo", "old-id")
	fake.createErr = errors.New("create failed")
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("restoreBackup() error = %v, want create failure", err)
	}
	requireCallOrder(t, fake.calls,
		"stop-container:old-id",
		"rename-container:old-id:demo-dm-restore-rollback-",
		"create-container:demo",
		"rename-container:old-id:demo",
		"start-container:old-id",
	)
	if hasCallPrefix(fake.calls, "remove-container:old-id") {
		t.Fatalf("calls = %#v, original container must not be removed on create failure", fake.calls)
	}
}

func TestRestoreReplaceRollsBackStartFailure(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	fake := runningExistingRestoreService("demo", "old-id")
	fake.startErrors = map[string]error{"restored-id": errors.New("start failed")}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("restoreBackup() error = %v, want start failure", err)
	}
	requireCallOrder(t, fake.calls,
		"start-container:restored-id",
		"remove-container:restored-id",
		"rename-container:old-id:demo",
		"start-container:old-id",
	)
}

func TestRestoreReplaceRetainsHealthyNewContainerOnCleanupFailure(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	fake := runningExistingRestoreService("demo", "old-id")
	fake.removeErrors = map[string]error{"old-id": errors.New("cleanup failed")}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("restoreBackup() error = %v, want cleanup failure", err)
	}
	if !strings.Contains(err.Error(), "healthy new container was retained as demo") {
		t.Fatalf("restoreBackup() error = %v, want retained-new-container guidance", err)
	}
	requireCallOrder(t, fake.calls, "rename-container:restored-id:demo", "remove-container:old-id", "inspect-container:old-id")
	for _, forbidden := range []string{"remove-container:restored-id", "rename-container:old-id:demo", "start-container:old-id"} {
		for _, call := range fake.calls {
			if call == forbidden {
				t.Fatalf("calls = %#v, cleanup uncertainty must retain the healthy new container", fake.calls)
			}
		}
	}
}

func TestRestoreReplaceRollsBackCancellationAfterCreate(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	fake := runningExistingRestoreService("demo", "old-id")
	fake.cancelAfterCreate = cancel
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(ctx, dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("restoreBackup() error = %v, want context.Canceled", err)
	}
	requireCallOrder(t, fake.calls,
		"create-container:demo",
		"remove-container:restored-id",
		"rename-container:old-id:demo",
		"start-container:old-id",
	)
}

func TestSafeExtractPathRejectsTraversal(t *testing.T) {
	if _, err := safeExtractPath(t.TempDir(), "../evil"); err == nil {
		t.Fatal("safeExtractPath() error = nil, want traversal error")
	}
}

func writeRestoreFixture(t *testing.T, inspect container.InspectResponse) string {
	t.Helper()
	dir := t.TempDir()
	name := normalizeContainerName(inspect.Name)
	if name == "" {
		name = "demo"
	}
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version:       1,
		ContainerName: name,
		InspectFile:   backupInspectName,
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), inspect)
	return dir
}

func runningExistingRestoreService(name, id string) *fakeBackupDockerService {
	return &fakeBackupDockerService{
		containerExists: true,
		inspects: map[string]container.InspectResponse{
			name: {ID: id, Name: "/" + name, State: &container.State{Running: true}},
		},
	}
}

func requireCallOrder(t *testing.T, calls []string, prefixes ...string) {
	t.Helper()
	next := 0
	for _, call := range calls {
		if next < len(prefixes) && strings.HasPrefix(call, prefixes[next]) {
			next++
		}
	}
	if next != len(prefixes) {
		t.Fatalf("calls = %#v, did not contain ordered prefixes %#v (stopped at %q)", calls, prefixes, prefixes[next])
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

func assertRestoreCandidateCommitted(t *testing.T, calls []string, target string) {
	t.Helper()
	createPrefix := "create-container:" + target + "-dm-restore-candidate-"
	createIndex := -1
	renameIndex := -1
	for i, call := range calls {
		if createIndex < 0 && strings.HasPrefix(call, createPrefix) {
			createIndex = i
		}
		if strings.HasPrefix(call, "rename-container:") && strings.HasSuffix(call, ":"+target) {
			renameIndex = i
		}
	}
	if createIndex < 0 || renameIndex <= createIndex {
		t.Fatalf("calls = %#v, want candidate create followed by commit rename for %s", calls, target)
	}
	if hasCall(calls, "create-container:"+target) {
		t.Fatalf("calls = %#v, final container name must not be created directly", calls)
	}
}
