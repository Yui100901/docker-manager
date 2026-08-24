package docker

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestImageManagerListAPI(t *testing.T) {
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireDockerAPIRequest(t, r, http.MethodGet, "/images/json", url.Values{"all": {"1"}})
		writeDockerAPITestJSON(t, w, http.StatusOK, []map[string]any{{
			"Id": "sha256:image-id", "RepoTags": []string{"repo/app:latest"},
		}})
	}))

	images, err := (&ImageManager{cli: cli}).List(true)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(images) != 1 || images[0].ID != "sha256:image-id" || !reflect.DeepEqual(images[0].RepoTags, []string{"repo/app:latest"}) {
		t.Fatalf("List() = %#v, want decoded image", images)
	}
}

func TestImageManagerSaveAPI(t *testing.T) {
	const payload = "docker-image-archive"
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireDockerAPIRequest(t, r, http.MethodGet, "/images/get", url.Values{
			"names": {"repo/app:one", "repo/app:two"},
		})
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = io.WriteString(w, payload)
	}))
	outputPath := filepath.Join(t.TempDir(), "images.tar")

	if err := (&ImageManager{cli: cli}).Save([]string{"repo/app:one", "repo/app:two"}, outputPath); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read saved image: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("saved image = %q, want %q", got, payload)
	}
}

func TestImageManagerLoadAPI(t *testing.T) {
	const input = "docker-image-input"
	const response = "load complete\n"
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireDockerAPIRequest(t, r, http.MethodPost, "/images/load", url.Values{"quiet": {"0"}})
		if got := r.Header.Get("Content-Type"); got != "application/x-tar" {
			t.Errorf("Content-Type = %q, want application/x-tar", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read image load body: %v", err)
		}
		if string(body) != input {
			t.Errorf("image load body = %q, want %q", body, input)
		}
		_, _ = io.WriteString(w, response)
	}))
	inputPath := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(inputPath, []byte(input), 0600); err != nil {
		t.Fatalf("write image input: %v", err)
	}

	var output bytes.Buffer
	if err := (&ImageManager{cli: cli}).LoadWithContext(context.Background(), inputPath, &output); err != nil {
		t.Fatalf("LoadWithContext() error = %v", err)
	}
	if output.String() != response {
		t.Fatalf("load output = %q, want %q", output.String(), response)
	}
}

func TestImageManagerTagAPI(t *testing.T) {
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireDockerAPIRequest(t, r, http.MethodPost, "/images/repo/app:source/tag", url.Values{
			"repo": {"registry.example/team/app"}, "tag": {"release"},
		})
		w.WriteHeader(http.StatusCreated)
	}))

	if err := (&ImageManager{cli: cli}).Tag(context.Background(), "repo/app:source", "registry.example/team/app:release"); err != nil {
		t.Fatalf("Tag() error = %v", err)
	}
}

func TestImageManagerPropagatesAPIErrors(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		query  url.Values
		call   func(*testing.T, *ImageManager) error
	}{
		{
			name: "list", method: http.MethodGet, path: "/images/json", query: url.Values{},
			call: func(_ *testing.T, manager *ImageManager) error {
				_, err := manager.ListWithContext(context.Background(), false)
				return err
			},
		},
		{
			name: "save", method: http.MethodGet, path: "/images/get", query: url.Values{"names": {"repo/app:latest"}},
			call: func(t *testing.T, manager *ImageManager) error {
				return manager.SaveWithContext(context.Background(), []string{"repo/app:latest"}, filepath.Join(t.TempDir(), "image.tar"))
			},
		},
		{
			name: "load", method: http.MethodPost, path: "/images/load", query: url.Values{"quiet": {"0"}},
			call: func(t *testing.T, manager *ImageManager) error {
				path := filepath.Join(t.TempDir(), "image.tar")
				if err := os.WriteFile(path, []byte("input"), 0600); err != nil {
					t.Fatalf("write load input: %v", err)
				}
				return manager.LoadWithContext(context.Background(), path, io.Discard)
			},
		},
		{
			name: "tag", method: http.MethodPost, path: "/images/repo/app:source/tag",
			query: url.Values{"repo": {"registry.example/team/app"}, "tag": {"release"}},
			call: func(_ *testing.T, manager *ImageManager) error {
				return manager.Tag(context.Background(), "repo/app:source", "registry.example/team/app:release")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireDockerAPIRequest(t, r, tt.method, tt.path, tt.query)
				writeDockerAPITestJSON(t, w, http.StatusInternalServerError, map[string]string{"message": "forced image API error"})
			}))
			err := tt.call(t, &ImageManager{cli: cli})
			if err == nil || !strings.Contains(err.Error(), "forced image API error") {
				t.Fatalf("%s error = %v, want daemon response", tt.name, err)
			}
		})
	}
}
