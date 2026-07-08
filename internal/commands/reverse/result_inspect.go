package reverse

import (
	"context"
	"log"
	"sort"
	"strings"

	"docker-manager/internal/parallel"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

type reverseInspectResult struct {
	info   container.InspectResponse
	parsed ParsedResult
	err    error
	ok     bool
}

func collectReverseResourceNames(info container.InspectResponse, volumeNames, networkNames map[string]bool) {
	for _, name := range reverseNamedVolumeNames(info) {
		volumeNames[name] = true
	}
	for _, name := range reverseNetworkNames(info) {
		networkNames[name] = true
	}
}

func inspectReverseVolumeMetadata(ctx context.Context, names []string) (map[string]volume.Volume, error) {
	meta := map[string]volume.Volume{}
	if len(names) == 0 {
		return meta, nil
	}
	results := make([]volume.Volume, len(names))
	errs := make([]error, len(names))
	ok := make([]bool, len(names))
	parallel.ForEachIndex(ctx, len(names), reverseInspectConcurrency, func(ctx context.Context, i int) {
		result, err := containerManager.InspectVolumeContext(ctx, names[i])
		if err != nil {
			errs[i] = err
			return
		}
		results[i] = result
		ok[i] = true
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, name := range names {
		if errs[i] != nil {
			log.Printf("volume %s inspect failed: %v", name, errs[i])
			continue
		}
		if ok[i] {
			meta[name] = results[i]
		}
	}
	return meta, nil
}

func inspectReverseNetworkMetadata(ctx context.Context, names []string) (map[string]network.Inspect, error) {
	meta := map[string]network.Inspect{}
	if len(names) == 0 {
		return meta, nil
	}
	results := make([]network.Inspect, len(names))
	errs := make([]error, len(names))
	ok := make([]bool, len(names))
	parallel.ForEachIndex(ctx, len(names), reverseInspectConcurrency, func(ctx context.Context, i int) {
		result, err := containerManager.InspectNetworkContext(ctx, names[i])
		if err != nil {
			errs[i] = err
			return
		}
		results[i] = result
		ok[i] = true
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, name := range names {
		if errs[i] != nil {
			log.Printf("network %s inspect failed: %v", name, errs[i])
			continue
		}
		if ok[i] {
			meta[name] = results[i]
		}
	}
	return meta, nil
}

func reverseNamedVolumeNames(info container.InspectResponse) []string {
	seen := map[string]bool{}
	for _, mount := range info.Mounts {
		if string(mount.Type) == "volume" && mount.Name != "" {
			seen[mount.Name] = true
		}
	}
	return sortedBoolMapKeys(seen)
}

func reverseNetworkNames(info container.InspectResponse) []string {
	seen := map[string]bool{}
	if info.NetworkSettings != nil {
		for name := range info.NetworkSettings.Networks {
			if isReverseCustomNetwork(name) {
				seen[name] = true
			}
		}
	}
	if info.HostConfig != nil {
		networkMode := string(info.HostConfig.NetworkMode)
		if isReverseCustomNetwork(networkMode) {
			seen[networkMode] = true
		}
	}
	return sortedBoolMapKeys(seen)
}

func sortedBoolMapKeys(src map[string]bool) []string {
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isReverseCustomNetwork(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	switch name {
	case "default", "bridge", "host", "none":
		return false
	default:
		return !strings.HasPrefix(name, "container:") && !strings.HasPrefix(name, "service:")
	}
}
