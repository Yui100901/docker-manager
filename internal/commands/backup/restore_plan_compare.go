package backup

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

func compareRestoreNetwork(expected, actual network.Inspect) []string {
	var diffs []string
	if expected.Driver != actual.Driver {
		diffs = append(diffs, fmt.Sprintf("driver: backup=%s current=%s", expected.Driver, actual.Driver))
	}
	if expected.Internal != actual.Internal {
		diffs = append(diffs, fmt.Sprintf("internal: backup=%v current=%v", expected.Internal, actual.Internal))
	}
	if expected.Attachable != actual.Attachable {
		diffs = append(diffs, fmt.Sprintf("attachable: backup=%v current=%v", expected.Attachable, actual.Attachable))
	}
	if expected.EnableIPv6 != actual.EnableIPv6 {
		diffs = append(diffs, fmt.Sprintf("enable_ipv6: backup=%v current=%v", expected.EnableIPv6, actual.EnableIPv6))
	}
	if !jsonEqual(expected.IPAM, actual.IPAM) {
		diffs = append(diffs, "ipam differs")
	}
	if !reflect.DeepEqual(expected.Options, actual.Options) {
		diffs = append(diffs, "options differ")
	}
	if !reflect.DeepEqual(expected.Labels, actual.Labels) {
		diffs = append(diffs, "labels differ")
	}
	return diffs
}

func compareRestoreVolume(expected, actual volume.Volume) []string {
	var diffs []string
	if expected.Driver != actual.Driver {
		diffs = append(diffs, fmt.Sprintf("driver: backup=%s current=%s", expected.Driver, actual.Driver))
	}
	if !reflect.DeepEqual(expected.Options, actual.Options) {
		diffs = append(diffs, "options differ")
	}
	if !reflect.DeepEqual(expected.Labels, actual.Labels) {
		diffs = append(diffs, "labels differ")
	}
	return diffs
}

func jsonEqual(a, b interface{}) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func restoreShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func addRestorePlanSummary(summary *RestorePlanSummary, plan RestoreContainerPlan) {
	switch plan.Image.Action {
	case "load-archive":
		summary.ImagesToLoad++
	case "reuse":
		summary.ImagesPresent++
	}
	for _, net := range plan.Networks {
		if net.Exists {
			summary.NetworksPresent++
		} else {
			summary.NetworksToCreate++
		}
		if net.Different {
			summary.NetworksDifferent++
		}
	}
	for _, vol := range plan.Volumes {
		if vol.Exists {
			summary.VolumesPresent++
		} else {
			summary.VolumesToCreate++
		}
		if vol.Different {
			summary.VolumesDifferent++
		}
	}
	switch plan.Container.Action {
	case "replace":
		summary.ContainersToReplace++
	case "conflict":
		summary.ContainerConflicts++
	default:
		summary.ContainersToCreate++
	}
	summary.PortConflicts += len(plan.PortConflicts)
}
