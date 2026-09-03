package pull

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"docker-manager/internal/audit"

	"github.com/Yui100901/MyGo/network/http_utils"
	digest "github.com/opencontainers/go-digest"
)

type pullAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (sink *pullAuditSink) Append(_ context.Context, event audit.Event) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.err != nil {
		return sink.err
	}
	sink.events = append(sink.events, event)
	return nil
}

func (sink *pullAuditSink) snapshot() []audit.Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]audit.Event(nil), sink.events...)
}

func newPullAuditSession(t *testing.T, sink audit.Sink, policy audit.FailurePolicy) *audit.Session {
	return newPullAuditSessionWithDetail(t, sink, policy, audit.DetailSafe)
}

func newPullAuditSessionWithDetail(t *testing.T, sink audit.Sink, policy audit.FailurePolicy, detail audit.Detail) *audit.Session {
	t.Helper()
	session, err := audit.NewSession(audit.SessionOptions{
		Sink:          sink,
		Detail:        detail,
		FailurePolicy: policy,
		Operation:     "pull",
		Command:       "dm pull",
		Endpoint:      "unix:///var/run/docker.sock",
		IdentifierKey: bytes.Repeat([]byte{0x41}, 32),
		Random:        bytes.NewReader(bytes.Repeat([]byte{0x42}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestCompletePulledImageAuditsLoadBeforeDockerMutation(t *testing.T) {
	sink := &pullAuditSink{}
	session := newPullAuditSession(t, sink, audit.FailureRequired)
	ctx := audit.WithSession(context.Background(), session)
	runner := newTestPullRunner()
	var loadedPath string
	runner.loadPulledImage = func(_ context.Context, path string, _ io.Writer) error {
		loadedPath = path
		return nil
	}
	if err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{Context: ctx, Load: true}); err != nil {
		t.Fatalf("completePulledImage() error = %v", err)
	}
	if loadedPath != "busybox.tar" {
		t.Fatalf("loadedPath = %q, want busybox.tar", loadedPath)
	}
	events := sink.snapshot()
	if len(events) != 3 || events[0].Type != audit.EventCommandStart || events[1].Type != audit.EventMutationCandidates || events[2].Type != audit.EventMutationAuthorized {
		t.Fatalf("audit events = %#v, want start/candidates/authorized", auditEventTypes(events))
	}
	if events[2].Mutation == nil || events[2].Mutation.Scope != audit.MutationDockerPersistent {
		t.Fatalf("authorized mutation = %#v, want docker_persistent", events[2].Mutation)
	}
	if len(events[1].Candidates) != 2 || events[1].Candidates[0].Action != "write" || events[1].Candidates[1].Action != "load" {
		t.Fatalf("candidate event = %#v", events[1])
	}
}

func TestCompletePulledImageRejectsLoadWhenAuditUnavailable(t *testing.T) {
	sink := &pullAuditSink{err: errors.New("audit disk full")}
	session := newPullAuditSession(t, sink, audit.FailureDenyMutation)
	ctx := audit.WithSession(context.Background(), session)
	runner := newTestPullRunner()
	var called bool
	runner.loadPulledImage = func(_ context.Context, _ string, _ io.Writer) error {
		called = true
		return nil
	}
	err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{Context: ctx, Load: true})
	if err == nil {
		t.Fatal("completePulledImage() error = nil, want audit failure")
	}
	if called {
		t.Fatal("Docker load was called after audit authorization failure")
	}
	if !strings.Contains(err.Error(), "审计授权失败") {
		t.Fatalf("error = %q, want audit authorization context", err)
	}
}

func TestAuthorizePulledImageMutationAuditsPushAsExternalOperation(t *testing.T) {
	sink := &pullAuditSink{}
	session := newPullAuditSessionWithDetail(t, sink, audit.FailureRequired, audit.DetailFull)
	ctx := audit.WithSession(context.Background(), session)
	target := "registry.example/team/busybox:latest"
	if err := authorizePulledImageMutation(ctx, "busybox.tar", target, PullOptions{To: "registry.example"}); err != nil {
		t.Fatalf("authorizePulledImageMutation() error = %v", err)
	}
	events := sink.snapshot()
	if len(events) != 3 {
		t.Fatalf("audit event count = %d, want 3", len(events))
	}
	authorized := events[len(events)-1]
	if authorized.Type != audit.EventMutationAuthorized || authorized.Mutation == nil || authorized.Mutation.Scope != audit.MutationExternalOperation {
		t.Fatalf("authorized event = %#v, want external operation", authorized)
	}
	candidates := events[1].Candidates
	want := []struct {
		kind    string
		action  string
		display string
	}{
		{kind: "image-archive", action: "write", display: "busybox.tar"},
		{kind: "image-archive", action: "load", display: "busybox.tar"},
		{kind: "image", action: "tag", display: target},
		{kind: "image", action: "push", display: target},
	}
	if len(candidates) != len(want) {
		t.Fatalf("candidate count = %d, want %d: %#v", len(candidates), len(want), candidates)
	}
	for index, expected := range want {
		if candidates[index].Kind != expected.kind || candidates[index].Action != expected.action || candidates[index].Display != expected.display {
			t.Fatalf("candidate %d = %#v, want kind=%q action=%q display=%q", index, candidates[index], expected.kind, expected.action, expected.display)
		}
	}
}

func TestGetImageAuditFailurePreventsArchivePublication(t *testing.T) {
	configBody := []byte(`{"architecture":"amd64","os":"linux"}`)
	manifestBody := testManifestBytes(t, configBody)
	configDigest := digest.FromBytes(configBody)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/v2/team/app/manifests/v1":
			_, _ = w.Write(manifestBody)
		case "/v2/team/app/blobs/" + configDigest.String():
			_, _ = w.Write(configBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	var dockerCalls []string
	runner.loadPulledImage = func(context.Context, string, io.Writer) error {
		dockerCalls = append(dockerCalls, "load")
		return nil
	}
	runner.tagPulledImage = func(context.Context, string, string) error {
		dockerCalls = append(dockerCalls, "tag")
		return nil
	}
	runner.pushPulledImage = func(context.Context, string, string, io.Writer) error {
		dockerCalls = append(dockerCalls, "push")
		return nil
	}
	session := newPullAuditSession(t, &pullAuditSink{err: errors.New("audit unavailable")}, audit.FailureDenyMutation)
	outputPath := filepath.Join(t.TempDir(), "denied-image.tar")
	imageName := strings.TrimPrefix(server.URL, "http://") + "/team/app:v1"
	err := runner.getImage(imageName, PullOptions{
		Context:   audit.WithSession(context.Background(), session),
		Output:    outputPath,
		To:        server.URL + "/mirror",
		PlainHTTP: true,
	})
	if err == nil || !strings.Contains(err.Error(), "审计授权失败") {
		t.Fatalf("getImage() error = %v, want audit authorization failure", err)
	}
	if _, statErr := os.Stat(outputPath); !os.IsNotExist(statErr) {
		t.Fatalf("archive stat error = %v, output must not exist after denied audit", statErr)
	}
	if len(dockerCalls) != 0 {
		t.Fatalf("Docker mutations after denied audit = %#v, want none", dockerCalls)
	}
}

func TestPullBatchAuditsStateAndReportPathsOnce(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	reportPath := filepath.Join(dir, "report.json")
	sink := &pullAuditSink{}
	session, err := audit.NewSession(audit.SessionOptions{
		Sink:          sink,
		Detail:        audit.DetailFull,
		FailurePolicy: audit.FailureRequired,
		Operation:     "pull",
		Command:       "dm pull",
		Endpoint:      "unix:///var/run/docker.sock",
		IdentifierKey: bytes.Repeat([]byte{0x41}, 32),
		Random:        bytes.NewReader(bytes.Repeat([]byte{0x42}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := audit.WithSession(context.Background(), session)
	_, err = runPullBatchWithDeps(ctx, PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   statePath,
		ReportFile:  reportPath,
		Concurrency: 1,
	}, func(image string, opts PullOptions) error {
		return writePullBatchTestArtifact(opts, image)
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatalf("runPullBatchWithDeps() error = %v", err)
	}

	events := sink.snapshot()
	if len(events) != 3 || events[0].Type != audit.EventCommandStart || events[1].Type != audit.EventMutationCandidates || events[2].Type != audit.EventMutationAuthorized {
		t.Fatalf("audit events = %#v, want one start/candidates/authorized sequence", auditEventTypes(events))
	}
	if events[2].Mutation == nil || events[2].Mutation.Scope != audit.MutationFilesystem || events[2].Mutation.Confirmation.Mechanism != "pull-batch-files" {
		t.Fatalf("authorized mutation = %#v, want pull batch filesystem authorization", events[2].Mutation)
	}
	candidates := events[1].Candidates
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d, want state and report", len(candidates))
	}
	if candidates[0].Kind != "pull-state" || candidates[0].Action != "write" || candidates[0].Display != filepath.Clean(statePath) {
		t.Fatalf("state candidate = %#v", candidates[0])
	}
	if candidates[1].Kind != "pull-report" || candidates[1].Action != "write" || candidates[1].Display != filepath.Clean(reportPath) {
		t.Fatalf("report candidate = %#v", candidates[1])
	}
}

func TestPullBatchAuditFailurePreventsWorkAndMetadataWrites(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "pull-state.json")
	reportPath := filepath.Join(dir, "report.json")
	session := newPullAuditSession(t, &pullAuditSink{err: errors.New("audit unavailable")}, audit.FailureDenyMutation)
	ctx := audit.WithSession(context.Background(), session)
	pullCalled := false
	existsCalled := false

	_, err := runPullBatchWithDeps(ctx, PullBatchOptions{
		Images:       []string{"busybox:latest"},
		To:           "registry.example/team",
		OutputDir:    dir,
		ReportFile:   reportPath,
		Concurrency:  1,
		SkipExisting: true,
	}, func(string, PullOptions) error {
		pullCalled = true
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		existsCalled = true
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "审计授权失败") {
		t.Fatalf("runPullBatchWithDeps() error = %v, want audit authorization failure", err)
	}
	if pullCalled || existsCalled {
		t.Fatalf("callbacks after denied audit: pull=%v exists=%v", pullCalled, existsCalled)
	}
	for _, path := range []string{statePath, statePath + ".tmp", reportPath, reportPath + ".tmp"} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("metadata path %q stat error = %v, want not exist", path, statErr)
		}
	}
}

func auditEventTypes(events []audit.Event) []audit.EventType {
	result := make([]audit.EventType, len(events))
	for index, event := range events {
		result[index] = event.Type
	}
	return result
}
