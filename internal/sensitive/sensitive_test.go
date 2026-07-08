package sensitive

import (
	"strings"
	"testing"
)

func TestNormalizeProfileKeepsDefaultNone(t *testing.T) {
	profile, err := NormalizeProfile("", false)
	if err != nil {
		t.Fatal(err)
	}
	if profile != ProfileNone {
		t.Fatalf("profile = %q, want none", profile)
	}
	profile, err = NormalizeProfile("", true)
	if err != nil {
		t.Fatal(err)
	}
	if profile != ProfileBasic {
		t.Fatalf("profile = %q, want basic", profile)
	}
}

func TestRedactTextBasic(t *testing.T) {
	text := `Authorization: Bearer abc
url=https://user:pass@example.com/path?token=abc&mode=prod
password=secret`
	got := RedactText(text, ProfileBasic)
	for _, leaked := range []string{"Bearer abc", "pass@example.com", "token=abc", "password=secret"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redacted text leaked %q:\n%s", leaked, got)
		}
	}
	if strings.Count(got, RedactedValue) < 3 {
		t.Fatalf("redacted text = %q, want multiple redactions", got)
	}
}

func TestRedactTextStrict(t *testing.T) {
	text := `Cookie: sid=abc
session_id=xyz
token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature
-----BEGIN PRIVATE KEY-----
abc
-----END PRIVATE KEY-----`
	got := RedactText(text, ProfileStrict)
	for _, leaked := range []string{"sid=abc", "session_id=xyz", "eyJhbGci", "BEGIN PRIVATE KEY"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("strict redacted text leaked %q:\n%s", leaked, got)
		}
	}
}

func TestStrictSensitiveKeyAddsTokenizedKeyNames(t *testing.T) {
	if IsSensitiveKey("public_key", ProfileBasic) {
		t.Fatal("basic should not redact public_key")
	}
	if !IsSensitiveKey("public_key", ProfileStrict) {
		t.Fatal("strict should redact public_key")
	}
}
