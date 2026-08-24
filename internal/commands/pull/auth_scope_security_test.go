package pull

import (
	"strings"
	"testing"
)

func TestBearerTokenQueryPinsScopeToRequestedImagePull(t *testing.T) {
	info := &ImageInfo{Repository: "team", Image: "app"}
	wantScope := "repository:team/app:pull"
	tests := []struct {
		name  string
		scope string
	}{
		{name: "missing scope"},
		{name: "matching pull scope", scope: wantScope},
		{name: "push action", scope: "repository:team/app:push"},
		{name: "pull and push actions", scope: "repository:team/app:pull,push"},
		{name: "different repository", scope: "repository:other/private:pull"},
		{name: "registry catalog", scope: "registry:catalog:*"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			challenge := authChallenge{Params: map[string]string{"scope": tt.scope}}
			query, err := bearerTokenQuery(challenge, info)
			if err != nil {
				t.Fatal(err)
			}
			if got := query["scope"]; got != wantScope {
				t.Fatalf("scope = %q, want %q", got, wantScope)
			}
		})
	}
}

func TestBearerTokenQueryAllowsHarborService(t *testing.T) {
	const service = "harbor-registry"
	query, err := bearerTokenQuery(
		authChallenge{Params: map[string]string{"service": service}},
		&ImageInfo{Repository: "library", Image: "busybox"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := query["service"]; got != service {
		t.Fatalf("service = %q, want %q", got, service)
	}
}

func TestBearerTokenQueryRejectsInvalidService(t *testing.T) {
	tests := []struct {
		name    string
		service string
	}{
		{name: "too long", service: strings.Repeat("a", maxBearerServiceBytes+1)},
		{name: "carriage return", service: "harbor-registry\rforged"},
		{name: "line feed", service: "harbor-registry\nforged"},
		{name: "nul", service: "harbor-registry\x00forged"},
		{name: "invalid utf8", service: string([]byte{0xff, 0xfe})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := bearerTokenQuery(
				authChallenge{Params: map[string]string{"service": tt.service}},
				&ImageInfo{Repository: "team", Image: "app"},
			)
			if err == nil {
				t.Fatalf("service %q was accepted", tt.service)
			}
		})
	}
}
