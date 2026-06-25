package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
)

type fakeBackupDockerService struct {
	inspect         container.InspectResponse
	network         network.Inspect
	volume          volume.Volume
	containerExists bool
	calls           []string
}

func (f *fakeBackupDockerService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	f.calls = append(f.calls, "inspect-container:"+name)
	return f.inspect, nil
}

func (f *fakeBackupDockerService) SaveImage(ctx context.Context, refs []string, outputFile string) error {
	f.calls = append(f.calls, "save-image:"+strings.Join(refs, ","))
	if err := os.MkdirAll(filepath.Dir(outputFile), 0755); err != nil {
		return err
	}
	return os.WriteFile(outputFile, []byte("image tar"), 0644)
}

func (f *fakeBackupDockerService) LoadImage(ctx context.Context, inputFile string) error {
	f.calls = append(f.calls, "load-image:"+filepath.Base(inputFile))
	return nil
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
	fake := &fakeBackupDockerService{
		inspect: container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				Name:       "/demo",
				HostConfig: &container.HostConfig{},
			},
			Config: &container.Config{Image: "busybox:latest"},
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "demo_data", Destination: "/data"},
			},
			NetworkSettings: &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{
					"demo_net": {},
				},
			},
		},
		network: network.Inspect{Name: "demo_net", Driver: "bridge"},
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
	if manifest.ContainerName != "demo" {
		t.Fatalf("ContainerName = %q, want demo", manifest.ContainerName)
	}
	if manifest.ImageArchive == "" {
		t.Fatal("ImageArchive is empty")
	}
	for _, rel := range []string{
		manifest.InspectFile,
		manifest.ComposeFile,
		manifest.ImageArchive,
		manifest.Networks[0].File,
		manifest.Volumes[0].File,
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("expected backup file %s: %v", rel, err)
		}
	}
	if !hasCall(fake.calls, "save-image:busybox:latest") {
		t.Fatalf("calls = %#v, want save-image", fake.calls)
	}
}

func TestBackupContainerWritesOfflineBundleArtifacts(t *testing.T) {
	fake := &fakeBackupDockerService{
		inspect: container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				Name:       "/demo",
				HostConfig: &container.HostConfig{},
			},
			Config: &container.Config{Image: "busybox:latest"},
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

func TestRestoreBackupRejectsExistingContainerWithoutReplace(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{
		Version:       1,
		ContainerName: "demo",
		InspectFile:   backupInspectName,
		ComposeFile:   backupComposeName,
	})
	writeTestJSON(t, filepath.Join(dir, backupInspectName), container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{Image: "busybox:latest"},
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
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{Image: "busybox:latest"},
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
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{Image: "busybox:latest"},
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
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{Image: "busybox:latest"},
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
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:       "/demo",
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{Image: "busybox:latest"},
	})
	writeTestJSON(t, filepath.Join(dir, filepath.FromSlash(networkFile)), network.Inspect{Name: "demo_net"})
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

	if err := restoreBackup(context.Background(), dir, RestoreOptions{Replace: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
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
