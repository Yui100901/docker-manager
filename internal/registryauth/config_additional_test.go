package registryauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const credentialHelperTestModeEnv = "DM_TEST_CREDENTIAL_HELPER_MODE"

func TestMain(m *testing.M) {
	mode := os.Getenv(credentialHelperTestModeEnv)
	if mode == "" {
		os.Exit(m.Run())
	}

	switch mode {
	case "success":
		server, _ := io.ReadAll(os.Stdin)
		_, _ = fmt.Fprintf(os.Stdout, `{"ServerURL":%q,"Username":"helper-user","Secret":"helper-secret"}`, strings.TrimSpace(string(server)))
	case "token":
		_, _ = io.WriteString(os.Stdout, `{"ServerURL":"registry.example","Username":"<token>","Secret":"identity-token"}`)
	case "invalid-json":
		_, _ = io.WriteString(os.Stdout, `{`)
	case "large-output":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", maxCredentialHelperStdout+1024))
	case "failure":
		_, _ = io.WriteString(os.Stderr, "helper-secret-error\n")
		os.Exit(7)
	case "sleep":
		time.Sleep(10 * time.Second)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown helper test mode %q", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestDefaultConfigPathUsesDockerConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docker-config")
	t.Setenv("DOCKER_CONFIG", "  "+dir+"  ")
	if got := DefaultConfigPath(); got != filepath.Join(dir, "config.json") {
		t.Fatalf("DefaultConfigPath() = %q, want Docker config path", got)
	}
}

func TestReadConfigCoversMissingValidInvalidAndReadError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	if cfg, exists, err := ReadConfig(missing); err != nil || exists || len(cfg.Auths) != 0 {
		t.Fatalf("ReadConfig(missing) = %#v, %t, %v", cfg, exists, err)
	}

	valid := filepath.Join(dir, "config.json")
	data := `{"auths":{"registry.example":{"username":"admin","password":"secret"}},"credsStore":"desktop","credHelpers":{"private.example":"pass"}}`
	if err := os.WriteFile(valid, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, exists, err := ReadConfig(valid)
	if err != nil || !exists {
		t.Fatalf("ReadConfig(valid) = %#v, %t, %v", cfg, exists, err)
	}
	if cfg.Auths["registry.example"].Username != "admin" || cfg.CredsStore != "desktop" || cfg.CredHelpers["private.example"] != "pass" {
		t.Fatalf("ReadConfig(valid) = %#v", cfg)
	}

	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"auths":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := ReadConfig(invalid); err == nil || !exists {
		t.Fatalf("ReadConfig(invalid) exists=%t error=%v, want parse error", exists, err)
	}
	if _, exists, err := ReadConfig(dir); err == nil || exists {
		t.Fatalf("ReadConfig(directory) exists=%t error=%v, want read error", exists, err)
	}
}

func TestCredentialFromAuthEntryDecodingAndPrecedence(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("user:password:with:colons"))
	got := CredentialFromAuthEntry(AuthEntry{Auth: encoded})
	if got.Username != "user" || got.Password != "password:with:colons" {
		t.Fatalf("CredentialFromAuthEntry(encoded) = %#v", got)
	}

	got = CredentialFromAuthEntry(AuthEntry{
		Auth:          encoded,
		Username:      "explicit-user",
		Password:      "explicit-password",
		IdentityToken: "token",
	})
	if got.Username != "explicit-user" || got.Password != "explicit-password" || got.IdentityToken != "token" {
		t.Fatalf("CredentialFromAuthEntry(explicit) = %#v", got)
	}

	for _, auth := range []string{"not-base64", base64.StdEncoding.EncodeToString([]byte("missing-colon"))} {
		if got := CredentialFromAuthEntry(AuthEntry{Auth: auth}); got.Username != "" || got.Password != "" {
			t.Fatalf("CredentialFromAuthEntry(%q) = %#v, want empty", auth, got)
		}
	}
}

func TestResolveCredentialWrapperAndFallbackMessages(t *testing.T) {
	cfg := Config{Auths: map[string]AuthEntry{
		"https://registry.example": {Username: "admin", Password: "secret"},
		"empty.example":            {},
	}}
	cred := ResolveCredential(context.Background(), cfg, "registry.example", nil)
	if !cred.Found || cred.Source != "auths" || cred.ServerAddress != "https://registry.example" || cred.Username != "admin" {
		t.Fatalf("ResolveCredential() = %#v", cred)
	}
	cred = ResolveCredential(context.Background(), cfg, "empty.example", nil)
	if cred.Found || cred.Source != "auths" || !strings.Contains(cred.Message, "no usable credential") {
		t.Fatalf("ResolveCredential(empty auth) = %#v", cred)
	}
	cred = ResolveCredential(context.Background(), cfg, "missing.example", nil)
	if cred.Found || !strings.Contains(cred.Message, "no matching") {
		t.Fatalf("ResolveCredential(missing) = %#v", cred)
	}
}

