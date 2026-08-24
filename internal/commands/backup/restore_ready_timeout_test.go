package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type blockingReadyRestoreService struct {
	*fakeBackupDockerService
	observedTimeout time.Duration
}

func (s *blockingReadyRestoreService) WaitContainerReady(ctx context.Context, id string, requireHealthy bool) error {
	if deadline, ok := ctx.Deadline(); ok {
		s.observedTimeout = time.Until(deadline)
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestRestoreReadyTimeoutRollsBackCandidate(t *testing.T) {
	base := runningExistingRestoreService("demo", "old-id")
	svc := &blockingReadyRestoreService{fakeBackupDockerService: base}

	_, err := createRestoredContainer(context.Background(), svc, basicRestoreInspect("demo"), "demo", true, "old-id", RestoreOptions{
		Replace:      true,
		ReadyTimeout: 20 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("createRestoredContainer() error = %v, want deadline exceeded", err)
	}
	if svc.observedTimeout <= 0 || svc.observedTimeout > 30*time.Millisecond {
		t.Fatalf("observed timeout = %v, want configured timeout", svc.observedTimeout)
	}
	requireCallOrder(t, base.calls, "remove-container:restored-id", "rename-container:old-id:demo", "start-container:old-id")
}

func TestRestoreCommandRejectsNonPositiveReadyTimeout(t *testing.T) {
	cmd := NewRestoreCommand()
	cmd.SetArgs([]string{"unused", "--confirm", "--ready-timeout=0s"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--ready-timeout") {
		t.Fatalf("Execute() error = %v, want ready-timeout validation", err)
	}
}
