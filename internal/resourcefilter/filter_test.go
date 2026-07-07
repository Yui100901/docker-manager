package resourcefilter

import (
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/volume"
)

func TestMatchSupportsKeyedWildcardAndPrefixFilters(t *testing.T) {
	candidates := []string{"api-1", "name:api-1", "state:running", "label:role=api"}

	tests := []string{
		"api-*",
		"name=api",
		"state:RUNNING",
		"label:role=api",
	}
	for _, filter := range tests {
		t.Run(filter, func(t *testing.T) {
			if !Match(candidates, []string{filter}, ContainerKeys...) {
				t.Fatalf("Match(%q) = false, want true", filter)
			}
		})
	}
}

func TestContainerCandidatesSupportLocalResourceFilters(t *testing.T) {
	c := container.Summary{
		ID:      "sha256:abcdef1234567890",
		Names:   []string{"/api-1"},
		Image:   "registry.local/team/api:v1",
		ImageID: "sha256:feedface12345678",
		State:   "running",
		Status:  "Up 1 minute",
		Labels:  map[string]string{"role": "api"},
	}

	for _, filter := range []string{
		"id:abcdef123456",
		"name:api-*",
		"image:team/api",
		"image:api",
		"state:running",
		"status:Up*",
		"label:role=api",
	} {
		t.Run(filter, func(t *testing.T) {
			if !Match(ContainerCandidates(c), []string{filter}, ContainerKeys...) {
				t.Fatalf("container filter %q = false, want true", filter)
			}
		})
	}
}

func TestImageCandidatesSupportRepositoryTagsDigestsAndLabels(t *testing.T) {
	img := image.Summary{
		ID:          "sha256:abcdef1234567890",
		RepoTags:    []string{"registry.local/team/api:v1"},
		RepoDigests: []string{"registry.local/team/api@sha256:deadbeef"},
		Labels:      map[string]string{"role": "api"},
	}

	for _, filter := range []string{
		"id:abcdef123456",
		"image:api",
		"repo:team/api",
		"tag:v1",
		"digest:*deadbeef",
		"label:role=api",
	} {
		t.Run(filter, func(t *testing.T) {
			if !Match(ImageCandidates(img), []string{filter}, ImageKeys...) {
				t.Fatalf("image filter %q = false, want true", filter)
			}
		})
	}
	if Match(ImageCandidates(img), []string{"repo:v1"}, ImageKeys...) {
		t.Fatal("image filter repo:v1 = true, want false")
	}
}

func TestVolumeCandidatesSupportDriverMountpointLabelsAndOptions(t *testing.T) {
	vol := &volume.Volume{
		Name:       "app_data",
		Driver:     "local",
		Mountpoint: "/var/lib/docker/volumes/app_data/_data",
		Scope:      "local",
		Labels:     map[string]string{"app": "demo"},
		Options:    map[string]string{"type": "nfs"},
	}

	for _, filter := range []string{
		"name:app_*",
		"driver:local",
		"mountpoint:*/app_data/*",
		"scope:local",
		"label:app=demo",
		"option:type=nfs",
	} {
		t.Run(filter, func(t *testing.T) {
			if !Match(VolumeCandidates(vol), []string{filter}, VolumeKeys...) {
				t.Fatalf("volume filter %q = false, want true", filter)
			}
		})
	}
}