func TestCredentialHelperLookupPrecedenceAndWrappers(t *testing.T) {
	cfg := Config{
		CredHelpers: map[string]string{
			"registry.example":         "  pass  ",
			"https://registry.example": "secondary",
		},
		CredsStore: "desktop",
	}
	keys := ConfigKeys("registry.example")
	helper, server := FindCredentialHelper(cfg, keys)
	if helper != "pass" || server != "registry.example" {
		t.Fatalf("FindCredentialHelper() = %q, %q", helper, server)
	}

	delete(cfg.CredHelpers, "registry.example")
	helper, server, source := FindCredentialHelperSource(cfg, keys)
	if helper != "secondary" || server != "https://registry.example" || source != "credHelpers[https://registry.example]" {
		t.Fatalf("FindCredentialHelperSource() = %q, %q, %q", helper, server, source)
	}

	helper, server, source = FindCredentialHelperSource(Config{CredsStore: " desktop "}, []string{"registry.example"})
	if helper != "desktop" || server != "registry.example" || source != "credsStore" {
		t.Fatalf("credsStore lookup = %q, %q, %q", helper, server, source)
	}
	if helper, server, source = FindCredentialHelperSource(Config{CredsStore: "desktop"}, nil); helper != "" || server != "" || source != "" {
		t.Fatalf("empty-key helper lookup = %q, %q, %q", helper, server, source)
	}
}

func TestDefaultRunCredentialHelperProtocolAndErrors(t *testing.T) {
	helperPath := installCredentialHelperTestBinary(t, "dmtest")

	t.Run("username and password", func(t *testing.T) {
		t.Setenv(credentialHelperTestModeEnv, "success")
		cred, err := DefaultRunCredentialHelper(context.Background(), "dmtest", "registry.example")
		if err != nil {
			t.Fatalf("DefaultRunCredentialHelper() error = %v", err)
		}
		if cred.Username != "helper-user" || cred.Password != "helper-secret" || cred.ServerAddress != "registry.example" {
			t.Fatalf("credential = %#v", cred)
		}
		if !sameCredentialHelperPath(cred.HelperPath, helperPath) {
			t.Fatalf("HelperPath = %q, want %q", cred.HelperPath, helperPath)
		}
	})

	t.Run("identity token", func(t *testing.T) {
		t.Setenv(credentialHelperTestModeEnv, "token")
		cred, err := DefaultRunCredentialHelper(context.Background(), "dmtest", "registry.example")
		if err != nil {
			t.Fatalf("DefaultRunCredentialHelper() error = %v", err)
		}
		if cred.Username != "" || cred.Password != "" || cred.IdentityToken != "identity-token" {
			t.Fatalf("token credential = %#v", cred)
		}
	})

	for _, tt := range []struct {
		name string
		mode string
		want string
	}{
		{name: "invalid JSON", mode: "invalid-json", want: "unexpected end"},
		{name: "large output", mode: "large-output", want: "output exceeds"},
		{name: "helper failure", mode: "failure", want: "helper-secret-error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(credentialHelperTestModeEnv, tt.mode)
			_, err := DefaultRunCredentialHelper(context.Background(), "dmtest", "registry.example")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DefaultRunCredentialHelper() error = %v, want %q", err, tt.want)
			}
		})
	}

	t.Run("context timeout", func(t *testing.T) {
		t.Setenv(credentialHelperTestModeEnv, "sleep")
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := DefaultRunCredentialHelper(ctx, "dmtest", "registry.example")
		if err == nil {
			t.Fatal("DefaultRunCredentialHelper(timeout) error = nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("DefaultRunCredentialHelper(timeout) error = %v, want context deadline", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("DefaultRunCredentialHelper(timeout) took %s", elapsed)
		}
	})

	if got := credentialHelperPath("dmtest"); !sameCredentialHelperPath(got, helperPath) {
		t.Fatalf("credentialHelperPath() = %q, want %q", got, helperPath)
	}
}

func TestDefaultRunCredentialHelperReportsMissingExecutable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := DefaultRunCredentialHelper(context.Background(), "definitely-missing", "registry.example")
	if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("DefaultRunCredentialHelper(missing) error = %v", err)
	}
	if got := credentialHelperPath("definitely-missing"); got != "" {
		t.Fatalf("credentialHelperPath(missing) = %q, want empty", got)
	}
}

func TestLimitedBufferExhaustedAndAccessors(t *testing.T) {
	buffer := &limitedBuffer{remaining: 0}
	payload := []byte("discarded")
	written, err := buffer.Write(payload)
	if err != nil || written != len(payload) || !buffer.truncated {
		t.Fatalf("Write() = %d, %v, truncated=%t", written, err, buffer.truncated)
	}
	if buffer.String() != "" || len(buffer.Bytes()) != 0 {
		t.Fatalf("limitedBuffer retained data: %q / %v", buffer.String(), buffer.Bytes())
	}
}

func TestBasicAuthHeaderAndUniqueStrings(t *testing.T) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:p:a:ss"))
	if got := BasicAuthHeader("user", "p:a:ss"); got != want {
		t.Fatalf("BasicAuthHeader() = %q, want %q", got, want)
	}
	values := UniqueStrings([]string{"registry.example", "", "registry.example", "https://registry.example"})
	if strings.Join(values, ",") != "registry.example,https://registry.example" {
		t.Fatalf("UniqueStrings() = %#v", values)
	}
}

func installCredentialHelperTestBinary(t *testing.T, helper string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "docker-credential-" + helper
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if runtime.GOOS != "windows" {
		if err := os.Link(executable, path); err == nil {
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			return path
		}
	}
	{
		data, readErr := os.ReadFile(executable)
		if readErr != nil {
			t.Fatalf("read test binary: %v", readErr)
		}
		if writeErr := os.WriteFile(path, data, 0o700); writeErr != nil {
			t.Fatalf("copy test credential helper: %v", writeErr)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func sameCredentialHelperPath(got, want string) bool {
	got = filepath.Clean(got)
	want = filepath.Clean(want)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(got, want)
	}
	return got == want
}
