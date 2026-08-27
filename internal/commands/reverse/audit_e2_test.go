package reverse

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"docker-manager/internal/audit"
)

type reverseAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (sink *reverseAuditSink) Append(_ context.Context, event audit.Event) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.err != nil {
		return sink.err
	}
	sink.events = append(sink.events, event)
	return nil
}

func (sink *reverseAuditSink) snapshot() []audit.Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]audit.Event(nil), sink.events...)
}

func newReverseAuditSession(t *testing.T, sink audit.Sink, policy audit.FailurePolicy) *audit.Session {
	t.Helper()
	session, err := audit.NewSession(audit.SessionOptions{
		Sink:          sink,
		Detail:        audit.DetailFull,
		FailurePolicy: policy,
		Operation:     "reverse",
		Command:       "dm reverse --save --reverse-type all",
		Endpoint:      "unix:///var/run/docker.sock",
		IdentifierKey: bytes.Repeat([]byte{0x81}, 32),
		Random:        bytes.NewReader(bytes.Repeat([]byte{0x82}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestAuthorizeReverseSaveAuditsSelectedOutputFiles(t *testing.T) {
	sink := &reverseAuditSink{}
	session := newReverseAuditSession(t, sink, audit.FailureRequired)
	ctx := audit.WithSession(context.Background(), session)
	if err := authorizeReverseSave(ctx, ReverseAll); err != nil {
		t.Fatalf("authorizeReverseSave() error = %v", err)
	}

	events := sink.snapshot()
	if len(events) != 3 || events[0].Type != audit.EventCommandStart || events[1].Type != audit.EventMutationCandidates || events[2].Type != audit.EventMutationAuthorized {
		t.Fatalf("audit event types = %#v, want start/candidates/authorized", reverseAuditEventTypes(events))
	}
	if events[2].Mutation == nil || events[2].Mutation.Scope != audit.MutationFilesystem || events[2].Mutation.Confirmation.Mechanism != "reverse-save" {
		t.Fatalf("authorized mutation = %#v, want filesystem reverse-save", events[2].Mutation)
	}
	if len(events[1].Candidates) != 2 {
		t.Fatalf("candidates = %#v, want two output files", events[1].Candidates)
	}
	for _, candidate := range events[1].Candidates {
		if candidate.Kind != "file" || candidate.Action != "write" {
			t.Fatalf("candidate = %#v, want file/write", candidate)
		}
	}
}

func TestAuthorizeReverseSaveFailureBlocksBeforeOutputWrite(t *testing.T) {
	sink := &reverseAuditSink{err: errors.New("audit unavailable")}
	session := newReverseAuditSession(t, sink, audit.FailureDenyMutation)
	ctx := audit.WithSession(context.Background(), session)

	if err := authorizeReverseSave(ctx, ReverseCmd); err == nil {
		t.Fatal("authorizeReverseSave() error = nil, want audit failure")
	}
}

func TestSaveReverseResultAuditFailureLeavesOutputAbsent(t *testing.T) {
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()

	sink := &reverseAuditSink{err: errors.New("audit unavailable")}
	session := newReverseAuditSession(t, sink, audit.FailureDenyMutation)
	result := NewReverseResult(nil, ReverseOptions{ReverseType: ReverseAll})
	err = saveReverseResultWithAudit(audit.WithSession(context.Background(), session), result, ReverseAll)
	if err == nil || !strings.Contains(err.Error(), "审计授权失败") {
		t.Fatalf("saveReverseResultWithAudit() error = %v, want audit authorization failure", err)
	}
	for _, name := range []string{"docker_run_command.sh", "docker-compose.reverse.yml"} {
		if _, statErr := os.Stat(filepath.Join(dir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("output %s stat = %v, want absent", name, statErr)
		}
	}
}

func TestAuthorizeRerunMutationsIncludesInspectBackupAndDockerTarget(t *testing.T) {
	sink := &reverseAuditSink{}
	session := newReverseAuditSession(t, sink, audit.FailureRequired)
	ctx := audit.WithSession(context.Background(), session)
	backupDir := filepath.Join("docker-inspect-backups", "fixed-run")
	if err := authorizeRerunMutations(ctx, []string{"demo"}, backupDir, true); err != nil {
		t.Fatalf("authorizeRerunMutations() error = %v", err)
	}

	events := sink.snapshot()
	if len(events) != 5 {
		t.Fatalf("audit event types = %#v, want start and two candidates/authorized pairs", reverseAuditEventTypes(events))
	}
	if events[1].Mutation == nil || events[1].Mutation.Scope != audit.MutationFilesystem || len(events[1].Candidates) != 1 {
		t.Fatalf("filesystem candidates event = %#v", events[1])
	}
	wantBackup := inspectBackupPath(backupDir, "demo")
	if candidate := events[1].Candidates[0]; candidate.Kind != "file" || candidate.Action != "write" || candidate.Display != wantBackup {
		t.Fatalf("inspect backup candidate = %#v, want %q", candidate, wantBackup)
	}
	if events[3].Mutation == nil || events[3].Mutation.Scope != audit.MutationDockerPersistent || len(events[3].Candidates) != 1 {
		t.Fatalf("Docker candidates event = %#v", events[3])
	}
	if candidate := events[3].Candidates[0]; candidate.Kind != "container" || candidate.Action != "rerun" || candidate.Display != "demo" {
		t.Fatalf("rerun candidate = %#v", candidate)
	}
}

func TestRerunDryRunUsesAuthorizedBackupDirectory(t *testing.T) {
	var output bytes.Buffer
	backupDir := filepath.Join("docker-inspect-backups", "fixed-run")
	if err := rerunContainers(context.Background(), []string{"demo"}, rerunOptions{
		DryRun:    true,
		Output:    &output,
		Service:   &fakeRerunService{},
		BackupDir: backupDir,
	}); err != nil {
		t.Fatal(err)
	}
	if want := inspectBackupPath(backupDir, "demo"); !strings.Contains(output.String(), want) {
		t.Fatalf("dry-run output = %q, want backup path %q", output.String(), want)
	}
}

func reverseAuditEventTypes(events []audit.Event) []audit.EventType {
	result := make([]audit.EventType, len(events))
	for index, event := range events {
		result[index] = event.Type
	}
	return result
}
