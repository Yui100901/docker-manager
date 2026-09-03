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

func TestRegistryPolicyRejectsProxySurroundingWhitespace(t *testing.T) {
	err := (RegistryPolicy{Proxy: " http://proxy.example:8080 "}).Validate()
	if err == nil || !strings.Contains(err.Error(), "leading or trailing whitespace") {
		t.Fatalf("Validate() error = %v, want surrounding whitespace rejection", err)
	}
}

func TestRegistryPolicyRejectsWhitespaceOnlyProxy(t *testing.T) {
	err := (RegistryPolicy{Proxy: "   "}).Validate()
	if err == nil || !strings.Contains(err.Error(), "leading or trailing whitespace") {
		t.Fatalf("Validate() error = %v, want whitespace-only rejection", err)
	}
}
