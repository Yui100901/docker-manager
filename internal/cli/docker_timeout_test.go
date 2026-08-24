package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootCommandRejectsNonPositiveDockerTimeout(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			cfg := appConfig{}
			opts := outputOptions{}
			cmd := newRootCommand(&cfg, &opts)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--docker-timeout", value, "version"})

			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "--docker-timeout must be greater than zero") {
				t.Fatalf("Execute() error = %v, want non-positive timeout rejection", err)
			}
		})
	}
}
