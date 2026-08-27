package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingSink struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (sink *recordingSink) Append(_ context.Context, event Event) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.err != nil {
		return sink.err
	}
	sink.events = append(sink.events, event)
	return nil
}

func (sink *recordingSink) snapshot() []Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]Event(nil), sink.events...)
}

type deterministicReader struct {
	value byte
}

func (reader *deterministicReader) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = reader.value
	}
	return len(p), nil
}

func newTestSession(t *testing.T, sink Sink, policy FailurePolicy, detail Detail) *Session {
	t.Helper()
	if sink == nil {
		sink = &recordingSink{}
	}
	session, err := NewSession(SessionOptions{
		Sink:              sink,
		Detail:            detail,
		FailurePolicy:     policy,
		Operation:         "report.prune",
		Command:           "dm prune --apply",
		Profile:           "prod",
		Endpoint:          "tcp://admin:password@example.invalid:2376?token=secret",
		Operator:          Operator{OSUser: "root", UIDOrSID: "0", Hostname: "node-1", AssertedActor: "ci"},
		IdentifierKey:     bytesOf(0x42, identifierKeyBytes),
		CandidatePageSize: 2,
		MaxCandidates:     8,
		Clock: func() time.Time {
			return time.Date(2026, time.August, 27, 1, 2, 3, 456000000, time.UTC)
		},
		Random: &deterministicReader{value: 0x11},
	})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	return session
}

func bytesOf(value byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return result
}

func TestSessionStartAuthorizeFinishSafeEvents(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(t, sink, FailureDenyMutation, DetailSafe)
	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	authorization, err := session.AuthorizeMutation(context.Background(), MutationRequest{
		Scope:        MutationDockerPersistent,
		Confirmation: Confirmation{Required: true, Provided: true, Mechanism: "--apply+--confirm"},
		Candidates: []CandidateInput{
			{Kind: "container", Action: "delete", Identifier: "container-id-1", Display: "web"},
			{Kind: "image", Action: "delete", Identifier: "sha256:abc", Display: "private-image"},
			{Kind: "volume", Action: "delete", Identifier: "volume-secret-name", Display: "volume-secret-name"},
		},
	})
	if err != nil {
		t.Fatalf("AuthorizeMutation() error = %v", err)
	}
	if !authorization.Allowed || !authorization.Audited || authorization.CandidateSetID == "" {
		t.Fatalf("authorization = %#v, want audited allowed authorization", authorization)
	}
	if err := session.Finish(context.Background(), FinishResult{Result: ResultSummary{Success: 3}}); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	events := sink.snapshot()
	if len(events) != 5 {
		t.Fatalf("event count = %d, want start + 2 candidate pages + authorized + finish", len(events))
	}
	if events[0].Type != EventCommandStart || events[len(events)-1].Type != EventCommandFinish {
		t.Fatalf("event types = %#v", eventTypes(events))
	}
	for index, event := range events {
		if event.Schema != SchemaVersion || event.RunID == "" || event.Sequence != uint64(index+1) {
			t.Errorf("event[%d] envelope = %#v", index, event)
		}
		if strings.Contains(event.Endpoint.ID, "password") || strings.Contains(event.Endpoint.ID, "secret") {
			t.Errorf("event[%d] endpoint leaked raw value: %#v", index, event.Endpoint)
		}
		data, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(data), "container-id-1") || strings.Contains(string(data), "volume-secret-name") || strings.Contains(string(data), "private-image") {
			t.Errorf("safe event[%d] leaked candidate value: %s", index, data)
		}
	}
	if events[1].CandidatePage != 1 || events[1].CandidatePageCount != 2 || events[2].CandidatePage != 2 || !events[2].CandidateFinal {
		t.Fatalf("candidate pages = %#v, %#v", events[1], events[2])
	}
	if events[3].Type != EventMutationAuthorized || events[3].CandidateSetID != authorization.CandidateSetID {
		t.Fatalf("authorized event = %#v", events[3])
	}
	if events[4].Outcome != OutcomeSuccess || events[4].Result == nil || events[4].Result.Success != 3 || events[4].AuthorizedMutations != 1 {
		t.Fatalf("finish event = %#v", events[4])
	}
}

func eventTypes(events []Event) []EventType {
	result := make([]EventType, len(events))
	for index, event := range events {
		result[index] = event.Type
	}
	return result
}

