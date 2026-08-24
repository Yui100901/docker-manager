package registryauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/sensitive"
)

func TestResolveCredentialWithOptionsDisablesHelperAndFallsBackToAuths(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("fallback:secret"))
	cfg := Config{
		Auths: map[string]AuthEntry{
			"registry.example": {Auth: auth},
		},
		CredHelpers: map[string]string{"registry.example": "pass"},
	}
	called := false
	cred := ResolveCredentialWithOptions(context.Background(), cfg, "registry.example", ResolveOptions{
		DisableHelpers: true,
		RunHelper: func(context.Context, string, string) (Credential, error) {
			called = true
			return Credential{}, nil
		},
	})
	if called {
		t.Fatal("credential helper ran while disabled")
	}
	if !cred.Found || cred.Source != "auths" || cred.Username != "fallback" || cred.Password != "secret" {
		t.Fatalf("credential = %#v, want auths fallback", cred)
	}
}

func TestResolveCredentialWithOptionsReportsDisabledHelperSource(t *testing.T) {
	cfg := Config{CredHelpers: map[string]string{"registry.example": "pass"}}
	cred := ResolveCredentialWithOptions(context.Background(), cfg, "registry.example", ResolveOptions{DisableHelpers: true})
	if cred.Found || cred.Helper != "pass" || cred.HelperSource != "credHelpers[registry.example]" {
		t.Fatalf("credential = %#v", cred)
	}
	if !strings.Contains(cred.Message, "disabled") {
		t.Fatalf("message = %q, want disabled explanation", cred.Message)
	}
}

func TestResolveCredentialWithOptionsAppliesIndependentHelperTimeout(t *testing.T) {
	cfg := Config{CredsStore: "pass"}
	started := time.Now()
	cred := ResolveCredentialWithOptions(context.Background(), cfg, "registry.example", ResolveOptions{
		HelperTimeout: 20 * time.Millisecond,
		RunHelper: func(ctx context.Context, helper, server string) (Credential, error) {
			<-ctx.Done()
			return Credential{}, ctx.Err()
		},
	})
	if !strings.Contains(cred.Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("credential = %#v, want deadline error", cred)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("helper timeout took %s", elapsed)
	}
	if cred.HelperSource != "credsStore" || cred.Source != "credential-helper" {
		t.Fatalf("credential source = %#v", cred)
	}
}

func TestResolveCredentialWithOptionsPreservesHelperMetadata(t *testing.T) {
	cfg := Config{CredHelpers: map[string]string{"registry.example": "secretservice"}}
	cred := ResolveCredentialWithOptions(context.Background(), cfg, "registry.example", ResolveOptions{
		RunHelper: func(ctx context.Context, helper, server string) (Credential, error) {
			if helper != "secretservice" || server != "registry.example" {
				t.Fatalf("helper=%q server=%q", helper, server)
			}
			return Credential{Username: "admin", Password: "secret"}, nil
		},
	})
	if !cred.Found || cred.Source != "credential-helper" || cred.Helper != "secretservice" || cred.HelperSource != "credHelpers[registry.example]" {
		t.Fatalf("credential = %#v", cred)
	}
}

func TestResolveCredentialWithOptionsRejectsEmptyHelperResult(t *testing.T) {
	cfg := Config{CredsStore: "pass"}
	cred := ResolveCredentialWithOptions(context.Background(), cfg, "registry.example", ResolveOptions{
		RunHelper: func(context.Context, string, string) (Credential, error) {
			return Credential{}, nil
		},
	})
	if cred.Found || cred.Source != "credential-helper" || !strings.Contains(cred.Message, "no usable credential") {
		t.Fatalf("credential = %#v, want unusable helper result", cred)
	}
}

func TestLimitedBufferDoesNotBlockLargeHelperStderr(t *testing.T) {
	buffer := &limitedBuffer{remaining: 8}
	payload := []byte(strings.Repeat("secret", 100))
	written, err := buffer.Write(payload)
	if err != nil || written != len(payload) {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if len(buffer.String()) != 8 {
		t.Fatalf("captured stderr bytes = %d, want 8", len(buffer.String()))
	}
	if !buffer.truncated {
		t.Fatal("limited buffer did not record truncation")
	}
}

func TestCredsStoreUsesCanonicalDockerHubServerKey(t *testing.T) {
	helper, server, source := FindCredentialHelperSource(Config{CredsStore: "desktop"}, ConfigKeys("registry-1.docker.io"))
	if helper != "desktop" || server != "https://index.docker.io/v1/" || source != "credsStore" {
		t.Fatalf("helper=%q server=%q source=%q", helper, server, source)
	}
}

func TestResolveCredentialWithOptionsReturnsHelperErrorWithoutWritingStderr(t *testing.T) {
	previous := sensitive.DefaultProfile()
	sensitive.SetDefaultProfile(sensitive.ProfileNone)
	t.Cleanup(func() { sensitive.SetDefaultProfile(previous) })

	want := errors.New("token=helper-secret")
	cred := ResolveCredentialWithOptions(context.Background(), Config{CredsStore: "pass"}, "registry.example", ResolveOptions{
		RunHelper: func(context.Context, string, string) (Credential, error) { return Credential{}, want },
	})
	if cred.Message != want.Error() {
		t.Fatalf("message = %q, want captured helper error", cred.Message)
	}
}

func TestResolveCredentialWithOptionsRedactsOpaqueHelperErrors(t *testing.T) {
	previous := sensitive.DefaultProfile()
	t.Cleanup(func() { sensitive.SetDefaultProfile(previous) })

	const opaqueSecret = "raw-helper-value-7f4d1c"
	for _, profile := range []sensitive.Profile{sensitive.ProfileBasic, sensitive.ProfileStrict} {
		t.Run(string(profile), func(t *testing.T) {
			sensitive.SetDefaultProfile(profile)
			cred := ResolveCredentialWithOptions(context.Background(), Config{CredsStore: "pass"}, "registry.example", ResolveOptions{
				RunHelper: func(context.Context, string, string) (Credential, error) {
					return Credential{}, errors.New("fatal helper response: " + opaqueSecret)
				},
			})
			if strings.Contains(cred.Message, opaqueSecret) {
				t.Fatalf("credential helper message leaked opaque stderr under %s: %q", profile, cred.Message)
			}
			if !strings.Contains(cred.Message, sensitive.RedactedValue) {
				t.Fatalf("credential helper message = %q, want redaction marker", cred.Message)
			}
		})
	}
}

func TestCredentialHelperFailureMessageKeepsCanonicalCancellationStatus(t *testing.T) {
	previous := sensitive.DefaultProfile()
	sensitive.SetDefaultProfile(sensitive.ProfileBasic)
	t.Cleanup(func() { sensitive.SetDefaultProfile(previous) })

	const opaqueSecret = "wrapped-helper-detail"
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "timeout", err: fmt.Errorf("%s: %w", opaqueSecret, context.DeadlineExceeded), want: "credential helper timed out"},
		{name: "canceled", err: fmt.Errorf("%s: %w", opaqueSecret, context.Canceled), want: "credential helper canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := credentialHelperFailureMessage(test.err); got != test.want {
				t.Fatalf("credentialHelperFailureMessage() = %q, want %q", got, test.want)
			}
		})
	}
}
