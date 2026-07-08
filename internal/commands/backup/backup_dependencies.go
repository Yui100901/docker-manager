package backup

import (
	"os"
	"sort"

	"docker-manager/internal/docker"

	"github.com/moby/moby/api/types/container"
)

func backupMountRefs(inspect container.InspectResponse) []BackupMountRef {
	refs := make([]BackupMountRef, 0, len(inspect.Mounts))
	for _, m := range inspect.Mounts {
		ref := BackupMountRef{
			Type:        string(m.Type),
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			Driver:      m.Driver,
			Mode:        m.Mode,
			RW:          m.RW,
			Propagation: string(m.Propagation),
		}
		annotateBackupMountRef(&ref)
		refs = append(refs, ref)
	}
	if hostConfig := backupHostConfig(inspect); hostConfig != nil {
		for destination := range hostConfig.Tmpfs {
			if backupMountRefExists(refs, "tmpfs", destination) {
				continue
			}
			refs = append(refs, BackupMountRef{
				Type:         "tmpfs",
				Destination:  destination,
				Verification: "not-applicable",
			})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Type == refs[j].Type {
			return refs[i].Destination < refs[j].Destination
		}
		return refs[i].Type < refs[j].Type
	})
	return refs
}

func backupHostConfig(inspect container.InspectResponse) *container.HostConfig {
	return inspect.HostConfig
}

func backupMountRefExists(refs []BackupMountRef, mountType, destination string) bool {
	for _, ref := range refs {
		if ref.Type == mountType && ref.Destination == destination {
			return true
		}
	}
	return false
}

func annotateBackupMountRef(ref *BackupMountRef) {
	switch ref.Type {
	case "bind":
		annotateHostPath(&ref.Verification, &ref.Warning, ref.Source, &ref.HostPathExists, &ref.HostPathReadable, &ref.HostPathWritable)
	case "npipe":
		annotateNamedPipePath(ref)
	default:
		ref.Verification = "not-applicable"
	}
}

func annotateNamedPipePath(ref *BackupMountRef) {
	if ref.Source == "" {
		ref.Verification = "missing-source"
		ref.Warning = "named pipe source is empty"
		return
	}
	if docker.IsRemoteEndpoint() {
		ref.Verification = "unverified-remote"
		ref.Warning = "named pipe belongs to the Docker daemon host and cannot be verified from this client"
		return
	}
	ref.Verification = "unverified-local"
	ref.Warning = "named pipe source cannot be verified with filesystem checks"
}

func backupDeviceRefs(inspect container.InspectResponse) []BackupDeviceRef {
	hostConfig := backupHostConfig(inspect)
	if hostConfig == nil || len(hostConfig.Devices) == 0 {
		return nil
	}
	refs := make([]BackupDeviceRef, 0, len(hostConfig.Devices))
	for _, device := range hostConfig.Devices {
		ref := BackupDeviceRef{
			Type:              "device",
			PathOnHost:        device.PathOnHost,
			PathInContainer:   device.PathInContainer,
			CgroupPermissions: device.CgroupPermissions,
		}
		annotateHostPath(&ref.Verification, &ref.Warning, ref.PathOnHost, &ref.HostPathExists, nil, nil)
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].PathInContainer < refs[j].PathInContainer
	})
	return refs
}

func annotateHostPath(verification *string, warning *string, path string, exists **bool, readable **bool, writable **bool) {
	if path == "" {
		*verification = "missing-source"
		*warning = "host path source is empty"
		setBoolPtr(exists, false)
		return
	}
	if docker.IsRemoteEndpoint() {
		*verification = "unverified-remote"
		*warning = "host path belongs to the Docker daemon host and cannot be verified from this client"
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		*verification = "missing"
		*warning = err.Error()
		setBoolPtr(exists, false)
		return
	}
	_ = info
	*verification = "verified-local"
	setBoolPtr(exists, true)
	if readable != nil {
		setBoolPtr(readable, canReadPath(path))
	}
	if writable != nil {
		setBoolPtr(writable, canWritePath(path))
	}
}

func setBoolPtr(target **bool, value bool) {
	if target == nil {
		return
	}
	v := value
	*target = &v
}

func canReadPath(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}

func canWritePath(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		file, err := os.CreateTemp(path, ".dm-write-check-*")
		if err != nil {
			return false
		}
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
		return true
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}