func TestSessionFullDetailRedactsAndBoundsText(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(t, sink, FailureRequired, DetailFull)
	if err := session.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := session.AuthorizeMutation(context.Background(), MutationRequest{
		Scope:        MutationDockerTemporary,
		Confirmation: Confirmation{Provided: true},
		Candidates:   []CandidateInput{{Kind: "probe", Action: "create", Identifier: "probe-1", Display: "token=super-secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishErr := errors.New("request failed authorization=Bearer abc password=hunter2")
	if err := session.Finish(context.Background(), FinishResult{Err: finishErr}); err != nil {
		t.Fatal(err)
	}
	events := sink.snapshot()
	finish := events[len(events)-1]
	if finish.Error == nil || finish.Error.Message == "" {
		t.Fatalf("finish error = %#v, want bounded full message", finish.Error)
	}
	if strings.Contains(finish.Error.Message, "abc") || strings.Contains(finish.Error.Message, "hunter2") {
		t.Fatalf("full detail leaked secret: %q", finish.Error.Message)
	}
	if len(finish.Error.Message) > maxErrorText {
		t.Fatalf("error message bytes = %d, want <= %d", len(finish.Error.Message), maxErrorText)
	}
	if events[1].Candidates[0].Display != "token=<redacted>" {
		t.Fatalf("candidate display = %q, want strict redaction", events[1].Candidates[0].Display)
	}
}

func TestAuthorizeMutationRequiresConfirmationAndRecordsRejection(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(t, sink, FailureDenyMutation, DetailSafe)
	authorization, err := session.AuthorizeMutation(context.Background(), MutationRequest{
		Scope:        MutationDockerPersistent,
		Confirmation: Confirmation{Required: true, Provided: false, Mechanism: "--apply+--confirm"},
		Candidates:   []CandidateInput{{Kind: "image", Action: "delete", Identifier: "image-id"}},
	})
	if !errors.Is(err, ErrConfirmationRequired) || authorization.Allowed {
		t.Fatalf("authorization=%#v err=%v, want rejection", authorization, err)
	}
	events := sink.snapshot()
	if len(events) != 2 || events[0].Type != EventCommandStart || events[1].Type != EventMutationRejected {
		t.Fatalf("events = %#v", events)
	}
	if events[1].Error == nil || events[1].Outcome != OutcomeRejected {
		t.Fatalf("rejection event = %#v", events[1])
	}
}

func TestFailurePolicies(t *testing.T) {
	tests := []struct {
		name          string
		policy        FailurePolicy
		wantErr       bool
		wantAllowed   bool
		wantAudited   bool
		wantWarnCount int
	}{
		{name: "warn", policy: FailureWarn, wantAllowed: true, wantAudited: false, wantWarnCount: 1},
		{name: "deny mutation", policy: FailureDenyMutation, wantErr: true, wantWarnCount: 0},
		{name: "required", policy: FailureRequired, wantErr: true, wantWarnCount: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sink := &recordingSink{err: errors.New("disk full")}
			var warnings []error
			session, err := NewSession(SessionOptions{
				Sink:          sink,
				FailurePolicy: test.policy,
				Operation:     "image.load",
				Endpoint:      "unix:///var/run/docker.sock",
				IdentifierKey: bytesOf(1, identifierKeyBytes),
				Warning: func(err error) {
					warnings = append(warnings, err)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			authorization, authErr := session.AuthorizeMutation(context.Background(), MutationRequest{
				Scope:        MutationDockerPersistent,
				Confirmation: Confirmation{Provided: true},
				Candidates:   []CandidateInput{{Kind: "image", Action: "load", Identifier: "id"}},
			})
			if test.wantErr != (authErr != nil) {
				t.Fatalf("AuthorizeMutation error=%v, wantErr=%v", authErr, test.wantErr)
			}
			if !test.wantErr && (authorization.Allowed != test.wantAllowed || authorization.Audited != test.wantAudited) {
				t.Fatalf("authorization=%#v", authorization)
			}
			if len(warnings) != test.wantWarnCount {
				t.Fatalf("warning count=%d, want %d", len(warnings), test.wantWarnCount)
			}
		})
	}
}

func TestSessionRejectsCandidateOverflowAndInvalidTokens(t *testing.T) {
	session := newTestSession(t, &recordingSink{}, FailureRequired, DetailSafe)
	tooMany := make([]CandidateInput, session.maxCandidates+1)
	for index := range tooMany {
		tooMany[index] = CandidateInput{Kind: "image", Action: "delete", Identifier: fmt.Sprintf("id-%d", index)}
	}
	if _, err := session.AuthorizeMutation(context.Background(), MutationRequest{Scope: MutationDockerPersistent, Confirmation: Confirmation{Provided: true}, Candidates: tooMany}); err == nil {
		t.Fatal("AuthorizeMutation() accepted too many candidates")
	}
	if _, err := session.AuthorizeMutation(context.Background(), MutationRequest{Scope: MutationDockerPersistent, Confirmation: Confirmation{Provided: true}, Candidates: []CandidateInput{{Kind: "bad kind", Action: "delete", Identifier: "id"}}}); err == nil {
		t.Fatal("AuthorizeMutation() accepted invalid candidate kind")
	}
}

func TestSessionContextRoundTrip(t *testing.T) {
	session := newTestSession(t, &recordingSink{}, FailureWarn, DetailSafe)
	ctx := WithSession(context.Background(), session)
	if got := FromContext(ctx); got != session {
		t.Fatalf("FromContext() = %p, want %p", got, session)
	}
	if FromContext(context.Background()) != nil {
		t.Fatal("FromContext() returned session for context without one")
	}
}
