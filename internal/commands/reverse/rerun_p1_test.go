package reverse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
)

type fakeRerunService struct {
	items       map[string]container.InspectResponse
	calls       []string
	errors      map[string]error
	waitForDone bool
	waitTimeout time.Duration
}

type asyncStopRerunService struct {
	*fakeRerunService
	running atomic.Bool
	starts  atomic.Int32
}

type foreignCandidateRerunService struct {
	*fakeRerunService
	candidateName string
}

func (s *foreignCandidateRerunService) CreateFromInspectContext(_ context.Context, _ container.InspectResponse, name string) (string, error) {
	s.calls = append(s.calls, "create:"+name)
	s.candidateName = name
	foreign := container.InspectResponse{
		ID:     "foreign-id",
		Name:   "/" + name,
		Config: &container.Config{Labels: map[string]string{"external.owner": "other"}},
		State:  &container.State{Status: container.StateRunning, Running: true},
	}
	s.items[foreign.ID] = foreign
	s.items[name] = foreign
	return "", errors.New("create name conflict")
}

func newAsyncStopRerunService() *asyncStopRerunService {
	service := &asyncStopRerunService{fakeRerunService: newFakeRerunService(true)}
	service.running.Store(true)
	return service
}

func (s *asyncStopRerunService) InspectContext(ctx context.Context, id string) (container.InspectResponse, error) {
	inspect, err := s.fakeRerunService.InspectContext(ctx, id)
	if err != nil {
		return inspect, err
	}
	if inspect.ID == "old-id" {
		inspect.State = &container.State{Running: s.running.Load()}
		if inspect.State.Running {
			inspect.State.Status = container.StateRunning
		} else {
			inspect.State.Status = container.StateExited
		}
	}
	return inspect, nil
}

func (s *asyncStopRerunService) StopContext(_ context.Context, id string) error {
	s.calls = append(s.calls, "stop:"+id)
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.running.Store(false)
	}()
	return context.DeadlineExceeded
}

func (s *asyncStopRerunService) StartContext(_ context.Context, id string) error {
	s.calls = append(s.calls, "start:"+id)
	s.starts.Add(1)
	s.running.Store(true)
	return nil
}

func newFakeRerunService(running bool) *fakeRerunService {
	state := &container.State{Running: running}
	if running {
		state.Status = container.StateRunning
	} else {
		state.Status = container.StateExited
	}
	inspect := container.InspectResponse{
		ID:         "old-id",
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
		State:      state,
	}
	return &fakeRerunService{
		items:  map[string]container.InspectResponse{"old-id": inspect, "demo": inspect},
		errors: map[string]error{},
	}
}

func (f *fakeRerunService) InspectContext(_ context.Context, id string) (container.InspectResponse, error) {
	f.calls = append(f.calls, "inspect:"+id)
	if err := f.errors["inspect:"+id]; err != nil {
		return container.InspectResponse{}, err
	}
	item, ok := f.items[id]
	if !ok {
		return container.InspectResponse{}, cerrdefs.ErrNotFound
	}
	return item, nil
}

func (f *fakeRerunService) StopContext(_ context.Context, id string) error {
	f.calls = append(f.calls, "stop:"+id)
	if err := f.errors["stop:"+id]; err != nil {
		return err
	}
	f.setRunning(id, false)
	if err := f.errors["stop-after-commit:"+id]; err != nil {
		return err
	}
	return nil
}

func (f *fakeRerunService) RenameContext(_ context.Context, id, name string) error {
	f.calls = append(f.calls, "rename:"+id+":"+name)
	if err := f.errors["rename:"+id+":"+name]; err != nil {
		return err
	}
	item, ok := f.items[id]
	if !ok {
		return cerrdefs.ErrNotFound
	}
	item.Name = "/" + name
	f.store(id, item)
	f.items[name] = item
	return nil
}

func (f *fakeRerunService) CreateFromInspectContext(_ context.Context, inspect container.InspectResponse, name string) (string, error) {
	f.calls = append(f.calls, "create:"+name)
	if err := f.errors["create"]; err != nil {
		return "", err
	}
	created := inspect
	created.ID = "new-id"
	created.Name = "/" + name
	created.State = &container.State{Status: container.StateCreated}
	f.items["new-id"] = created
	f.items[name] = created
	if err := f.errors["create-after-commit"]; err != nil {
		return "", err
	}
	return "new-id", nil
}

