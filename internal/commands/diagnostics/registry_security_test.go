package diagnostics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestPingRegistryV2RejectsCrossOriginCredentialRedirect(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("redirect target received Authorization %q", r.Header.Get("Authorization"))
		}
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusFound)
	}))
	defer source.Close()

	client := source.Client()
	client.CheckRedirect = registryCredentialRedirectPolicy
	restore := replaceRegistryCheckHTTPClient(client)
	defer restore()
	result := pingRegistryV2(context.Background(), strings.TrimPrefix(source.URL, "http://"), true, registryCredential{
		Found:    true,
		Username: "admin",
		Password: "redirect-secret",
	})
	if result.Status != "failed" || !strings.Contains(result.Message, "credential redirect") {
		t.Fatalf("result = %#v, want redirect failure", result)
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target requests = %d, want 0", targetRequests.Load())
	}
}

func TestRegistryCredentialRedirectPolicyRejectsHTTPSDowngrade(t *testing.T) {
	original, _ := http.NewRequest(http.MethodGet, "https://registry.example/v2/", nil)
	redirected, _ := http.NewRequest(http.MethodGet, "http://registry.example/v2/", nil)
	if err := registryCredentialRedirectPolicy(redirected, []*http.Request{original}); err == nil {
		t.Fatal("HTTPS downgrade accepted")
	}
}

func TestRunRegistryLoginCheckCanDisableCredentialHelper(t *testing.T) {
	previous := runDockerCredentialHelper
	called := false
	runDockerCredentialHelper = func(context.Context, string, string) (registryCredential, error) {
		called = true
		return registryCredential{}, nil
	}
	t.Cleanup(func() { runDockerCredentialHelper = previous })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	registryName := strings.TrimPrefix(server.URL, "http://")
	configPath := writeDockerConfig(t, fmt.Sprintf(`{"credHelpers":{%q:"pass"}}`, registryName))

	report, err := runRegistryLoginCheck(context.Background(), registryName, RegistryLoginCheckOptions{
		DockerConfig:             configPath,
		DisableCredentialHelpers: true,
		CredentialHelperTimeout:  time.Second,
		Timeout:                  time.Second,
		PlainHTTP:                true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("credential helper ran while disabled")
	}
	if report.Credential.Found || report.Credential.HelperSource != "credHelpers["+registryName+"]" || !strings.Contains(report.Credential.Message, "disabled") {
		t.Fatalf("credential report = %#v", report.Credential)
	}
}

func TestRegistryAndDoctorExposeCredentialHelperFlags(t *testing.T) {
	for _, cmd := range []*cobra.Command{NewRegistryReportCommand(), NewDoctorCommand()} {
		for _, name := range []string{"disable-credential-helpers", "credential-helper-timeout"} {
			if cmd.Flags().Lookup(name) == nil {
				t.Fatalf("%s missing --%s", cmd.Name(), name)
			}
		}
	}
}
