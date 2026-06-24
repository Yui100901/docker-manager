package docker

import (
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
