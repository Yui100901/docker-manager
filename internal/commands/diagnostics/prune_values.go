package diagnostics

import (
	"sort"
	"strings"

	"github.com/moby/moby/api/types/image"
)

func sortPruneReport(report *PruneReport) {
	sort.Slice(report.StoppedContainers, func(i, j int) bool {
		return report.StoppedContainers[i].Name < report.StoppedContainers[j].Name
	})
	sort.Slice(report.DanglingImages, func(i, j int) bool {
		return report.DanglingImages[i].ID < report.DanglingImages[j].ID
	})
	sort.Slice(report.UnusedVolumes, func(i, j int) bool {
		return report.UnusedVolumes[i].Name < report.UnusedVolumes[j].Name
	})
	sort.Slice(report.BuildCaches, func(i, j int) bool {
		return report.BuildCaches[i].ID < report.BuildCaches[j].ID
	})
}

func firstContainerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

func isDanglingImage(img *image.Summary) bool {
	if len(img.RepoTags) == 0 {
		return true
	}
	for _, tag := range img.RepoTags {
		if tag != "" && tag != "<none>:<none>" {
			return false
		}
	}
	return true
}

func cleanRepoTags(tags []string) []string {
	var cleaned []string
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		cleaned = append(cleaned, tag)
	}
	return cleaned
}

func imageDeleteRefs(items []image.DeleteResponse) []string {
	refs := make([]string, 0, len(items))
	for _, item := range items {
		if item.Deleted != "" {
			refs = append(refs, item.Deleted)
		}
		if item.Untagged != "" {
			refs = append(refs, item.Untagged)
		}
	}
	return refs
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func addPositiveSize(total *uint64, size int64) {
	if size > 0 {
		*total += uint64(size)
	}
}

func uint64FromInt64(size int64) uint64 {
	if size <= 0 {
		return 0
	}
	return uint64(size)
}
