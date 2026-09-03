package backup

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

const (
	restoreNetworkEnableIPv4Option = "com.docker.network.enable_ipv4"
	restoreNetworkEnableIPv6Option = "com.docker.network.enable_ipv6"
)

var restoreContainerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]+$`)

func validateRestoreManifestArtifacts(backupDir string, manifest BackupManifest, opts RestoreOptions) error {
	seenTargets := make(map[string]struct{}, len(manifest.Containers))
	for _, entry := range manifest.Containers {
		entryDir, err := restoreEntryDir(backupDir, entry)
		if err != nil {
			return err
		}
		inspect, err := readContainerInspect(entryDir, entry)
		if err != nil {
			return err
		}
		if inspect.Config == nil {
			return fmt.Errorf("container %s inspect does not contain Config", entry.ContainerName)
		}
		if strings.TrimSpace(inspect.Config.Image) == "" {
			return fmt.Errorf("container %s inspect does not contain an image reference", entry.ContainerName)
		}
		if entry.Image != "" && entry.Image != inspect.Config.Image {
			return fmt.Errorf("container %s image metadata mismatch: manifest=%q inspect=%q", entry.ContainerName, entry.Image, inspect.Config.Image)
		}
		if err := validateRestoreNetworkReferences(entry, inspect); err != nil {
			return err
		}
		if err := validateRestoreVolumeReferences(entry, inspect); err != nil {
			return err
		}
		targetName, err := restoreEntryTargetName(entry, inspect, opts)
		if err != nil {
			return err
		}
		if _, exists := seenTargets[targetName]; exists {
			return fmt.Errorf("restore manifest contains duplicate target container %s", targetName)
		}
		seenTargets[targetName] = struct{}{}

		if entry.ImageArchive != "" {
			imagePath, err := backupFilePath(entryDir, entry.ImageArchive)
			if err != nil {
				return err
			}
			info, err := os.Stat(imagePath)
			if err != nil {
				return fmt.Errorf("image archive %s: %w", entry.ImageArchive, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("image archive is not a regular file: %s", entry.ImageArchive)
			}
		}
		risks, err := unsafeRestoreEntryConfig(entryDir, entry, inspect)
		if err != nil {
			return err
		}
		if !opts.DryRun && len(risks) > 0 && !opts.AllowUnsafeHostConfig {
			return fmt.Errorf("container %s contains unsafe restore configuration: %s; inspect the dry-run plan and use --allow-dangerous-config only for a trusted backup", targetName, strings.Join(risks, "; "))
		}
	}
	return nil
}

func validateRestoreNetworkReferences(entry BackupContainerManifest, inspect container.InspectResponse) error {
	packaged := make(map[string]struct{}, len(entry.Networks))
	for _, ref := range entry.Networks {
		packaged[ref.Name] = struct{}{}
	}
	var referenced []string
	if inspect.NetworkSettings != nil {
		for name := range inspect.NetworkSettings.Networks {
			if !isBuiltinNetwork(name) {
				referenced = append(referenced, name)
			}
		}
	}
	if inspect.HostConfig != nil {
		mode := string(inspect.HostConfig.NetworkMode)
		if isBackupCustomNetwork(mode) {
			referenced = append(referenced, mode)
		}
	}
	for _, name := range referenced {
		if _, exists := packaged[name]; !exists {
			return fmt.Errorf("container %s references network %q that is missing from the restore manifest", entry.ContainerName, name)
		}
	}
	return nil
}

func validateRestoreVolumeReferences(entry BackupContainerManifest, inspect container.InspectResponse) error {
	packaged := make(map[string]struct{}, len(entry.Volumes))
	for _, ref := range entry.Volumes {
		if strings.TrimSpace(ref.Name) == "" {
			return fmt.Errorf("container %s restore manifest contains an empty volume name", entry.ContainerName)
		}
		packaged[ref.Name] = struct{}{}
	}
	if inspect.HostConfig == nil {
		return nil
	}
	referenced := make(map[string]struct{})
	for _, bind := range inspect.HostConfig.Binds {
		source := strings.TrimSpace(bindSource(bind))
		if source != "" && !isHostBindSpec(bind) {
			referenced[source] = struct{}{}
		}
	}
	for _, spec := range inspect.HostConfig.Mounts {
		if spec.Type == mount.TypeVolume && strings.TrimSpace(spec.Source) != "" {
			referenced[spec.Source] = struct{}{}
		}
	}
	var missing []string
	for name := range referenced {
		if _, exists := packaged[name]; !exists {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("container %s references named volume(s) missing from the restore manifest: %s", entry.ContainerName, strings.Join(missing, ", "))
	}
	return nil
}

func unsafeRestoreEntryConfig(entryDir string, entry BackupContainerManifest, inspect container.InspectResponse) ([]string, error) {
	var risks []string
	for _, risk := range unsafeRestoreHostConfig(inspect) {
		risks = append(risks, "HostConfig: "+risk)
	}
	for _, ref := range entry.Networks {
		metadata, err := readNetworkInspect(entryDir, ref)
		if err != nil {
			return nil, err
		}
		if ref.Name == "" || metadata.Name != ref.Name {
			return nil, fmt.Errorf("network metadata name mismatch: manifest=%q metadata=%q", ref.Name, metadata.Name)
		}
		risks = append(risks, unsafeRestoreNetworkConfig(metadata)...)
	}
	for _, ref := range entry.Volumes {
		metadata, err := readVolumeInspect(entryDir, ref)
		if err != nil {
			return nil, err
		}
		if ref.Name == "" || metadata.Name != ref.Name {
			return nil, fmt.Errorf("volume metadata name mismatch: manifest=%q metadata=%q", ref.Name, metadata.Name)
		}
		risks = append(risks, unsafeRestoreVolumeConfig(metadata)...)
	}
	sort.Strings(risks)
	return risks, nil
}

func restoreEntryDir(backupDir string, entry BackupContainerManifest) (string, error) {
	if entry.Path == "" {
		return backupDir, nil
	}
	return backupFilePath(backupDir, entry.Path)
}

func restoreEntryTargetName(entry BackupContainerManifest, inspect container.InspectResponse, opts RestoreOptions) (string, error) {
	targetName := opts.Name
	if targetName == "" {
		targetName = entry.ContainerName
	}
	if targetName == "" {
		targetName = normalizeContainerName(inspect.Name)
	}
	targetName = normalizeContainerName(targetName)
	if targetName == "" {
		return "", fmt.Errorf("backup does not contain a container name; use --name")
	}
	if !restoreContainerNamePattern.MatchString(targetName) {
		return "", fmt.Errorf("invalid restore container name %q", targetName)
	}
	return targetName, nil
}

func preflightRestoreDockerTargets(ctx context.Context, svc backupDockerService, backupDir string, manifest BackupManifest, opts RestoreOptions) ([]string, map[string]string, error) {
	targets := make([]string, 0, len(manifest.Containers))
	existingTargetIDs := make(map[string]string, len(manifest.Containers))
	for _, entry := range manifest.Containers {
		if err := checkBackupContext(ctx); err != nil {
			return targets, existingTargetIDs, err
		}
		entryDir, err := restoreEntryDir(backupDir, entry)
		if err != nil {
			return targets, existingTargetIDs, err
		}
		inspect, err := readContainerInspect(entryDir, entry)
		if err != nil {
			return targets, existingTargetIDs, err
		}
		targetName, err := restoreEntryTargetName(entry, inspect, opts)
		if err != nil {
			return targets, existingTargetIDs, err
		}
		exists, err := svc.ContainerExists(ctx, targetName)
		if err != nil {
			return targets, existingTargetIDs, err
		}
		if exists && !opts.Replace {
			return targets, existingTargetIDs, fmt.Errorf("container %s already exists; use --replace to overwrite", targetName)
		}
		if exists && opts.Replace {
			previous, err := svc.InspectContainer(ctx, targetName)
			if err != nil {
				return targets, existingTargetIDs, fmt.Errorf("inspect existing container %s before replace: %w", targetName, err)
			}
			if previous.HostConfig != nil && previous.HostConfig.AutoRemove {
				return targets, existingTargetIDs, fmt.Errorf("cannot safely replace auto-remove container %s because stopping it would delete the rollback copy", targetName)
			}
			existingTargetIDs[targetName] = restoreContainerIdentity(previous, targetName)
		} else {
			existingTargetIDs[targetName] = ""
		}
		if entry.ImageArchive == "" {
			imageRef := inspect.Config.Image
			if imageRef == "" {
				return targets, existingTargetIDs, fmt.Errorf("container %s has no image reference or image archive", targetName)
			}
			imageExists, err := svc.ImageExists(ctx, imageRef)
			if err != nil {
				return targets, existingTargetIDs, fmt.Errorf("inspect image %s: %w", imageRef, err)
			}
			if !imageExists {
				return targets, existingTargetIDs, fmt.Errorf("image %s required by container %s is missing and the backup has no image archive", imageRef, targetName)
			}
		}
		targets = append(targets, targetName)
	}
	return targets, existingTargetIDs, nil
}

func restoreContainerIdentity(inspect container.InspectResponse, fallback string) string {
	if inspect.ID != "" {
		return inspect.ID
	}
	return fallback
}

type preparedRestoreBackup struct {
	source             string
	dir                string
	cleanup            func()
	signatureStatus    string
	manifest           BackupManifest
	svc                backupDockerService
	targets            []string
	existingTargets    map[string]string
	itemBudgetReserved bool
}

func (prepared *preparedRestoreBackup) Close() {
	if prepared != nil && prepared.cleanup != nil {
		prepared.cleanup()
		prepared.cleanup = nil
	}
}

func prepareRestoreBackup(ctx context.Context, source string, opts RestoreOptions) (prepared *preparedRestoreBackup, resultErr error) {
	limits, err := resolveRestoreLimits(opts)
	if err != nil {
		return nil, err
	}
	resolvedDir, cleanup, err := resolveRestoreBackupDirWithOptions(ctx, source, opts)
	if err != nil {
		return nil, err
	}
	prepared = &preparedRestoreBackup{source: source, dir: resolvedDir, cleanup: cleanup}
	defer func(owned *preparedRestoreBackup) {
		if resultErr != nil {
			owned.Close()
		}
	}(prepared)
	prepared.signatureStatus, err = verifyBackupSignatureWithContext(ctx, resolvedDir, opts.TrustedPublicKey)
	if err != nil {
		return nil, fmt.Errorf("verify backup signature: %w", err)
	}
	if !opts.SkipChecksum {
		verified, err := verifyBackupChecksumsWithContext(ctx, resolvedDir)
		if err != nil {
			return nil, fmt.Errorf("verify checksums: %w", err)
		}
		if !verified && !opts.DryRun {
			return nil, fmt.Errorf("checksums.txt is required for confirmed restore; use --skip-checksum only after independently verifying the source")
		}
		if verified {
			log.Printf("Checksum verification passed: %s", resolvedDir)
		}
	} else {
		log.Printf("Skip checksum verification: %s", resolvedDir)
	}
	prepared.manifest, err = readBackupManifestWithLimit(resolvedDir, limits.jsonBytes)
	if err != nil {
		return nil, err
	}
	if err := validateRestoreJSONBudgetWithLimit(resolvedDir, prepared.manifest, limits.jsonBytes); err != nil {
		return nil, err
	}
	if len(prepared.manifest.Containers) == 0 {
		return nil, fmt.Errorf("manifest does not contain any containers")
	}
	if opts.Name != "" && len(prepared.manifest.Containers) != 1 {
		return nil, fmt.Errorf("--name 只支持恢复单个备份")
	}
	if err := reserveRestoreManifestItems(ctx, prepared.manifest, opts.itemBudget); err != nil {
		return nil, err
	}
	prepared.itemBudgetReserved = true
	if err := validateRestoreManifestArtifacts(resolvedDir, prepared.manifest, opts); err != nil {
		return nil, err
	}
	prepared.svc, err = newBackupDockerService()
	if err != nil {
		return nil, err
	}
	if !opts.DryRun {
		prepared.targets, prepared.existingTargets, err = preflightRestoreDockerTargets(ctx, prepared.svc, resolvedDir, prepared.manifest, opts)
		if err != nil {
			return nil, err
		}
	}
	return prepared, nil
}

type restoreNetworkFingerprint struct {
	Name       string
	Driver     string
	Scope      string
	EnableIPv4 bool
	EnableIPv6 bool
	IPAM       network.IPAM
	Internal   bool
	Attachable bool
	Ingress    bool
	ConfigOnly bool
	ConfigFrom string
	Options    map[string]string
	Labels     map[string]string
}

type restoreVolumeFingerprint struct {
	Name    string
	Driver  string
	Options map[string]string
	Labels  map[string]string
}

func validatePreparedRestoreSet(ctx context.Context, preparedBackups []*preparedRestoreBackup, opts RestoreOptions) error {
	if len(preparedBackups) == 0 {
		return nil
	}
	existingPorts, err := activeRestorePortBindings(ctx, preparedBackups[0].svc)
	if err != nil {
		return err
	}
	var plannedPorts []restoreHostPortBinding
	networks := make(map[string]restoreNetworkFingerprint)
	volumes := make(map[string]restoreVolumeFingerprint)

	for _, prepared := range preparedBackups {
		for _, entry := range prepared.manifest.Containers {
			entryDir, err := restoreEntryDir(prepared.dir, entry)
			if err != nil {
				return err
			}
			inspect, err := readContainerInspect(entryDir, entry)
			if err != nil {
				return err
			}
			targetName, err := restoreEntryTargetName(entry, inspect, opts)
			if err != nil {
				return err
			}
			for _, binding := range restoreContainerHostPortBindings(inspect, targetName) {
				for _, previous := range plannedPorts {
					if previous.owner != targetName && restoreHostPortsConflict(binding, previous) {
						return fmt.Errorf("host port %s is requested by both %s and %s", binding.String(), previous.owner, targetName)
					}
				}
				for _, existing := range existingPorts {
					if restoreHostPortsConflict(binding, existing) && !(opts.Replace && existing.owner == targetName) {
						return fmt.Errorf("host port %s requested by %s conflicts with %s owned by %s", binding.String(), targetName, existing.String(), existing.owner)
					}
				}
				plannedPorts = append(plannedPorts, binding)
			}
			for _, ref := range entry.Networks {
				expected, err := readNetworkInspect(entryDir, ref)
				if err != nil {
					return err
				}
				fingerprint := newRestoreNetworkFingerprint(expected)
				if previous, exists := networks[ref.Name]; exists && !restoreNetworkFingerprintsEqual(previous, fingerprint) {
					return fmt.Errorf("network %s has conflicting definitions across restore inputs", ref.Name)
				}
				networks[ref.Name] = fingerprint
				if isBuiltinNetwork(ref.Name) {
					continue
				}
				actual, err := prepared.svc.InspectNetwork(ctx, ref.Name)
				if err != nil {
					if cerrdefs.IsNotFound(err) {
						continue
					}
					return fmt.Errorf("inspect existing network %s: %w", ref.Name, err)
				}
				if !restoreNetworkFingerprintsEqual(fingerprint, newRestoreNetworkFingerprint(actual)) {
					return fmt.Errorf("existing network %s differs from the restore definition", ref.Name)
				}
			}
			for _, ref := range entry.Volumes {
				expected, err := readVolumeInspect(entryDir, ref)
				if err != nil {
					return err
				}
				fingerprint := newRestoreVolumeFingerprint(expected)
				if previous, exists := volumes[ref.Name]; exists && !restoreVolumeFingerprintsEqual(previous, fingerprint) {
					return fmt.Errorf("volume %s has conflicting definitions across restore inputs", ref.Name)
				}
				volumes[ref.Name] = fingerprint
				actual, err := prepared.svc.InspectVolume(ctx, ref.Name)
				if err != nil {
					if cerrdefs.IsNotFound(err) {
						continue
					}
					return fmt.Errorf("inspect existing volume %s: %w", ref.Name, err)
				}
				if !restoreVolumeFingerprintsEqual(fingerprint, newRestoreVolumeFingerprint(actual)) {
					return fmt.Errorf("existing volume %s differs from the restore definition", ref.Name)
				}
			}
		}
	}
	return nil
}

func newRestoreNetworkFingerprint(value network.Inspect) restoreNetworkFingerprint {
	enableIPv4, enableIPv6, options := normalizeRestoreNetworkOptions(value)
	return restoreNetworkFingerprint{
		Name:       value.Name,
		Driver:     strings.TrimSpace(value.Driver),
		Scope:      strings.TrimSpace(value.Scope),
		EnableIPv4: enableIPv4,
		EnableIPv6: enableIPv6,
		IPAM:       normalizeRestoreIPAM(value.IPAM),
		Internal:   value.Internal,
		Attachable: value.Attachable,
		Ingress:    value.Ingress,
		ConfigOnly: value.ConfigOnly,
		ConfigFrom: value.ConfigFrom.Network,
		Options:    options,
		Labels:     normalizeRestoreStringMap(value.Labels),
	}
}

func newRestoreVolumeFingerprint(value volume.Volume) restoreVolumeFingerprint {
	driver := strings.TrimSpace(value.Driver)
	if driver == "" {
		driver = "local"
	}
	return restoreVolumeFingerprint{
		Name:    value.Name,
		Driver:  driver,
		Options: normalizeRestoreStringMap(value.Options),
		Labels:  normalizeRestoreStringMap(value.Labels),
	}
}

func restoreNetworkFingerprintsEqual(left, right restoreNetworkFingerprint) bool {
	return jsonEqual(left, right)
}

func restoreVolumeFingerprintsEqual(left, right restoreVolumeFingerprint) bool {
	return jsonEqual(left, right)
}

func restoredNetworkMatchesCreateRequest(expected, actual network.Inspect) bool {
	want := newRestoreNetworkFingerprint(expected)
	got := newRestoreNetworkFingerprint(actual)

	// Blank create fields ask the daemon to choose defaults. Ignore only the
	// corresponding daemon-populated values; explicitly requested values remain
	// strict so this check still catches a create that committed differently.
	if strings.TrimSpace(expected.Driver) == "" {
		want.Driver = got.Driver
	}
	if strings.TrimSpace(expected.Scope) == "" {
		want.Scope = got.Scope
	}
	if len(expected.IPAM.Config) == 0 {
		want.IPAM.Config = got.IPAM.Config
	}
	if strings.TrimSpace(expected.IPAM.Driver) == "" {
		want.IPAM.Driver = got.IPAM.Driver
	}
	if !restoreRequestedStringMapMatches(want.IPAM.Options, got.IPAM.Options) {
		return false
	}
	want.IPAM.Options = got.IPAM.Options
	if !restoreRequestedStringMapMatches(want.Options, got.Options) {
		return false
	}
	want.Options = got.Options
	if !restoreRequestedStringMapMatches(expected.Labels, actual.Labels) {
		return false
	}
	want.Labels = got.Labels
	return restoreNetworkFingerprintsEqual(want, got)
}

func restoredVolumeMatchesCreateRequest(expected, actual volume.Volume) bool {
	want := newRestoreVolumeFingerprint(expected)
	got := newRestoreVolumeFingerprint(actual)
	if !restoreRequestedStringMapMatches(expected.Options, actual.Options) {
		return false
	}
	want.Options = got.Options
	if !restoreRequestedStringMapMatches(expected.Labels, actual.Labels) {
		return false
	}
	want.Labels = got.Labels
	return restoreVolumeFingerprintsEqual(want, got)
}

func restoreRequestedStringMapMatches(expected, actual map[string]string) bool {
	for key, expectedValue := range expected {
		if actualValue, exists := actual[key]; !exists || actualValue != expectedValue {
			return false
		}
	}
	return true
}

func normalizeRestoreIPAM(value network.IPAM) network.IPAM {
	driver := strings.TrimSpace(value.Driver)
	if driver == "" {
		driver = "default"
	}
	result := network.IPAM{
		Driver:  driver,
		Options: normalizeRestoreStringMap(value.Options),
	}
	if len(value.Config) == 0 {
		return result
	}
	result.Config = make([]network.IPAMConfig, 0, len(value.Config))
	for _, config := range value.Config {
		config.Subnet = normalizeRestorePrefix(config.Subnet)
		config.IPRange = normalizeRestorePrefix(config.IPRange)
		config.Gateway = normalizeRestoreAddress(config.Gateway)
		if len(config.AuxAddress) == 0 {
			config.AuxAddress = nil
		} else {
			aux := make(map[string]netip.Addr, len(config.AuxAddress))
			for name, address := range config.AuxAddress {
				aux[name] = normalizeRestoreAddress(address)
			}
			config.AuxAddress = aux
		}
		result.Config = append(result.Config, config)
	}
	sort.Slice(result.Config, func(i, j int) bool {
		return restoreIPAMConfigSortKey(result.Config[i]) < restoreIPAMConfigSortKey(result.Config[j])
	})
	return result
}

func restoreNetworkCreateIPAM(value network.IPAM) network.IPAM {
	result := normalizeRestoreIPAM(value)
	if strings.TrimSpace(value.Driver) == "" {
		result.Driver = ""
	}
	return result
}

func normalizeRestoreNetworkOptions(value network.Inspect) (bool, bool, map[string]string) {
	options := normalizeRestoreStringMap(value.Options)
	enableIPv4 := value.EnableIPv4
	enableIPv6 := value.EnableIPv6
	ipv4Known := value.EnableIPv4
	ipv6Known := value.EnableIPv6
	if raw, exists := options[restoreNetworkEnableIPv4Option]; exists {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			if !value.EnableIPv4 {
				enableIPv4 = parsed
			}
			ipv4Known = true
			delete(options, restoreNetworkEnableIPv4Option)
		}
	}
	if raw, exists := options[restoreNetworkEnableIPv6Option]; exists {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			if !value.EnableIPv6 {
				enableIPv6 = parsed
			}
			ipv6Known = true
			delete(options, restoreNetworkEnableIPv6Option)
		}
	}
	if !ipv4Known {
		enableIPv4 = restoreIPAMHasIPv4(value.IPAM)
	}
	if !ipv6Known {
		enableIPv6 = restoreIPAMHasIPv6(value.IPAM)
	}
	if len(options) == 0 {
		options = nil
	}
	return enableIPv4, enableIPv6, options
}

func restoreIPAMHasIPv4(value network.IPAM) bool {
	for _, config := range value.Config {
		if (config.Subnet.IsValid() && config.Subnet.Addr().Unmap().Is4()) ||
			(config.IPRange.IsValid() && config.IPRange.Addr().Unmap().Is4()) ||
			(config.Gateway.IsValid() && config.Gateway.Unmap().Is4()) {
			return true
		}
		for _, address := range config.AuxAddress {
			if address.IsValid() && address.Unmap().Is4() {
				return true
			}
		}
	}
	return false
}

func restoreIPAMHasIPv6(value network.IPAM) bool {
	for _, config := range value.Config {
		if (config.Subnet.IsValid() && config.Subnet.Addr().Is6()) ||
			(config.IPRange.IsValid() && config.IPRange.Addr().Is6()) ||
			(config.Gateway.IsValid() && config.Gateway.Is6()) {
			return true
		}
		for _, address := range config.AuxAddress {
			if address.IsValid() && address.Is6() {
				return true
			}
		}
	}
	return false
}

func normalizeRestorePrefix(value netip.Prefix) netip.Prefix {
	if !value.IsValid() {
		return netip.Prefix{}
	}
	return value.Masked()
}

func normalizeRestoreAddress(value netip.Addr) netip.Addr {
	if !value.IsValid() {
		return netip.Addr{}
	}
	return value.Unmap()
}

func normalizeRestoreStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func restoreIPAMConfigSortKey(config network.IPAMConfig) string {
	parts := []string{strconv.Itoa(restoreIPAMConfigFamily(config)), config.Subnet.String(), config.IPRange.String(), config.Gateway.String()}
	keys := make([]string, 0, len(config.AuxAddress))
	for key := range config.AuxAddress {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, key, config.AuxAddress[key].String())
	}
	return strings.Join(parts, "\x00")
}

func restoreIPAMConfigFamily(config network.IPAMConfig) int {
	for _, prefix := range []netip.Prefix{config.Subnet, config.IPRange} {
		if prefix.IsValid() {
			if prefix.Addr().Unmap().Is4() {
				return 4
			}
			return 6
		}
	}
	if config.Gateway.IsValid() {
		if config.Gateway.Unmap().Is4() {
			return 4
		}
		return 6
	}
	return 0
}

type restoreHostPortBinding struct {
	hostIP   netip.Addr
	hostPort string
	protocol string
	owner    string
}

func (binding restoreHostPortBinding) String() string {
	return fmt.Sprintf("%s:%s/%s", binding.hostIP, binding.hostPort, binding.protocol)
}

func restoreContainerHostPortBindings(inspect container.InspectResponse, owner string) []restoreHostPortBinding {
	if inspect.HostConfig == nil {
		return nil
	}
	var result []restoreHostPortBinding
	for port, bindings := range inspect.HostConfig.PortBindings {
		for _, binding := range bindings {
			if binding.HostPort == "" {
				continue
			}
			hostIP := binding.HostIP
			if !hostIP.IsValid() {
				hostIP = netip.IPv4Unspecified()
			}
			result = append(result, restoreHostPortBinding{hostIP: hostIP.Unmap(), hostPort: binding.HostPort, protocol: string(port.Proto()), owner: owner})
		}
	}
	return result
}

func restoreHostPortsConflict(left, right restoreHostPortBinding) bool {
	if left.hostPort != right.hostPort || left.protocol != right.protocol {
		return false
	}
	if left.hostIP == right.hostIP {
		return true
	}
	if left.hostIP.BitLen() != right.hostIP.BitLen() {
		return false
	}
	return left.hostIP.IsUnspecified() || right.hostIP.IsUnspecified()
}

func activeRestorePortBindings(ctx context.Context, svc backupDockerService) ([]restoreHostPortBinding, error) {
	containers, err := svc.ListContainers(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("list containers for host port preflight: %w", err)
	}
	var result []restoreHostPortBinding
	for _, summary := range containers {
		ref := summary.ID
		if ref == "" {
			ref = firstContainerName(summary.Names)
		}
		if ref == "" {
			continue
		}
		inspect, err := svc.InspectContainer(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("inspect container %s for host port preflight: %w", ref, err)
		}
		running := inspect.State != nil && inspect.State.Running
		if inspect.State == nil {
			state := strings.ToLower(string(summary.State))
			running = state == "running" || state == "paused" || state == "restarting"
		}
		if !running {
			continue
		}
		owner := normalizeContainerName(inspect.Name)
		if owner == "" {
			owner = firstContainerName(summary.Names)
		}
		result = append(result, restoreContainerHostPortBindings(inspect, owner)...)
	}
	return result, nil
}
