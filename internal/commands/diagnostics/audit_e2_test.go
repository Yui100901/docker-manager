package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"docker-manager/internal/audit"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/volume"
)

type volumeAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (sink *volumeAuditSink) Append(_ context.Context, event audit.Event) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.err != nil {
		return sink.err
	}
	sink.events = append(sink.events, event)
	return nil
}

func (sink *volumeAuditSink) snapshot() []audit.Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]audit.Event(nil), sink.events...)
}

func newVolumeAuditSession(t *testing.T, sink audit.Sink, policy audit.FailurePolicy) *audit.Session {
	t.Helper()
	session, err := audit.NewSession(audit.SessionOptions{
		Sink:          sink,
		FailurePolicy: policy,
		Operation:     "volumes",
		Command:       "dm volumes",
		Endpoint:      "unix:///var/run/docker.sock",
		IdentifierKey: bytes.Repeat([]byte{0x51}, 32),
		Random:        bytes.NewReader(bytes.Repeat([]byte{0x52}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

type countingVolumeService struct {
	volumes     volume.ListResponse
	measureSize int64
	measureErr  error
	measureCall atomic.Int32
}

func (service *countingVolumeService) ListVolumes(context.Context) (volume.ListResponse, error) {
	return service.volumes, nil
}

func (service *countingVolumeService) ListContainers(context.Context, bool) ([]container.Summary, error) {
	return nil, nil
}

func (service *countingVolumeService) InspectContainer(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{}, nil
}

func (service *countingVolumeService) MeasureVolumeSize(context.Context, string, string) (int64, error) {
	service.measureCall.Add(1)
	if service.measureErr != nil {
		return -1, service.measureErr
	}
	return service.measureSize, nil
}

func TestRunVolumeReportAuditsDockerRunBeforeProbe(t *testing.T) {
	service := &countingVolumeService{
		volumes:     volume.ListResponse{Volumes: []volume.Volume{{Name: "data", Driver: "local"}}},
		measureSize: 4096,
	}
	previousFactory := newVolumeDockerService
	newVolumeDockerService = func() (volumeDockerService, error) { return service, nil }
	t.Cleanup(func() { newVolumeDockerService = previousFactory })

	sink := &volumeAuditSink{}
	session := newVolumeAuditSession(t, sink, audit.FailureRequired)
	ctx := audit.WithSession(context.Background(), session)
	report, err := runVolumeReport(ctx, VolumeOptions{SizeMode: volumeSizeModeDockerRun, SizeImage: "busybox:latest"})
	if err != nil {
		t.Fatalf("runVolumeReport() error = %v", err)
	}
	if service.measureCall.Load() != 1 {
		t.Fatalf("MeasureVolumeSize calls = %d, want 1", service.measureCall.Load())
	}
	if len(report.Volumes) != 1 || report.Volumes[0].Size != 4096 {
		t.Fatalf("report volumes = %#v, want measured size", report.Volumes)
	}
	events := sink.snapshot()
	if len(events) != 3 || events[1].Type != audit.EventMutationCandidates || events[2].Type != audit.EventMutationAuthorized {
		t.Fatalf("audit events = %#v, want start/candidates/authorized", volumeAuditEventTypes(events))
	}
	if events[2].Mutation == nil || events[2].Mutation.Scope != audit.MutationDockerTemporary {
		t.Fatalf("authorized mutation = %#v, want docker_temporary", events[2].Mutation)
	}
	if len(events[1].Candidates) != 1 || events[1].Candidates[0].Action != "size-probe" {
		t.Fatalf("candidate = %#v, want size-probe", events[1].Candidates)
	}
}

func TestRunVolumeReportAuditFailureBlocksDockerRunProbe(t *testing.T) {
	service := &countingVolumeService{
		volumes:     volume.ListResponse{Volumes: []volume.Volume{{Name: "data", Driver: "local"}}},
		measureSize: 4096,
	}
	previousFactory := newVolumeDockerService
	newVolumeDockerService = func() (volumeDockerService, error) { return service, nil }
	t.Cleanup(func() { newVolumeDockerService = previousFactory })

	sink := &volumeAuditSink{err: errors.New("audit unavailable")}
	session := newVolumeAuditSession(t, sink, audit.FailureDenyMutation)
	_, err := runVolumeReport(audit.WithSession(context.Background(), session), VolumeOptions{SizeMode: volumeSizeModeDockerRun})
	if err == nil {
		t.Fatal("runVolumeReport() error = nil, want audit authorization failure")
	}
	if service.measureCall.Load() != 0 {
		t.Fatalf("MeasureVolumeSize calls = %d, want 0 after denied audit", service.measureCall.Load())
	}
}

func TestRunVolumeReportAutoAuditsPotentialDockerFallback(t *testing.T) {
	service := &countingVolumeService{
		volumes:     volume.ListResponse{Volumes: []volume.Volume{{Name: "data", Driver: "local", Mountpoint: "/tmp/data"}}},
		measureSize: 8192,
	}
	previousFactory := newVolumeDockerService
	newVolumeDockerService = func() (volumeDockerService, error) { return service, nil }
	t.Cleanup(func() { newVolumeDockerService = previousFactory })
	previousLocal := measureLocalVolumeSize
	measureLocalVolumeSize = func(context.Context, *VolumeRef) (int64, error) {
		return -1, errors.New("local probe unavailable")
	}
	t.Cleanup(func() { measureLocalVolumeSize = previousLocal })

	sink := &volumeAuditSink{}
	session := newVolumeAuditSession(t, sink, audit.FailureRequired)
	if _, err := runVolumeReport(audit.WithSession(context.Background(), session), VolumeOptions{SizeMode: volumeSizeModeAuto}); err != nil {
		t.Fatalf("runVolumeReport() error = %v", err)
	}
	events := sink.snapshot()
	if len(events) != 3 || events[2].Mutation == nil || events[2].Mutation.Scope != audit.MutationDockerTemporary {
		t.Fatalf("audit events = %#v, want auto fallback authorization", volumeAuditEventTypes(events))
	}
	if service.measureCall.Load() != 1 {
		t.Fatalf("MeasureVolumeSize calls = %d, want docker fallback call", service.measureCall.Load())
	}
}

func volumeAuditEventTypes(events []audit.Event) []audit.EventType {
	result := make([]audit.EventType, len(events))
	for index, event := range events {
		result[index] = event.Type
	}
	return result
}
