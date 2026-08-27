package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failThenRecordSink lets lifecycle tests exercise a failed write followed by
// recovery without changing the Session's public API.
type failThenRecordSink struct {
	mu        sync.Mutex
	remaining int
	err       error
	events    []Event
}

func (sink *failThenRecordSink) Append(ctx context.Context, event Event) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.remaining > 0 {
		sink.remaining--
		return sink.err
	}
	sink.events = append(sink.events, event)
	return nil
}

func (sink *failThenRecordSink) snapshot() []Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]Event(nil), sink.events...)
}

func TestSessionLifecycleIsIdempotentAndAutoStarts(t *testing.T) {
	t.Run("explicit start and repeated finish", func(t *testing.T) {
		sink := &recordingSink{}
		session := newTestSession(t, sink, FailureRequired, DetailSafe)
		if !session.Enabled() {
			t.Fatal("Enabled() = false for a session with a sink")
		}
		if session.RunID() == "" {
			t.Fatal("RunID() is empty")
		}
		if err := session.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := session.Start(context.Background()); err != nil {
			t.Fatalf("second Start() error = %v", err)
		}
		if err := session.Finish(context.Background(), FinishResult{Outcome: OutcomeSuccess}); err != nil {
			t.Fatal(err)
		}
		if err := session.Finish(context.Background(), FinishResult{Outcome: OutcomeFailed, Err: errors.New("must be ignored")}); err != nil {
			t.Fatalf("second Finish() error = %v", err)
		}
		if err := session.Start(context.Background()); !errors.Is(err, ErrSessionFinished) {
			t.Fatalf("Start() after Finish() = %v, want ErrSessionFinished", err)
		}
		if _, err := session.AuthorizeMutation(context.Background(), MutationRequest{
			Scope:        MutationDockerPersistent,
			Confirmation: Confirmation{Provided: true},
		}); !errors.Is(err, ErrSessionFinished) {
			t.Fatalf("AuthorizeMutation() after Finish() = %v, want ErrSessionFinished", err)
		}
		events := sink.snapshot()
		if len(events) != 2 || events[0].Type != EventCommandStart || events[1].Type != EventCommandFinish {
			t.Fatalf("events = %#v, want one start and one finish", eventTypes(events))
		}
		if events[0].Sequence != 1 || events[1].Sequence != 2 {
			t.Fatalf("event sequences = %d, %d", events[0].Sequence, events[1].Sequence)
		}
	})

	t.Run("authorize starts session when needed", func(t *testing.T) {
		sink := &recordingSink{}
		session := newTestSession(t, sink, FailureRequired, DetailSafe)
		authorization, err := session.AuthorizeMutation(context.Background(), MutationRequest{
			Scope:        MutationDockerTemporary,
			Confirmation: Confirmation{Provided: true},
			Candidates:   []CandidateInput{{Kind: "image", Action: "load", Identifier: "sha256:one"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !authorization.Allowed || !authorization.Audited {
			t.Fatalf("authorization = %#v, want audited authorization", authorization)
		}
		if err := session.Finish(context.Background(), FinishResult{}); err != nil {
			t.Fatal(err)
		}
		events := sink.snapshot()
		if len(events) != 4 {
			t.Fatalf("event count = %d, want start + candidate + authorized + finish", len(events))
		}
		if events[0].Type != EventCommandStart || events[1].Type != EventMutationCandidates || events[2].Type != EventMutationAuthorized || events[3].Type != EventCommandFinish {
			t.Fatalf("event types = %#v", eventTypes(events))
		}
	})
}

func TestSessionSafeDetailDoesNotLeakAnyConfiguredSecret(t *testing.T) {
	const secret = "audit-secret-value-9f3e"
	sink := &recordingSink{}
	session, err := NewSession(SessionOptions{
		Sink:          sink,
		Detail:        DetailSafe,
		FailurePolicy: FailureRequired,
		Operation:     "diagnostics.health",
		Command:       "dm health --token=" + secret,
		Profile:       "production-password=" + secret,
		Endpoint:      "tcp://admin:" + secret + "@docker.example:2376/path?token=" + secret,
		Operator:      Operator{OSUser: "operator", UIDOrSID: "0", Hostname: "node", AssertedActor: "authorization=Bearer " + secret},
		IdentifierKey: bytesOf(0x99, identifierKeyBytes),
		Clock: func() time.Time {
			return time.Date(2026, time.August, 27, 1, 2, 3, 0, time.UTC)
		},
		Random: &deterministicReader{value: 0x22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := session.AuthorizeMutation(context.Background(), MutationRequest{
		Scope:        MutationDockerPersistent,
		Confirmation: Confirmation{Provided: true, Mechanism: "--confirm token=" + secret},
		Candidates: []CandidateInput{{
			Kind:       "container",
			Action:     "delete",
			Identifier: secret,
			Display:    "password=" + secret,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Finish(context.Background(), FinishResult{Err: fmt.Errorf("request failed: authorization=Bearer %s password=%s", secret, secret)}); err != nil {
		t.Fatal(err)
	}
	for index, event := range sink.snapshot() {
		data, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("safe event[%d] leaked %q: %s", index, secret, data)
		}
		if event.Endpoint.ID == secret || strings.Contains(event.Endpoint.ID, "://") {
			t.Fatalf("event[%d] endpoint ID is not an opaque identifier: %#v", index, event.Endpoint)
		}
	}
}

func TestSessionFailurePolicyWarnsOnceAndDenyMutationBlocks(t *testing.T) {
	t.Run("warn continues after a transient start failure", func(t *testing.T) {
		sink := &failThenRecordSink{remaining: 1, err: errors.New("temporary audit outage")}
		var warningCount atomic.Int32
		session, err := NewSession(SessionOptions{
			Sink:          sink,
			FailurePolicy: FailureWarn,
			Operation:     "health",
			Warning: func(error) {
				warningCount.Add(1)
			},
			IdentifierKey: bytesOf(2, identifierKeyBytes),
			Random:        &deterministicReader{value: 3},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Start(context.Background()); err != nil {
			t.Fatalf("warn Start() error = %v", err)
		}
		if _, err := session.AuthorizeMutation(context.Background(), MutationRequest{
			Scope:        MutationDockerTemporary,
			Confirmation: Confirmation{Provided: true},
		}); err != nil {
			t.Fatal(err)
		}
		if err := session.Finish(context.Background(), FinishResult{}); err != nil {
			t.Fatal(err)
		}
		if got := warningCount.Load(); got != 1 {
			t.Fatalf("warning count = %d, want one warning", got)
		}
		if got := len(sink.snapshot()); got == 0 {
			t.Fatal("transient sink never recorded a recovery event")
		}
	})

	t.Run("deny mutation still allows read-only start failure", func(t *testing.T) {
		sink := &recordingSink{err: errors.New("audit unavailable")}
		var warningCount atomic.Int32
		session := newTestSession(t, sink, FailureDenyMutation, DetailSafe)
		session.warning = func(error) { warningCount.Add(1) }
		if err := session.Start(context.Background()); err != nil {
			t.Fatalf("read-only Start() error = %v", err)
		}
		if _, err := session.AuthorizeMutation(context.Background(), MutationRequest{
			Scope:        MutationFilesystem,
			Confirmation: Confirmation{Provided: true},
		}); err == nil {
			t.Fatal("mutation was allowed while audit sink remained unavailable")
		}
		if got := warningCount.Load(); got != 1 {
			t.Fatalf("warning count = %d, want one warning for the read-only failure", got)
		}
	})
}

func TestNewSessionRejectsInvalidConfiguration(t *testing.T) {
	valid := SessionOptions{
		Sink:          &recordingSink{},
		Operation:     "health",
		IdentifierKey: bytesOf(4, identifierKeyBytes),
		Random:        &deterministicReader{value: 5},
	}
	tests := []struct {
		name   string
		mutate func(*SessionOptions)
	}{
		{name: "detail", mutate: func(opts *SessionOptions) { opts.Detail = "verbose" }},
		{name: "failure policy", mutate: func(opts *SessionOptions) { opts.FailurePolicy = "ignore" }},
		{name: "operation", mutate: func(opts *SessionOptions) { opts.Operation = "health/unsafe" }},
		{name: "page size", mutate: func(opts *SessionOptions) { opts.CandidatePageSize = DefaultCandidatePageSize + 1 }},
		{name: "max candidates", mutate: func(opts *SessionOptions) { opts.MaxCandidates = DefaultMaxCandidates + 1 }},
		{name: "short key", mutate: func(opts *SessionOptions) { opts.IdentifierKey = []byte("short") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := valid
			test.mutate(&opts)
			if _, err := NewSession(opts); err == nil {
				t.Fatal("NewSession() accepted invalid configuration")
			}
		})
	}
}

func TestSessionNilAndDisabledSinksRemainNoopButHonorConfirmation(t *testing.T) {
	var nilSession *Session
	if nilSession.Enabled() {
		t.Fatal("nil session reported Enabled()")
	}
	if err := nilSession.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := nilSession.Finish(context.Background(), FinishResult{}); err != nil {
		t.Fatal(err)
	}

	session, err := NewSession(SessionOptions{
		FailurePolicy: FailureRequired,
		Operation:     "health",
		IdentifierKey: bytesOf(7, identifierKeyBytes),
		Random:        &deterministicReader{value: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.Enabled() {
		t.Fatal("session with nil sink reported Enabled()")
	}
	if _, err := session.AuthorizeMutation(context.Background(), MutationRequest{
		Scope:        MutationDockerPersistent,
		Confirmation: Confirmation{Required: true},
	}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("disabled audit confirmation error = %v, want ErrConfirmationRequired", err)
	}
	authorization, err := session.AuthorizeMutation(context.Background(), MutationRequest{
		Scope:        MutationDockerPersistent,
		Confirmation: Confirmation{Required: true, Provided: true},
	})
	if err != nil || !authorization.Allowed || authorization.Audited {
		t.Fatalf("disabled authorization = %#v, err=%v", authorization, err)
	}
}

func TestFileSinkHardenExistingPermissionsAndRejectsMalformedEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	keyPath := filepath.Join(dir, "audit.key")
	lockPath := path + ".lock"
	keyText := strings.Repeat("ab", identifierKeyBytes) + "\n"
	for _, item := range []struct {
		path string
		data []byte
	}{
		{path: path, data: []byte{}},
		{path: keyPath, data: []byte(keyText)},
		{path: lockPath, data: []byte{}},
	} {
		if err := os.WriteFile(item.path, item.data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	sink, err := OpenFileSink(FileOptions{Path: path, KeyPath: keyPath, MaxEventBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Path(); got != path {
		t.Fatalf("Path() = %q, want %q", got, path)
	}
	if err := sink.Append(context.Background(), Event{Schema: "wrong"}); err == nil {
		t.Fatal("Append() accepted malformed event")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, item := range []string{path, keyPath, lockPath} {
			info, statErr := os.Stat(item)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if got := info.Mode().Perm(); got != 0600 {
				t.Errorf("%s mode = %o, want 600", item, got)
			}
		}
	}
	if err := sink.Append(context.Background(), testEvent(2, "closed")); err == nil {
		t.Fatal("Append() succeeded after Close()")
	}
}

func TestFileSinkConcurrentAppendsAcrossSinkInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	first, err := OpenFileSink(FileOptions{Path: path, MaxBytes: 1 << 20, MaxEventBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenFileSink(FileOptions{Path: path, MaxBytes: 1 << 20, MaxEventBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if string(first.IdentifierKey()) != string(second.IdentifierKey()) {
		t.Fatal("sink instances did not share the persisted identifier key")
	}

	const perSink = 30
	errCh := make(chan error, perSink*2)
	var group sync.WaitGroup
	appendEvents := func(sink *FileSink, offset uint64) {
		defer group.Done()
		for index := 0; index < perSink; index++ {
			sequence := offset + uint64(index)
			if appendErr := sink.Append(context.Background(), testEvent(sequence, strings.Repeat("z", 180))); appendErr != nil {
				errCh <- fmt.Errorf("append %d: %w", sequence, appendErr)
			}
		}
	}
	group.Add(2)
	go appendEvents(first, 1)
	go appendEvents(second, perSink+1)
	group.Wait()
	close(errCh)
	for appendErr := range errCh {
		t.Error(appendErr)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := auditJSONLines(data)
	if got, want := len(lines), perSink*2; got != want {
		t.Fatalf("line count = %d, want %d", got, want)
	}
	for index, line := range lines {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("line %d is invalid JSON: %v", index+1, err)
		}
	}
}

func TestFileSinkLockTimeoutAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink, err := OpenFileSink(FileOptions{Path: path, MaxBytes: 1 << 20, LockTimeout: 80 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	holder, err := openAuditLockFile(sink.lockPath)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := tryLockAuditFile(holder)
	if err != nil {
		_ = holder.Close()
		t.Fatal(err)
	}
	if !locked {
		_ = holder.Close()
		t.Skipf("platform did not permit an independent audit lock handle")
	}
	defer func() {
		_ = unlockAuditFile(holder)
		_ = holder.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	err = sink.Append(ctx, testEvent(1, "blocked"))
	cancel()
	if err == nil {
		t.Fatal("Append() succeeded while another handle held the audit lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock timeout error = %v, want context deadline", err)
	}
	if unlockErr := unlockAuditFile(holder); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	if err := sink.Append(context.Background(), testEvent(2, "recovered")); err != nil {
		t.Fatalf("Append() after lock release = %v", err)
	}
}

func TestFileSinkRotationKeepsBoundedWholeJSONLFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink, err := OpenFileSink(FileOptions{Path: path, MaxBytes: 1400, MaxFiles: 2, MaxEventBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 20; index++ {
		if err := sink.Append(context.Background(), testEvent(uint64(index), strings.Repeat("r", 220))); err != nil {
			t.Fatalf("Append(%d) error = %v", index, err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= 3; index++ {
		candidate := path
		if index > 0 {
			candidate = rotatedAuditPath(path, index)
		}
		_, statErr := os.Stat(candidate)
		if index == 3 {
			if statErr == nil {
				t.Fatalf("rotation exceeded MaxFiles: %s exists", candidate)
			}
			continue
		}
		if statErr != nil {
			t.Fatalf("expected rotation file %s: %v", candidate, statErr)
		}
		data, readErr := os.ReadFile(candidate)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for lineIndex, line := range auditJSONLines(data) {
			var event Event
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("%s line %d is invalid JSON: %v", candidate, lineIndex+1, err)
			}
		}
	}
}

func TestOpenFileSinkRejectsPathLinksForDataKeyAndLock(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}

	parentLink := filepath.Join(root, "parent-link")
	if err := os.Symlink(outside, parentLink); err != nil {
		t.Skipf("symlink unavailable on %s: %v", runtime.GOOS, err)
	}
	if _, err := OpenFileSink(FileOptions{Path: filepath.Join(parentLink, "events.jsonl")}); err == nil {
		t.Fatal("OpenFileSink() followed a parent symlink")
	}

	dataPath := filepath.Join(root, "events.jsonl")
	keyTarget := filepath.Join(outside, "key")
	if err := os.WriteFile(keyTarget, []byte(strings.Repeat("cd", identifierKeyBytes)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	keyLink := filepath.Join(root, "key-link")
	if err := os.Symlink(keyTarget, keyLink); err != nil {
		t.Skipf("symlink unavailable on %s: %v", runtime.GOOS, err)
	}
	if _, err := OpenFileSink(FileOptions{Path: dataPath, KeyPath: keyLink}); err == nil {
		t.Fatal("OpenFileSink() followed a key symlink")
	}

	lockTarget := filepath.Join(outside, "lock")
	if err := os.WriteFile(lockTarget, nil, 0600); err != nil {
		t.Fatal(err)
	}
	lockLink := dataPath + ".lock"
	if err := os.Symlink(lockTarget, lockLink); err != nil {
		t.Skipf("symlink unavailable on %s: %v", runtime.GOOS, err)
	}
	if _, err := OpenFileSink(FileOptions{Path: dataPath}); err == nil {
		t.Fatal("OpenFileSink() followed a lock symlink")
	}
}