func (f *fakeRerunService) StartContext(_ context.Context, id string) error {
	f.calls = append(f.calls, "start:"+id)
	if err := f.errors["start:"+id]; err != nil {
		return err
	}
	f.setRunning(id, true)
	if err := f.errors["start-after-commit:"+id]; err != nil {
		return err
	}
	return nil
}

func (f *fakeRerunService) WaitReadyContext(ctx context.Context, id string, requireHealthy bool) error {
	f.calls = append(f.calls, fmt.Sprintf("wait:%s:healthy=%v", id, requireHealthy))
	if deadline, ok := ctx.Deadline(); ok {
		f.waitTimeout = time.Until(deadline)
	}
	if f.waitForDone {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.errors["wait:"+id]
}

func (f *fakeRerunService) RemoveContext(_ context.Context, id string, force, removeVolumes bool) error {
	f.calls = append(f.calls, fmt.Sprintf("remove:%s:force=%v:volumes=%v", id, force, removeVolumes))
	if err := f.errors["remove:"+id]; err != nil {
		return err
	}
	targetID := id
	if item, ok := f.items[id]; ok && item.ID != "" {
		targetID = item.ID
	}
	delete(f.items, id)
	for key, item := range f.items {
		if item.ID == targetID {
			delete(f.items, key)
		}
	}
	if err := f.errors["remove-after-commit:"+id]; err != nil {
		return err
	}
	return nil
}

func (f *fakeRerunService) setRunning(id string, running bool) {
	item := f.items[id]
	if item.State == nil {
		item.State = &container.State{}
	}
	item.State.Running = running
	if running {
		item.State.Status = container.StateRunning
	} else {
		item.State.Status = container.StateExited
	}
	f.store(id, item)
}

func (f *fakeRerunService) store(id string, item container.InspectResponse) {
	f.items[id] = item
	for key, existing := range f.items {
		if existing.ID == item.ID {
			f.items[key] = item
		}
	}
}

func TestSafelyRerunContainerCommitsHealthyCandidateBeforeOldRemoval(t *testing.T) {
	fake := newFakeRerunService(true)
	old := fake.items["old-id"]
	old.Config.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "true"}}
	fake.store("old-id", old)

	id, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second)
	if err != nil || id != "new-id" {
		t.Fatalf("safelyRerunContainer() = %q, %v", id, err)
	}
	assertRerunCallOrder(t, fake.calls, "stop:old-id", "rename:old-id:demo-dm-rerun-rollback-", "create:demo-dm-rerun-candidate-", "start:new-id", "wait:new-id:healthy=true", "rename:new-id:demo", "remove:old-id:force=false")
	if current := fake.items["demo"]; current.ID != "new-id" || current.State == nil || !current.State.Running {
		t.Fatalf("committed container = %#v", current)
	}
}

func TestSafelyRerunContainerRollsBackAfterCandidateStartFailure(t *testing.T) {
	fake := newFakeRerunService(true)
	fake.errors["start:new-id"] = errors.New("start failed")

	_, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second)
	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("safelyRerunContainer() error = %v", err)
	}
	assertRerunCallOrder(t, fake.calls, "create:demo-dm-rerun-candidate-", "start:new-id", "remove:new-id:force=true", "rename:old-id:demo", "start:old-id")
	if current := fake.items["demo"]; current.ID != "old-id" || current.State == nil || !current.State.Running {
		t.Fatalf("rolled back container = %#v", current)
	}
}

func TestSafelyRerunContainerReadyTimeoutRollsBack(t *testing.T) {
	fake := newFakeRerunService(true)
	fake.waitForDone = true

	_, err := safelyRerunContainer(context.Background(), fake, "demo", 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("safelyRerunContainer() error = %v", err)
	}
	if fake.waitTimeout <= 0 || fake.waitTimeout > 30*time.Millisecond {
		t.Fatalf("wait timeout = %v, want configured timeout", fake.waitTimeout)
	}
	assertRerunCallOrder(t, fake.calls, "wait:new-id", "remove:new-id:force=true", "rename:old-id:demo", "start:old-id")
}

