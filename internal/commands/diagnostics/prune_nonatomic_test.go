package diagnostics

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
)

func TestPruneCommandExposesNonAtomicDeleteAcknowledgement(t *testing.T) {
	cmd := NewPruneReportCommand()
	if flag := cmd.Flags().Lookup("allow-non-atomic-delete"); flag == nil {
		t.Fatal("prune command missing --allow-non-atomic-delete")
	}
}

func TestRunPruneReportRejectsImageOrVolumeMutationWithoutAcknowledgement(t *testing.T) {
	fake := &fakePruneDockerService{usage: pruneDiskUsage{
		Containers: []*container.Summary{{ID: "old-container", State: "exited"}},
		Images:     []*image.Summary{{ID: "sha256:dangling", RepoTags: []string{"<none>:<none>"}}},
	}}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	report, err := runPruneReport(context.Background(), PruneReportOptions{Apply: true, Confirm: true})
	if err == nil || !strings.Contains(err.Error(), "--allow-non-atomic-delete") {
		t.Fatalf("runPruneReport() error = %v, want acknowledgement requirement", err)
	}
	if report.Applied || report.NonAtomicDeleteAcknowledged {
		t.Fatalf("report = %#v, no mutation should be marked applied", report)
	}
	if len(report.Warnings) == 0 || !strings.Contains(strings.Join(report.Warnings, "\n"), "compare-and-delete") {
		t.Fatalf("warnings = %#v", report.Warnings)
	}
	for _, call := range fake.callList() {
		if strings.HasPrefix(call, "remove-") {
			t.Fatalf("calls = %#v, mutation occurred before acknowledgement", fake.callList())
		}
	}
}

func TestRunPruneReportContainerOnlyDoesNotRequireNonAtomicAcknowledgement(t *testing.T) {
	fake := &fakePruneDockerService{usage: pruneDiskUsage{
		Containers: []*container.Summary{{ID: "old-container", State: "exited"}},
		Images:     []*image.Summary{{ID: "sha256:dangling", RepoTags: []string{"<none>:<none>"}}},
	}}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	report, err := runPruneReport(context.Background(), PruneReportOptions{
		Apply:   true,
		Confirm: true,
		Only:    []string{"container"},
	})
	if err != nil {
		t.Fatalf("runPruneReport() error = %v", err)
	}
	if !report.Applied || len(report.ApplyResult.ContainersDeleted) != 1 {
		t.Fatalf("report = %#v", report)
	}
	if hasPruneCall(fake.callList(), "remove-image:") {
		t.Fatalf("calls = %#v", fake.callList())
	}
}
