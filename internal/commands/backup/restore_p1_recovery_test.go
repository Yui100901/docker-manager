package backup

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
)

type asyncStopRestoreService struct {
	*fakeBackupDockerService
	running atomic.Bool
	starts  atomic.Int32
}

type foreignCandidateRestoreService struct {
	*fakeBackupDockerService
	candidateName string
}

func (s *foreignCandidateRestoreService) CreateContainer(_ context.Context, _ container.InspectResponse, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "create-container:"+name)
	s.candidateName = name
	foreign := container.InspectResponse{
		ID:     "foreign-id",
		Name:   "/" + name,
		Config: &container.Config{Labels: map[string]string{"external.owner": "other"}},
	}
	if s.inspects == nil {
		s.inspects = make(map[string]container.InspectResponse)
	}
	s.inspects[foreign.ID] = foreign
	s.inspects[name] = foreign
	return "", errors.New("create name conflict")
}

func newAsyncStopRestoreService() *asyncStopRestoreService {
	service := &asyncStopRestoreService{fakeBackupDockerService: runningExistingRestoreService("demo", "old-id")}
	service.running.Store(true)
	return service
}

func (s *asyncStopRestoreService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	inspect, err := s.fakeBackupDockerService.InspectContainer(ctx, name)
	if err != nil {
		return inspect, err
	}
	if name == "demo" || name == "old-id" {
		inspect.ID = "old-id"
		inspect.Name = "/demo"
		inspect.HostConfig = &container.HostConfig{}
		inspect.State = &container.State{Running: s.running.Load()}
		if inspect.State.Running {
			inspect.State.Status = container.StateRunning
		} else {
			inspect.State.Status = container.StateExited
		}
	}
	return inspect, nil
}

func (s *asyncStopRestoreService) StopContainer(_ context.Context, id string) error {
	s.mu.Lock()
	s.calls = append(s.calls, "stop-container:"+id)
	s.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.running.Store(false)
	}()
	return context.DeadlineExceeded
}

func (s *asyncStopRestoreService) StartContainer(_ context.Context, id string) error {
	s.mu.Lock()
	s.calls = append(s.calls, "start-container:"+id)
	s.mu.Unlock()
	s.starts.Add(1)
	s.running.Store(true)
	return nil
}

func TestStageRestoreReplacementRecoversOriginalAfterAsynchronousStopError(t *testing.T) {
	service := newAsyncStopRestoreService()

	_, err := stageRestoreReplacement(context.Background(), service, "demo", "old-id")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stageRestoreReplacement() error = %v, want original stop deadline", err)
	}
	if !service.running.Load() || service.starts.Load() == 0 {
		t.Fatalf("original state running=%v starts=%d, want recovered running container", service.running.Load(), service.starts.Load())
	}
	if hasCallPrefix(service.calls, "rename-container:") {
		t.Fatalf("calls = %#v, original container must not be staged after an uncertain stop", service.calls)
	}
}

func TestStageRestoreReplacementRecoversOriginalAfterCommittedOrdinaryStopError(t *testing.T) {
	service := runningExistingRestoreService("demo", "old-id")
	service.stopCommit = true
	service.stopErr = errors.New("daemon stop response failed")
	service.startErrors = map[string]error{"old-id": errors.New("daemon start response failed")}
	service.startCommitError = map[string]bool{"old-id": true}

	_, err := stageRestoreReplacement(context.Background(), service, "demo", "old-id")
	if err == nil || !strings.Contains(err.Error(), "daemon stop response failed") {
		t.Fatalf("stageRestoreReplacement() error = %v, want original stop error", err)
	}
	old, inspectErr := service.InspectContainer(context.Background(), "old-id")
	if inspectErr != nil || old.State == nil || !old.State.Running {
		t.Fatalf("old container = %#v error=%v, want restored running state", old, inspectErr)
	}
	if !hasCall(service.calls, "start-container:old-id") {
		t.Fatalf("calls = %#v, want original container restart", service.calls)
	}
	if hasCallPrefix(service.calls, "rename-container:") {
		t.Fatalf("calls = %#v, original container must not be staged after a stop error", service.calls)
	}
}

func TestRestoreNewTargetCreateCommitErrorCleansRandomCandidate(t *testing.T) {
	service := &fakeBackupDockerService{
		createErr:         errors.New("create response lost"),
		createCommitError: true,
	}

	_, err := createRestoredContainer(context.Background(), service, basicRestoreInspect("demo"), "demo", false, "", RestoreOptions{NoStart: true})
	if err == nil || !strings.Contains(err.Error(), "create response lost") {
		t.Fatalf("createRestoredContainer() error = %v, want create response error", err)
	}
	if !hasCallPrefix(service.calls, "create-container:demo-dm-restore-candidate-") {
		t.Fatalf("calls = %#v, want random candidate creation", service.calls)
	}
	if !hasCall(service.calls, "remove-container:restored-id") {
		t.Fatalf("calls = %#v, committed candidate was not reconciled and removed", service.calls)
	}
	if hasCall(service.calls, "create-container:demo") || hasCall(service.calls, "rename-container:restored-id:demo") {
		t.Fatalf("calls = %#v, uncertain candidate must not claim the final target", service.calls)
	}
}

