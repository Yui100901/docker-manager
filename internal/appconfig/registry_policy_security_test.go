package appconfig

import (
	"strings"
	"testing"
)

func TestRegistryPolicyRejectsUnsupportedProxyScheme(t *testing.T) {
	err := (RegistryPolicy{Proxy: "ftp://proxy.example"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("Validate() error = %v, want unsupported proxy scheme", err)
	}
}