func TestSafelyRerunContainerWithoutHealthcheckCommitsAfterRunning(t *testing.T) {
	fake := newFakeRerunService(true)

	if _, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second); err != nil {
		t.Fatal(err)
	}
	assertRerunCallOrder(t, fake.calls, "start:new-id", "wait:new-id:healthy=false", "rename:new-id:demo", "remove:old-id:force=false")
}

func TestSafelyRerunContainerUnhealthyCandidateRollsBack(t *testing.T) {
	fake := newFakeRerunService(true)
	old := fake.items["old-id"]
	old.Config.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "false"}}
	fake.store("old-id", old)
	fake.errors["wait:new-id"] = errors.New("container healthcheck reported unhealthy")

	_, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("safelyRerunContainer() error = %v", err)
	}
	assertRerunCallOrder(t, fake.calls, "wait:new-id:healthy=true", "remove:new-id:force=true", "rename:old-id:demo", "start:old-id")
	if current := fake.items["demo"]; current.ID != "old-id" || current.State == nil || !current.State.Running {
		t.Fatalf("rolled back container = %#v", current)
	}
}

func TestSafelyRerunContainerCancellationDuringReadinessRollsBack(t *testing.T) {
	fake := newFakeRerunService(true)
	fake.errors["wait:new-id"] = context.Canceled

	_, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("safelyRerunContainer() error = %v, want context.Canceled", err)
	}
	assertRerunCallOrder(t, fake.calls, "wait:new-id", "remove:new-id:force=true", "rename:old-id:demo", "start:old-id")
}

func TestSafelyRerunContainerCleansCandidateAfterCreateResponseLoss(t *testing.T) {
	fake := newFakeRerunService(true)
	fake.errors["create-after-commit"] = errors.New("create response lost")

	_, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second)
	if err == nil || !strings.Contains(err.Error(), "create response lost") {
		t.Fatalf("safelyRerunContainer() error = %v, want create response error", err)
	}
	if _, exists := fake.items["new-id"]; exists {
		t.Fatalf("candidate remains after reconciliation: %#v", fake.items)
	}
	if current := fake.items["demo"]; current.ID != "old-id" || current.State == nil || !current.State.Running {
		t.Fatalf("original container was not restored: %#v", current)
	}
}

func TestSafelyRerunContainerDoesNotRemoveForeignSameNameCandidate(t *testing.T) {
	service := &foreignCandidateRerunService{fakeRerunService: newFakeRerunService(true)}

	_, err := safelyRerunContainer(context.Background(), service, "demo", time.Second)
	if err == nil || !strings.Contains(err.Error(), "create name conflict") || !strings.Contains(err.Error(), "ownership label") {
		t.Fatalf("safelyRerunContainer() error = %v, want create and ownership errors", err)
	}
	if service.candidateName == "" {
		t.Fatal("candidate name was not captured")
	}
	if foreign, ok := service.items["foreign-id"]; !ok || foreign.ID != "foreign-id" {
		t.Fatalf("foreign candidate = %#v, want preserved", foreign)
	}
	if callsContainExact(service.calls, "remove:foreign-id:force=true:volumes=false") ||
		callsContainExact(service.calls, "remove:"+service.candidateName+":force=true:volumes=false") {
		t.Fatalf("calls = %#v, foreign candidate must not be removed", service.calls)
	}
	if current := service.items["demo"]; current.ID != "old-id" || current.State == nil || !current.State.Running {
		t.Fatalf("original container was not restored: %#v", current)
	}
}

func TestSafelyRerunContainerAcceptsConfirmedCandidateRemovalAfterResponseError(t *testing.T) {
	fake := newFakeRerunService(true)
	fake.errors["start:new-id"] = errors.New("start failed")
	fake.errors["remove-after-commit:new-id"] = errors.New("remove response lost")

	_, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second)
	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("safelyRerunContainer() error = %v, want start failure", err)
	}
	if strings.Contains(err.Error(), "remove response lost") {
		t.Fatalf("confirmed candidate removal was reported as failed: %v", err)
	}
	if current := fake.items["demo"]; current.ID != "old-id" || current.State == nil || !current.State.Running {
		t.Fatalf("original container was not restored: %#v", current)
	}
}

