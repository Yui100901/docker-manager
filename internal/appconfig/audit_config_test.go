package appconfig

import (
	"fmt"
	"strings"
	"testing"

	"docker-manager/internal/audit"
)

func TestAuditMaxBytesValidationUsesDefaultEventSizeFloor(t *testing.T) {
	minimum := int64(audit.DefaultMaxEventBytes)
	for _, test := range []struct {
		name    string
		value   int64
		wantErr bool
	}{
		{name: "disabled", value: 0},
		{name: "exact event floor", value: minimum},
		{name: "below event floor", value: minimum - 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (Config{AuditMaxBytes: test.value}).Validate()
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), fmt.Sprint(audit.DefaultMaxEventBytes)) {
					t.Fatalf("Config.Validate(audit_max_bytes=%d) error = %v, want event-size floor", test.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Config.Validate(audit_max_bytes=%d) error = %v", test.value, err)
			}
		})
	}
}

func TestLoadValidatesAuditMaxBytesForBaseAndProfiles(t *testing.T) {
	minimum := audit.DefaultMaxEventBytes
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{name: "base exact floor", data: fmt.Sprintf("audit_max_bytes: %d\n", minimum)},
		{name: "base below floor", data: fmt.Sprintf("audit_max_bytes: %d\n", minimum-1), wantErr: "audit_max_bytes"},
		{name: "profile exact floor", data: fmt.Sprintf("profiles:\n  production:\n    audit_max_bytes: %d\n", minimum)},
		{name: "profile explicit zero", data: fmt.Sprintf("audit_max_bytes: %d\nprofiles:\n  production:\n    audit_max_bytes: 0\n", minimum)},
		{name: "profile below floor", data: fmt.Sprintf("profiles:\n  production:\n    audit_max_bytes: %d\n", minimum-1), wantErr: `profile "production"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadWithOptions(writeConfig(t, test.data), LoadOptions{Required: true})
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadWithOptions() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("LoadWithOptions() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
