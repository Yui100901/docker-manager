package docker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/moby/moby/client"
)

const dockerAPITestVersion = "1.55"

func newDockerAPITestClient(t *testing.T, handler http.Handler) *client.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cli, err := client.New(
		client.WithHost(server.URL),
		client.WithHTTPClient(server.Client()),
		client.WithScheme("http"),
		client.WithAPIVersion(dockerAPITestVersion),
	)
	if err != nil {
		t.Fatalf("create Moby test client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func requireDockerAPIRequest(t *testing.T, r *http.Request, method, path string, query url.Values) {
	t.Helper()
	if r.Method != method {
		t.Errorf("request method = %q, want %q", r.Method, method)
	}
	wantPath := "/v" + dockerAPITestVersion + path
	if r.URL.Path != wantPath {
		t.Errorf("request path = %q, want %q", r.URL.Path, wantPath)
	}
	if got := r.URL.Query(); !reflect.DeepEqual(got, query) {
		t.Errorf("request query = %#v, want %#v", got, query)
	}
}

func writeDockerAPITestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("write Docker API response: %v", err)
	}
}