func TestRestoreCreateErrorDoesNotRemoveForeignSameNameCandidate(t *testing.T) {
	base := runningExistingRestoreService("demo", "old-id")
	base.inspects["old-id"] = base.inspects["demo"]
	service := &foreignCandidateRestoreService{fakeBackupDockerService: base}

	_, err := createRestoredContainer(context.Background(), service, basicRestoreInspect("demo"), "demo", true, "old-id", RestoreOptions{Replace: true, NoStart: true})
	if err == nil || !strings.Contains(err.Error(), "create name conflict") || !strings.Contains(err.Error(), "ownership label") {
		t.Fatalf("createRestoredContainer() error = %v, want create and ownership errors", err)
	}
	if service.candidateName == "" {
		t.Fatal("candidate name was not captured")
	}
	foreign, inspectErr := service.InspectContainer(context.Background(), "foreign-id")
	if inspectErr != nil || foreign.ID != "foreign-id" {
		t.Fatalf("foreign candidate = %#v error=%v, want preserved", foreign, inspectErr)
	}
	if hasCall(service.calls, "remove-container:foreign-id") || hasCall(service.calls, "remove-container:"+service.candidateName) {
		t.Fatalf("calls = %#v, foreign candidate must not be removed", service.calls)
	}
	old, inspectErr := service.InspectContainer(context.Background(), "old-id")
	if inspectErr != nil || normalizeContainerName(old.Name) != "demo" || old.State == nil || !old.State.Running {
		t.Fatalf("original container = %#v error=%v, want restored name and running state", old, inspectErr)
	}
}

func TestRollbackRestoredContainerReconcilesCommittedResponseErrors(t *testing.T) {
	const candidateOwner = "restore-test-owner"
	responseLost := errors.New("response lost")
	service := runningExistingRestoreService("demo", "old-id")
	service.inspects["old-id"] = container.InspectResponse{
		ID: "old-id", Name: "/demo-dm-restore-rollback-existing", State: &container.State{Running: false},
	}
	service.inspects["new-id"] = container.InspectResponse{
		ID: "new-id", Name: "/demo-dm-restore-candidate-existing",
		Config: &container.Config{Labels: map[string]string{restoreCandidateOwnerLabel: candidateOwner}},
	}
	service.removeErrors = map[string]error{"new-id": responseLost}
	service.removeCommitError = map[string]bool{"new-id": true}
	service.renameErrors = map[string]error{"demo": responseLost}
	service.renameCommitError = map[string]bool{"demo": true}
	service.startErrors = map[string]error{"old-id": responseLost}
	service.startCommitError = map[string]bool{"old-id": true}

	err := rollbackRestoredContainer(service, "new-id", candidateOwner, &stagedRestoreReplacement{
		oldID: "old-id", targetName: "demo", backupName: "demo-dm-restore-rollback-existing", wasRunning: true,
	})
	if err != nil {
		t.Fatalf("rollbackRestoredContainer() error = %v, want committed response errors reconciled", err)
	}
	if _, inspectErr := service.InspectContainer(context.Background(), "new-id"); !errors.Is(inspectErr, cerrdefs.ErrNotFound) {
		t.Fatalf("new container inspect error = %v, want not found", inspectErr)
	}
	old, inspectErr := service.InspectContainer(context.Background(), "old-id")
	if inspectErr != nil || normalizeContainerName(old.Name) != "demo" || old.State == nil || !old.State.Running {
		t.Fatalf("old container = %#v error=%v, want restored name and running state", old, inspectErr)
	}
}

func TestRollbackRestoredContainerReportsUnresolvedResponseErrors(t *testing.T) {
	const candidateOwner = "restore-test-owner"
	responseLost := errors.New("response lost")
	service := runningExistingRestoreService("demo", "old-id")
	service.inspects["old-id"] = container.InspectResponse{
		ID: "old-id", Name: "/demo-dm-restore-rollback-existing", State: &container.State{Running: false},
	}
	service.inspects["new-id"] = container.InspectResponse{
		ID: "new-id", Name: "/demo-dm-restore-candidate-existing",
		Config: &container.Config{Labels: map[string]string{restoreCandidateOwnerLabel: candidateOwner}},
	}
	service.removeErrors = map[string]error{"new-id": responseLost}
	service.renameErrors = map[string]error{"demo": responseLost}
	service.startErrors = map[string]error{"old-id": responseLost}

	err := rollbackRestoredContainer(service, "new-id", candidateOwner, &stagedRestoreReplacement{
		oldID: "old-id", targetName: "demo", backupName: "demo-dm-restore-rollback-existing", wasRunning: true,
	})
	if err == nil {
		t.Fatal("rollbackRestoredContainer() error = nil, want unresolved rollback state")
	}
	for _, want := range []string{"container still exists", "did not regain target name", "is not running"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("rollback error = %v, want %q", err, want)
		}
	}
}

func TestStageRestoreReplacementReconcilesRestartResponseError(t *testing.T) {
	responseLost := errors.New("restart response lost")
	service := runningExistingRestoreService("demo", "old-id")
	service.stopCommit = true
	service.renameBeforeCommitErr = errors.New("rename response lost")
	service.startErrors = map[string]error{"old-id": responseLost}
	service.startCommitError = map[string]bool{"old-id": true}

	_, err := stageRestoreReplacement(context.Background(), service, "demo", "old-id")
	if err == nil || !strings.Contains(err.Error(), "rename response lost") {
		t.Fatalf("stageRestoreReplacement() error = %v, want original rename error", err)
	}
	if strings.Contains(err.Error(), "rollback restart") {
		t.Fatalf("stage error = %v, committed restart must be reconciled", err)
	}
	old, inspectErr := service.InspectContainer(context.Background(), "old-id")
	if inspectErr != nil || old.State == nil || !old.State.Running {
		t.Fatalf("old container = %#v error=%v, want running after reconciled restart", old, inspectErr)
	}
}
