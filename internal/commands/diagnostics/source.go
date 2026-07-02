package diagnostics

import (
	"fmt"
	"io"
)

func printDockerEndpoint(w io.Writer, endpoint string) {
	if endpoint == "" {
		return
	}
	fmt.Fprintf(w, "来源 Docker: %s\n", endpoint)
}
