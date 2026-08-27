package audit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"regexp"
	"strings"
	"unicode/utf8"

	"docker-manager/internal/sensitive"
)

const (
	identifierKeyBytes = 32
	maxOperationBytes  = 128
	maxCommandBytes    = 256
	maxProfileBytes    = 128
	maxActorBytes      = 256
	maxCandidateText   = 512
	maxErrorText       = 2048
)

var auditTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// The shared redactor intentionally preserves surrounding text. Audit error
// summaries must additionally consume the complete authorization value so a
// token separated by whitespace cannot remain after "authorization=<redacted>".
var auditAuthorizationValuePattern = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\s*[:=]\s*)(?:basic|bearer)?\s*[^\s,;]+(?:\s+[^\s,;]+)?`)

func CurrentOperator(assertedActor string) Operator {
	result := Operator{AssertedActor: sanitizeAuditText(assertedActor, maxActorBytes)}
	if current, err := user.Current(); err == nil {
		result.OSUser = sanitizeAuditText(current.Username, maxActorBytes)
		result.UIDOrSID = sanitizeAuditText(current.Uid, maxActorBytes)
	}
	if hostname, err := os.Hostname(); err == nil {
		result.Hostname = sanitizeAuditText(hostname, maxActorBytes)
	}
	return result
}

func newIdentifierKey(reader io.Reader) ([]byte, error) {
	if reader == nil {
		reader = rand.Reader
	}
	key := make([]byte, identifierKeyBytes)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("generate audit identifier key: %w", err)
	}
	return key, nil
}

func randomAuditID(reader io.Reader) (string, error) {
	if reader == nil {
		reader = rand.Reader
	}
	random := make([]byte, 16)
	if _, err := io.ReadFull(reader, random); err != nil {
		return "", fmt.Errorf("generate audit ID: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func safeIdentifier(key []byte, domain, value string) string {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(value))
	return "hmac-sha256:" + hex.EncodeToString(hash.Sum(nil)[:16])
}

func safeEndpoint(raw string, key []byte) Endpoint {
	raw = strings.TrimSpace(raw)
	scheme := "unknown"
	if parsed, err := url.Parse(raw); err == nil && validEndpointScheme(parsed.Scheme) {
		scheme = strings.ToLower(parsed.Scheme)
	} else if before, _, found := strings.Cut(raw, ":"); found && validEndpointScheme(before) {
		scheme = strings.ToLower(before)
	}
	return Endpoint{Scheme: scheme, ID: safeIdentifier(key, "endpoint", raw)}
}

func validEndpointScheme(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		case i > 0 && (r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

func sanitizeCandidate(input CandidateInput, detail Detail, key []byte) (Candidate, error) {
	kind := strings.TrimSpace(input.Kind)
	action := strings.TrimSpace(input.Action)
	if !auditTokenPattern.MatchString(kind) {
		return Candidate{}, fmt.Errorf("audit candidate kind %q must match %s", kind, auditTokenPattern.String())
	}
	if !auditTokenPattern.MatchString(action) {
		return Candidate{}, fmt.Errorf("audit candidate action %q must match %s", action, auditTokenPattern.String())
	}
	if strings.TrimSpace(input.Identifier) == "" {
		return Candidate{}, errors.New("audit candidate identifier cannot be empty")
	}
	result := Candidate{
		Kind:   kind,
		Action: action,
		ID:     safeIdentifier(key, "candidate\x00"+kind+"\x00"+action, input.Identifier),
	}
	if detail == DetailFull {
		display := input.Display
		if display == "" {
			display = input.Identifier
		}
		result.Display = sanitizeAuditText(display, maxCandidateText)
	}
	return result, nil
}

func sanitizeError(err error, detail Detail, key []byte) *ErrorInfo {
	if err == nil {
		return nil
	}
	result := &ErrorInfo{
		Class: classifyError(err),
		ID:    safeIdentifier(key, "error", err.Error()),
	}
	if detail == DetailFull {
		result.Message = sanitizeAuditText(err.Error(), maxErrorText)
	}
	return result
}

func classifyError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	case errors.Is(err, os.ErrExist):
		return "already_exists"
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		if networkErr.Timeout() {
			return "timeout"
		}
		return "network"
	}
	return "error"
}

func sanitizeAuditText(value string, maxBytes int) string {
	value = sensitive.RedactText(value, sensitive.ProfileStrict)
	value = auditAuthorizationValuePattern.ReplaceAllString(value, `${1}`+sensitive.RedactedValue)
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.TrimSpace(value)
	return truncateUTF8(value, maxBytes)
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
