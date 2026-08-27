package images

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

	"github.com/moby/moby/api/types/image"
)

type imageAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (sink *imageAuditSink) Append(_ context.Context, event audit.Event) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.err != nil {
		return sink.err
	}
	sink.events = append(sink.events, event)
	return nil
}

func (sink *imageAuditSink) snapshot() []audit.Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]audit.Event(nil), sink.events...)
}

func newImageAuditSession(t *testing.T, sink audit.Sink, policy audit.FailurePolicy) *audit.Session {
	t.Helper()
	session, err := audit.NewSession(audit.SessionOptions{
		Sink:          sink,
		Detail:        audit.DetailFull,
		FailurePolicy: policy,
		Operation:     "image.save",
		Command:       "dm image save",
		Endpoint:      "unix:///var/run/docker.sock",
		IdentifierKey: bytes.Repeat([]byte{0x61}, 32),
		Random:        bytes.NewReader(bytes.Repeat([]byte{0x62}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestSaveImagesAuditsBeforeWritingArchive(t *testing.T) {
	tests := []struct {
		name      string
		merge     bool
		wantFiles func(string) []string
		wantSaves int
	}{
		{
			name: "one archive per image",
			wantFiles: func(outputDir string) []string {
				return []string{
					filepath.Join(outputDir, "repo_app-v1.tar"),
					filepath.Join(outputDir, "team_worker-v2.tar"),
				}
			},
			wantSaves: 2,
		},
		{
			name:  "merged archive",
			merge: true,
			wantFiles: func(outputDir string) []string {
				return []string{filepath.Join(outputDir, "images.tar")}
			},
			wantSaves: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &fakeImageManager{images: []image.Summary{
				{ID: "sha256:one", RepoTags: []string{"repo/app:v1"}},
				{ID: "sha256:two", RepoTags: []string{"team/worker:v2"}},
			}}
			withFakeImageManager(t, manager)
			sink := &imageAuditSink{}
			session := newImageAuditSession(t, sink, audit.FailureRequired)
			ctx := audit.WithSession(context.Background(), session)
			outputDir := filepath.Join(t.TempDir(), "exports")
			if err := saveImagesWithOptions(ctx, outputDir, SaveOptions{Merge: test.merge}); err != nil {
				t.Fatalf("saveImagesWithOptions() error = %v", err)
			}
			if len(manager.saveCalls) != test.wantSaves {
				t.Fatalf("Save calls = %d, want %d", len(manager.saveCalls), test.wantSaves)
			}
			events := sink.snapshot()
			if len(events) != 3 || events[1].Type != audit.EventMutationCandidates || events[2].Type != audit.EventMutationAuthorized {
				t.Fatalf("audit events = %#v, want start/candidates/authorized", imageAuditEventTypes(events))
			}
			if events[2].Mutation == nil || events[2].Mutation.Scope != audit.MutationFilesystem {
				t.Fatalf("authorized mutation = %#v, want filesystem", events[2].Mutation)
			}
			wantFiles := test.wantFiles(outputDir)
			candidates := events[1].Candidates
			if len(candidates) != len(wantFiles) {
				t.Fatalf("candidate count = %d, want %d: %#v", len(candidates), len(wantFiles), candidates)
			}
			for index, wantFile := range wantFiles {
				candidate := candidates[index]
				if candidate.Kind != "image-archive" || candidate.Action != "write" || candidate.Display != wantFile {
					t.Fatalf("candidate %d = %#v, want image-archive/write display=%q", index, candidate, wantFile)
				}
				if manager.saveCalls[index].outputFile != wantFile {
					t.Fatalf("Save output %d = %q, want audited path %q", index, manager.saveCalls[index].outputFile, wantFile)
				}
			}
		})
	}
}

func TestSaveImagesAuditFailurePreventsArchiveWrite(t *testing.T) {
	manager := &fakeImageManager{images: []image.Summary{{ID: "sha256:one", RepoTags: []string{"repo/app:v1"}}}}
	withFakeImageManager(t, manager)
	sink := &imageAuditSink{err: errors.New("audit unavailable")}
	session := newImageAuditSession(t, sink, audit.FailureDenyMutation)
	outputDir := filepath.Join(t.TempDir(), "denied-output")
	err := saveImagesWithOptions(audit.WithSession(context.Background(), session), outputDir, SaveOptions{})
	if err == nil {
		t.Fatal("saveImagesWithOptions() error = nil, want audit authorization failure")
	}
	if len(manager.saveCalls) != 0 {
		t.Fatalf("Save calls = %d, want 0 after denied audit", len(manager.saveCalls))
	}
	if _, statErr := os.Stat(outputDir); !os.IsNotExist(statErr) {
		t.Fatalf("output directory stat error = %v, directory must not exist after denied audit", statErr)
	}
}

func TestSaveImagesDryRunSkipsAuditAuthorization(t *testing.T) {
	manager := &fakeImageManager{images: []image.Summary{{ID: "sha256:one", RepoTags: []string{"repo/app:v1"}}}}
	withFakeImageManager(t, manager)
	sink := &imageAuditSink{}
	session := newImageAuditSession(t, sink, audit.FailureRequired)
	if err := saveImagesWithOptions(audit.WithSession(context.Background(), session), t.TempDir(), SaveOptions{DryRun: true}); err != nil {
		t.Fatalf("saveImagesWithOptions() error = %v", err)
	}
	if len(sink.snapshot()) != 0 {
		t.Fatal("dry-run emitted mutation audit events")
	}
	if len(manager.saveCalls) != 0 {
		t.Fatalf("Save calls = %d, want 0 for dry-run", len(manager.saveCalls))
	}
}

func TestLoadCommandAuditFailurePreventsManagerLoad(t *testing.T) {
	manager := &fakeImageManager{}
	withFakeImageManager(t, manager)
	archive := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(archive, []byte("not read before authorization"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := newImageAuditSession(t, &imageAuditSink{err: errors.New("audit unavailable")}, audit.FailureDenyMutation)
	cmd := NewLoadCommand()
	cmd.SetContext(audit.WithSession(context.Background(), session))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{archive})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "审计授权失败") {
		t.Fatalf("Execute() error = %v, want audit authorization failure", err)
	}
	if len(manager.loadCalls) != 0 {
		t.Fatalf("Load calls = %#v, want none after denied audit", manager.loadCalls)
	}
}

func imageAuditEventTypes(events []audit.Event) []audit.EventType {
	result := make([]audit.EventType, len(events))
	for index, event := range events {
		result[index] = event.Type
	}
	return result
}
