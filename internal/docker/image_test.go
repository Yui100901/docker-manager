package docker

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
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