func TestSafelyRerunContainerRecoversOriginalAfterAsynchronousStopError(t *testing.T) {
	service := newAsyncStopRerunService()

	_, err := safelyRerunContainer(context.Background(), service, "demo", time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("safelyRerunContainer() error = %v, want original stop deadline", err)
	}
	if !service.running.Load() || service.starts.Load() == 0 {
		t.Fatalf("original state running=%v starts=%d, want recovered running container", service.running.Load(), service.starts.Load())
	}
	if callsContain(service.calls, "create:") || callsContain(service.calls, "rename:") {
		t.Fatalf("calls = %#v, candidate work must not begin after an uncertain stop", service.calls)
	}
}

func TestSafelyRerunContainerRecoversOriginalAfterCommittedStopError(t *testing.T) {
	fake := newFakeRerunService(true)
	fake.errors["stop-after-commit:old-id"] = errors.New("daemon stop response failed")

	_, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second)
	if err == nil || !strings.Contains(err.Error(), "daemon stop response failed") {
		t.Fatalf("safelyRerunContainer() error = %v, want stop response error", err)
	}
	if current := fake.items["demo"]; current.State == nil || !current.State.Running {
		t.Fatalf("original container was not restored: %#v", current)
	}
	if callsContain(fake.calls, "rename:") || callsContain(fake.calls, "create:") {
		t.Fatalf("calls = %#v, candidate work must not begin after stop failure", fake.calls)
	}
}

func TestSafelyRerunContainerPreservesStoppedState(t *testing.T) {
	fake := newFakeRerunService(false)

	if _, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(fake.calls, "\n")
	if strings.Contains(joined, "stop:old-id") || strings.Contains(joined, "start:new-id") || strings.Contains(joined, "wait:new-id") {
		t.Fatalf("calls = %#v, stopped state should be preserved", fake.calls)
	}
	if current := fake.items["demo"]; current.State == nil || current.State.Running {
		t.Fatalf("committed stopped container = %#v", current)
	}
}

func TestSafelyRerunContainerRejectsAutoRemoveWithoutMutation(t *testing.T) {
	fake := newFakeRerunService(true)
	old := fake.items["old-id"]
	old.HostConfig.AutoRemove = true
	fake.store("old-id", old)

	_, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second)
	if err == nil || !strings.Contains(err.Error(), "auto-remove") {
		t.Fatalf("safelyRerunContainer() error = %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "inspect:demo" {
		t.Fatalf("calls = %#v, want inspect only", fake.calls)
	}
}

func TestSafelyRerunContainerRetainsCommittedCandidateWhenOldCleanupFails(t *testing.T) {
	fake := newFakeRerunService(true)
	fake.errors["remove:old-id"] = errors.New("busy")

	id, err := safelyRerunContainer(context.Background(), fake, "demo", time.Second)
	if id != "new-id" || err == nil || !strings.Contains(err.Error(), "healthy new container retained") {
		t.Fatalf("safelyRerunContainer() = %q, %v", id, err)
	}
	if current := fake.items["demo"]; current.ID != "new-id" {
		t.Fatalf("new container was not retained: %#v", current)
	}
	if callsContain(fake.calls, "remove:new-id:force=true") || callsContainExact(fake.calls, "rename:old-id:demo") {
		t.Fatalf("calls = %#v, committed candidate must not roll back", fake.calls)
	}
}

func TestRerunRejectsNonPositiveReadyTimeout(t *testing.T) {
	cmd := NewRerunCommand()
	cmd.SetArgs([]string{"demo", "--dry-run", "--ready-timeout=0s"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--ready-timeout") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func assertRerunCallOrder(t *testing.T, calls []string, prefixes ...string) {
	t.Helper()
	next := 0
	for _, call := range calls {
		if next < len(prefixes) && strings.HasPrefix(call, prefixes[next]) {
			next++
		}
	}
	if next != len(prefixes) {
		t.Fatalf("calls = %#v, missing ordered prefix %q", calls, prefixes[next])
	}
}

func callsContain(calls []string, prefix string) bool {
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

func callsContainExact(calls []string, value string) bool {
	for _, call := range calls {
		if call == value {
			return true
		}
	}
	return false
}
