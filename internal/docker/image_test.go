package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
)

func TestReadOnlyReaderDoesNotExposeCloser(t *testing.T) {
	reader := readOnlyReader{Reader: strings.NewReader("image")}

	if _, ok := any(reader).(io.Closer); ok {
		t.Fatal("readOnlyReader implements io.Closer, want read-only wrapper")
	}
}

func TestCopyDockerPushStreamReturnsDockerError(t *testing.T) {
	input := strings.Join([]string{
		`{"status":"Preparing","id":"layer"}`,
		`{"errorDetail":{"message":"unauthorized: action: push"},"error":"unauthorized: action: push"}`,
	}, "\n")
	var output bytes.Buffer

	err := copyDockerPushStream(context.Background(), &output, strings.NewReader(input))
	if err == nil {
		t.Fatal("copyDockerPushStream() error = nil, want docker push error")
	}
	if !strings.Contains(err.Error(), "unauthorized: action: push") {
		t.Fatalf("copyDockerPushStream() error = %v, want unauthorized message", err)
	}
	if !strings.Contains(output.String(), `"status":"Preparing"`) || !strings.Contains(output.String(), `"errorDetail"`) {
		t.Fatalf("output = %q, want copied docker stream", output.String())
	}
}

func TestCopyDockerPushStreamIgnoresNonJSONProgress(t *testing.T) {
	input := "plain progress\n{\"status\":\"Pushed\"}\n"
	var output bytes.Buffer

	if err := copyDockerPushStream(context.Background(), &output, strings.NewReader(input)); err != nil {
		t.Fatalf("copyDockerPushStream() error = %v, want nil", err)
	}
	if got := output.String(); got != input {
		t.Fatalf("output = %q, want %q", got, input)
	}
}

func TestPushWithAuthOutputSendsRegistryAuthAndCopiesResponse(t *testing.T) {
	const response = "{\"status\":\"Pushed\"}\n"
	encodeAuth := func(config registry.AuthConfig) string {
		t.Helper()
		payload, err := json.Marshal(config)
		if err != nil {
			t.Fatalf("marshal registry auth: %v", err)
		}
		return base64.URLEncoding.EncodeToString(payload)
	}
	credentials := registry.AuthConfig{
		Username:      "push-user",
		Password:      "push-password",
		ServerAddress: "example.com",
	}
	identityToken := registry.AuthConfig{
		IdentityToken: "identity-token",
		ServerAddress: "example.com",
	}
	tests := []struct {
		name         string
		registryAuth string
		wantConfig   registry.AuthConfig
	}{
		{
			name: "anonymous uses empty JSON auth",
		},
		{
			name:         "username and password auth is preserved",
			registryAuth: encodeAuth(credentials),
			wantConfig:   credentials,
		},
		{
			name:         "identity token auth is preserved",
			registryAuth: encodeAuth(identityToken),
			wantConfig:   identityToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type requestDetails struct {
				method        string
				path          string
				tag           string
				registryAuth  string
				decodedConfig registry.AuthConfig
				decodeErr     error
				jsonErr       error
			}
			requests := make(chan requestDetails, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				encodedAuth := r.Header.Get(registry.AuthHeader)
				decodedAuth, decodeErr := base64.URLEncoding.DecodeString(encodedAuth)
				var decodedConfig registry.AuthConfig
				var jsonErr error
				if decodeErr == nil {
					jsonErr = json.Unmarshal(decodedAuth, &decodedConfig)
				}
				requests <- requestDetails{
					method:        r.Method,
					path:          r.URL.Path,
					tag:           r.URL.Query().Get("tag"),
					registryAuth:  encodedAuth,
					decodedConfig: decodedConfig,
					decodeErr:     decodeErr,
					jsonErr:       jsonErr,
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()

			cli, err := client.New(
				client.WithHost(server.URL),
				client.WithHTTPClient(server.Client()),
				client.WithScheme("http"),
				client.WithAPIVersion("1.55"),
			)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer func() { _ = cli.Close() }()

			manager := &ImageManager{cli: cli}
			var output bytes.Buffer
			if err := manager.PushWithAuthOutput(context.Background(), "example.com/team/app:release", tt.registryAuth, &output); err != nil {
				t.Fatalf("PushWithAuthOutput() error = %v, want nil", err)
			}

			request := <-requests
			if request.method != http.MethodPost {
				t.Errorf("request method = %q, want %q", request.method, http.MethodPost)
			}
			if !strings.HasSuffix(request.path, "/images/example.com/team/app/push") {
				t.Errorf("request path = %q, want image push path", request.path)
			}
			if request.tag != "release" {
				t.Errorf("request tag = %q, want %q", request.tag, "release")
			}
			wantAuth := tt.registryAuth
			if wantAuth == "" {
				wantAuth = "e30="
			}
			if request.registryAuth != wantAuth {
				t.Errorf("X-Registry-Auth = %q, want %q", request.registryAuth, wantAuth)
			}
			if tt.registryAuth != "" && request.registryAuth != tt.registryAuth {
				t.Errorf("non-empty registry auth changed in transit: got %q, want %q", request.registryAuth, tt.registryAuth)
			}
			if request.decodeErr != nil {
				t.Fatalf("decode X-Registry-Auth as base64url: %v", request.decodeErr)
			}
			if request.jsonErr != nil {
				t.Fatalf("decode X-Registry-Auth JSON: %v", request.jsonErr)
			}
			if request.decodedConfig != tt.wantConfig {
				t.Errorf("decoded X-Registry-Auth = %#v, want %#v", request.decodedConfig, tt.wantConfig)
			}
			if got := output.String(); got != response {
				t.Errorf("output = %q, want %q", got, response)
			}
		})
	}
}
