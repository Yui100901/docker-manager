package audit

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrConfirmationRequired = errors.New("audit mutation confirmation is required")
	ErrSessionFinished      = errors.New("audit session is already finished")
)

type WriteError struct {
	Stage string
	Err   error
}

func (err *WriteError) Error() string {
	return fmt.Sprintf("write audit %s event: %v", err.Stage, err.Err)
}

func (err *WriteError) Unwrap() error {
	return err.Err
}

type Session struct {
	mu sync.Mutex

	sink          Sink
	detail        Detail
	failurePolicy FailurePolicy
	operation     string
	command       string
	profile       string
	operator      Operator
	endpoint      Endpoint
	identifierKey []byte
	pageSize      int
	maxCandidates int
	warning       func(error)
	clock         func() time.Time
	random        io.Reader

	runID               string
	sequence            uint64
	startedAt           time.Time
	started             bool
	finished            bool
	finishErr           error
	authorizedMutations int
	warned              bool
}

func NewSession(opts SessionOptions) (*Session, error) {
	detail := opts.Detail
	if detail == "" {
		detail = DetailSafe
	}
	if detail != DetailSafe && detail != DetailFull {
		return nil, fmt.Errorf("unsupported audit detail %q", detail)
	}
	policy := opts.FailurePolicy
	if policy == "" {
		policy = FailureDenyMutation
	}
	if policy != FailureWarn && policy != FailureDenyMutation && policy != FailureRequired {
		return nil, fmt.Errorf("unsupported audit failure policy %q", policy)
	}
	operation := strings.TrimSpace(opts.Operation)
	if operation == "" || len(operation) > maxOperationBytes {
		return nil, fmt.Errorf("audit operation must contain 1 to %d bytes", maxOperationBytes)
	}
	if !validOperation(operation) {
		return nil, fmt.Errorf("audit operation %q contains unsupported characters", operation)
	}

	pageSize := opts.CandidatePageSize
	if pageSize == 0 {
		pageSize = DefaultCandidatePageSize
	}
	if pageSize < 1 || pageSize > DefaultCandidatePageSize {
		return nil, fmt.Errorf("audit candidate page size must be between 1 and %d", DefaultCandidatePageSize)
	}
	maxCandidates := opts.MaxCandidates
	if maxCandidates == 0 {
		maxCandidates = DefaultMaxCandidates
	}
	if maxCandidates < 1 || maxCandidates > DefaultMaxCandidates {
		return nil, fmt.Errorf("audit max candidates must be between 1 and %d", DefaultMaxCandidates)
	}

	randomReader := opts.Random
	if randomReader == nil {
		randomReader = rand.Reader
	}
	key := append([]byte(nil), opts.IdentifierKey...)
	if len(key) == 0 {
		if provider, ok := opts.Sink.(IdentifierKeyProvider); ok {
			key = provider.IdentifierKey()
		}
	}
	if len(key) == 0 {
		var err error
		key, err = newIdentifierKey(randomReader)
		if err != nil {
			return nil, err
		}
	}
	if len(key) < identifierKeyBytes {
		return nil, fmt.Errorf("audit identifier key must contain at least %d bytes", identifierKeyBytes)
	}

	runID, err := randomAuditID(randomReader)
	if err != nil {
		return nil, err
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	operator := opts.Operator
	if operator.OSUser == "" && operator.UIDOrSID == "" && operator.Hostname == "" {
		operator = CurrentOperator(operator.AssertedActor)
	} else {
		operator.OSUser = sanitizeAuditText(operator.OSUser, maxActorBytes)
		operator.UIDOrSID = sanitizeAuditText(operator.UIDOrSID, maxActorBytes)
		operator.Hostname = sanitizeAuditText(operator.Hostname, maxActorBytes)
		operator.AssertedActor = sanitizeAuditText(operator.AssertedActor, maxActorBytes)
	}

	startedAt := clock()
	return &Session{
		sink:          opts.Sink,
		detail:        detail,
		failurePolicy: policy,
		operation:     operation,
		command:       sanitizeAuditText(opts.Command, maxCommandBytes),
		profile:       sanitizeAuditText(opts.Profile, maxProfileBytes),
		operator:      operator,
		endpoint:      safeEndpoint(opts.Endpoint, key),
		identifierKey: append([]byte(nil), key...),
		pageSize:      pageSize,
		maxCandidates: maxCandidates,
		warning:       opts.Warning,
		clock:         clock,
		random:        randomReader,
		runID:         runID,
		startedAt:     startedAt,
	}, nil
}

func (session *Session) Enabled() bool {
	return session != nil && session.sink != nil
}

func (session *Session) RunID() string {
	if session == nil {
		return ""
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.runID
}

func (session *Session) Start(ctx context.Context) error {
	if session == nil || session.sink == nil {
		return nil
	}
	session.mu.Lock()
	if session.finished {
		session.mu.Unlock()
		return ErrSessionFinished
	}
	err := session.startRawLocked(ctx)
	resultErr, warning := session.applyFailurePolicyLocked("start", MutationNone, err)
	session.mu.Unlock()
	session.notifyWarning(warning)
	return resultErr
}

func (session *Session) AuthorizeMutation(ctx context.Context, request MutationRequest) (Authorization, error) {
	if session == nil {
		return Authorization{Allowed: true}, nil
	}
	candidates, summary, err := session.prepareCandidates(request.Candidates)
	if err != nil {
		return Authorization{}, err
	}
	request.Confirmation.Mechanism = sanitizeAuditText(request.Confirmation.Mechanism, maxCommandBytes)
	if !validMutationScope(request.Scope) || !request.Scope.Mutates() {
		return Authorization{}, fmt.Errorf("unsupported audit mutation scope %q", request.Scope)
	}

	session.mu.Lock()
	if session.finished {
		session.mu.Unlock()
		return Authorization{}, ErrSessionFinished
	}
	if session.sink == nil {
		session.mu.Unlock()
		if request.Confirmation.Required && !request.Confirmation.Provided {
			return Authorization{Allowed: false, Summary: summary}, ErrConfirmationRequired
		}
		return Authorization{Allowed: true, Audited: false, Summary: summary}, nil
	}

	if err := session.startRawLocked(ctx); err != nil {
		resultErr, warning := session.applyFailurePolicyLocked("start", request.Scope, err)
		session.mu.Unlock()
		session.notifyWarning(warning)
		if resultErr != nil {
			return Authorization{Allowed: false, Summary: summary}, resultErr
		}
		if request.Confirmation.Required && !request.Confirmation.Provided {
			return Authorization{Allowed: false, Summary: summary}, ErrConfirmationRequired
		}
		return Authorization{Allowed: true, Audited: false, Summary: summary}, nil
	}

	mutation := Mutation{Scope: request.Scope, Confirmation: request.Confirmation}
	if request.Confirmation.Required && !request.Confirmation.Provided {
		event := session.newEventLocked(EventMutationRejected)
		event.Mutation = &mutation
		event.CandidateSummary = &summary
		event.Outcome = OutcomeRejected
		event.Error = sanitizeError(ErrConfirmationRequired, session.detail, session.identifierKey)
		writeErr := session.appendLocked(ctx, event, "rejected")
		_, warning := session.applyFailurePolicyLocked("rejected", request.Scope, writeErr)
		session.mu.Unlock()
		session.notifyWarning(warning)
		return Authorization{Allowed: false, Audited: writeErr == nil, Summary: summary}, ErrConfirmationRequired
	}

	setID, err := randomAuditID(session.random)
	if err != nil {
		session.mu.Unlock()
		return Authorization{Allowed: false, Summary: summary}, err
	}
	pageCount := 0
	if len(candidates) > 0 {
		pageCount = (len(candidates) + session.pageSize - 1) / session.pageSize
	}
	for page := 0; page < pageCount; page++ {
		start := page * session.pageSize
		end := min(start+session.pageSize, len(candidates))
		event := session.newEventLocked(EventMutationCandidates)
		event.Mutation = &mutation
		event.CandidateSetID = setID
		event.CandidatePage = page + 1
		event.CandidatePageCount = pageCount
		event.CandidateFinal = page+1 == pageCount
		event.Candidates = append([]Candidate(nil), candidates[start:end]...)
		if writeErr := session.appendLocked(ctx, event, "candidates"); writeErr != nil {
			resultErr, warning := session.applyFailurePolicyLocked("candidates", request.Scope, writeErr)
			session.mu.Unlock()
			session.notifyWarning(warning)
			if resultErr != nil {
				return Authorization{Allowed: false, CandidateSetID: setID, Summary: summary}, resultErr
			}
			return Authorization{Allowed: true, Audited: false, CandidateSetID: setID, Summary: summary}, nil
		}
	}

	authorized := session.newEventLocked(EventMutationAuthorized)
	authorized.Mutation = &mutation
	authorized.CandidateSetID = setID
	authorized.CandidateSummary = &summary
	if writeErr := session.appendLocked(ctx, authorized, "authorized"); writeErr != nil {
		resultErr, warning := session.applyFailurePolicyLocked("authorized", request.Scope, writeErr)
		session.mu.Unlock()
		session.notifyWarning(warning)
		if resultErr != nil {
			return Authorization{Allowed: false, CandidateSetID: setID, Summary: summary}, resultErr
		}
		return Authorization{Allowed: true, Audited: false, CandidateSetID: setID, Summary: summary}, nil
	}
	session.authorizedMutations++
	session.mu.Unlock()
	return Authorization{Allowed: true, Audited: true, CandidateSetID: setID, Summary: summary}, nil
}

func (session *Session) Finish(ctx context.Context, result FinishResult) error {
	if session == nil || session.sink == nil {
		return nil
	}
	session.mu.Lock()
	if session.finished {
		err := session.finishErr
		session.mu.Unlock()
		return err
	}
	var startErr error
	if !session.started {
		startErr = session.startRawLocked(ctx)
	}
	event := session.newEventLocked(EventCommandFinish)
	event.Outcome = normalizeOutcome(result.Outcome, result.Err)
	duration := session.clock().Sub(session.startedAt)
	if duration > 0 {
		event.DurationMS = duration.Milliseconds()
	}
	event.Result = sanitizeResultSummary(result.Result)
	event.Error = sanitizeError(result.Err, session.detail, session.identifierKey)
	event.AuthorizedMutations = session.authorizedMutations
	finishErr := session.appendLocked(ctx, event, "finish")
	combined := errors.Join(startErr, finishErr)
	scope := MutationNone
	if session.authorizedMutations > 0 {
		scope = MutationDockerPersistent
	}
	resultErr, warning := session.applyFailurePolicyLocked("finish", scope, combined)
	session.finished = true
	session.finishErr = resultErr
	session.mu.Unlock()
	session.notifyWarning(warning)
	return resultErr
}

func (session *Session) startRawLocked(ctx context.Context) error {
	if session.started {
		return nil
	}
	event := session.newEventLocked(EventCommandStart)
	if err := session.appendLocked(ctx, event, "start"); err != nil {
		return err
	}
	session.started = true
	return nil
}

func (session *Session) newEventLocked(eventType EventType) Event {
	session.sequence++
	return Event{
		Schema:    SchemaVersion,
		Type:      eventType,
		Time:      session.clock().UTC().Format(time.RFC3339Nano),
		RunID:     session.runID,
		Sequence:  session.sequence,
		Operation: session.operation,
		Command:   session.command,
		Operator:  session.operator,
		Profile:   session.profile,
		Endpoint:  session.endpoint,
	}
}

func (session *Session) appendLocked(ctx context.Context, event Event, stage string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := session.sink.Append(ctx, event); err != nil {
		return &WriteError{Stage: stage, Err: err}
	}
	return nil
}

func (session *Session) applyFailurePolicyLocked(stage string, scope MutationScope, err error) (error, error) {
	if err == nil {
		return nil, nil
	}
	wrapped := err
	var writeErr *WriteError
	if !errors.As(err, &writeErr) {
		wrapped = &WriteError{Stage: stage, Err: err}
	}
	switch session.failurePolicy {
	case FailureRequired:
		return wrapped, nil
	case FailureDenyMutation:
		if scope.Mutates() {
			return wrapped, nil
		}
	}
	if session.warned {
		return nil, nil
	}
	session.warned = true
	return nil, wrapped
}

func (session *Session) notifyWarning(err error) {
	if err != nil && session.warning != nil {
		session.warning(err)
	}
}

func (session *Session) prepareCandidates(inputs []CandidateInput) ([]Candidate, CandidateSummary, error) {
	if len(inputs) > session.maxCandidates {
		return nil, CandidateSummary{}, fmt.Errorf("audit candidate count %d exceeds limit %d", len(inputs), session.maxCandidates)
	}
	result := make([]Candidate, len(inputs))
	summary := CandidateSummary{Total: len(inputs)}
	if len(inputs) > 0 {
		summary.ByKind = make(map[string]int)
	}
	for index, input := range inputs {
		candidate, err := sanitizeCandidate(input, session.detail, session.identifierKey)
		if err != nil {
			return nil, CandidateSummary{}, fmt.Errorf("audit candidate %d: %w", index, err)
		}
		result[index] = candidate
		summary.ByKind[candidate.Kind]++
	}
	return result, summary, nil
}

func sanitizeResultSummary(result ResultSummary) *ResultSummary {
	copy := result
	if len(result.ByKind) > 0 {
		copy.ByKind = make(map[string]int, len(result.ByKind))
		keys := make([]string, 0, len(result.ByKind))
		for key := range result.ByKind {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if auditTokenPattern.MatchString(key) && result.ByKind[key] >= 0 {
				copy.ByKind[key] = result.ByKind[key]
			}
		}
	}
	if copy.Success == 0 && copy.Failed == 0 && copy.Unknown == 0 && copy.Skipped == 0 && len(copy.ByKind) == 0 {
		return nil
	}
	return &copy
}

func normalizeOutcome(outcome Outcome, err error) Outcome {
	if outcome != "" {
		return outcome
	}
	if errors.Is(err, context.Canceled) {
		return OutcomeCanceled
	}
	if err != nil {
		return OutcomeFailed
	}
	return OutcomeSuccess
}

func validMutationScope(scope MutationScope) bool {
	switch scope {
	case MutationDockerPersistent, MutationDockerTemporary, MutationFilesystem, MutationExternalOperation:
		return true
	default:
		return false
	}
}

func validOperation(value string) bool {
	for _, part := range strings.Split(value, ".") {
		if !auditTokenPattern.MatchString(part) {
			return false
		}
	}
	return true
}
