package audit

import (
	"context"
	"io"
	"time"
)

const (
	SchemaVersion            = "dm.audit/v1"
	DefaultCandidatePageSize = 64
	DefaultMaxCandidates     = 100_000
	DefaultMaxEventBytes     = 64 * 1024
)

type EventType string

const (
	EventCommandStart       EventType = "command.start"
	EventMutationCandidates EventType = "operation.candidates"
	EventMutationAuthorized EventType = "operation.authorized"
	EventMutationRejected   EventType = "operation.rejected"
	EventCommandFinish      EventType = "command.finish"
)

type Detail string

const (
	DetailSafe Detail = "safe"
	DetailFull Detail = "full"
)

type FailurePolicy string

const (
	FailureWarn         FailurePolicy = "warn"
	FailureDenyMutation FailurePolicy = "deny-mutation"
	FailureRequired     FailurePolicy = "required"
)

type MutationScope string

const (
	MutationNone              MutationScope = "none"
	MutationDockerPersistent  MutationScope = "docker_persistent"
	MutationDockerTemporary   MutationScope = "docker_temporary"
	MutationFilesystem        MutationScope = "filesystem"
	MutationExternalOperation MutationScope = "external"
)

func (scope MutationScope) Mutates() bool {
	return scope != "" && scope != MutationNone
}

type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomePartial  Outcome = "partial"
	OutcomeFailed   Outcome = "failed"
	OutcomeCanceled Outcome = "canceled"
	OutcomeRejected Outcome = "rejected"
	OutcomeNoop     Outcome = "noop"
)

type Operator struct {
	OSUser        string `json:"os_user,omitempty"`
	UIDOrSID      string `json:"uid_or_sid,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	AssertedActor string `json:"asserted_actor,omitempty"`
}

type Endpoint struct {
	Scheme string `json:"scheme"`
	ID     string `json:"id"`
}

type Confirmation struct {
	Required  bool   `json:"required"`
	Provided  bool   `json:"provided"`
	Mechanism string `json:"mechanism,omitempty"`
}

type Mutation struct {
	Scope        MutationScope `json:"scope"`
	Confirmation Confirmation  `json:"confirmation"`
}

// CandidateInput is accepted from command packages. Identifier and Display are
// never copied directly into a safe audit event.
type CandidateInput struct {
	Kind       string
	Action     string
	Identifier string
	Display    string
}

type Candidate struct {
	Kind    string `json:"kind"`
	Action  string `json:"action"`
	ID      string `json:"id"`
	Display string `json:"display,omitempty"`
}

type CandidateSummary struct {
	Total  int            `json:"total"`
	ByKind map[string]int `json:"by_kind,omitempty"`
}

type ResultSummary struct {
	Success int            `json:"success,omitempty"`
	Failed  int            `json:"failed,omitempty"`
	Unknown int            `json:"unknown,omitempty"`
	Skipped int            `json:"skipped,omitempty"`
	ByKind  map[string]int `json:"by_kind,omitempty"`
}

type ErrorInfo struct {
	Class   string `json:"class"`
	ID      string `json:"id"`
	Message string `json:"message,omitempty"`
}

type Event struct {
	Schema    string    `json:"schema"`
	Type      EventType `json:"type"`
	Time      string    `json:"time"`
	RunID     string    `json:"run_id"`
	Sequence  uint64    `json:"sequence"`
	Operation string    `json:"operation"`
	Command   string    `json:"command,omitempty"`

	Operator Operator `json:"operator"`
	Profile  string   `json:"profile,omitempty"`
	Endpoint Endpoint `json:"endpoint"`

	Mutation           *Mutation         `json:"mutation,omitempty"`
	CandidateSetID     string            `json:"candidate_set_id,omitempty"`
	CandidatePage      int               `json:"candidate_page,omitempty"`
	CandidatePageCount int               `json:"candidate_page_count,omitempty"`
	CandidateFinal     bool              `json:"candidate_final,omitempty"`
	Candidates         []Candidate       `json:"candidates,omitempty"`
	CandidateSummary   *CandidateSummary `json:"candidate_summary,omitempty"`

	Outcome             Outcome        `json:"outcome,omitempty"`
	DurationMS          int64          `json:"duration_ms,omitempty"`
	Result              *ResultSummary `json:"result,omitempty"`
	Error               *ErrorInfo     `json:"error,omitempty"`
	AuthorizedMutations int            `json:"authorized_mutations,omitempty"`
}

type Sink interface {
	Append(context.Context, Event) error
}

type IdentifierKeyProvider interface {
	IdentifierKey() []byte
}

type SessionOptions struct {
	Sink          Sink
	Detail        Detail
	FailurePolicy FailurePolicy
	Operation     string
	Command       string
	Profile       string
	Endpoint      string
	Operator      Operator
	IdentifierKey []byte

	CandidatePageSize int
	MaxCandidates     int
	Warning           func(error)

	// Clock and Random are injectable to make event lifecycle tests deterministic.
	Clock  func() time.Time
	Random io.Reader
}

type MutationRequest struct {
	Scope        MutationScope
	Confirmation Confirmation
	Candidates   []CandidateInput
}

type Authorization struct {
	Allowed        bool
	Audited        bool
	CandidateSetID string
	Summary        CandidateSummary
}

type FinishResult struct {
	Outcome Outcome
	Result  ResultSummary
	Err     error
}

type FileOptions struct {
	Path          string
	KeyPath       string
	MaxBytes      int64
	MaxFiles      int
	MaxEventBytes int
	LockTimeout   time.Duration
}
