package cli

import (
	"testing"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/audit"
)

func TestConfigShowEffectiveAuditFlagsOverrideConfigAndSources(t *testing.T) {
	cfg := appConfig{
		AuditFile:     "configured.jsonl",
		AuditActor:    "configured-actor",
		AuditDetail:   "safe",
		AuditOnError:  "warn",
		AuditMaxBytes: 128 << 10,
		AuditMaxFiles: 9,
		AuditKeyFile:  "configured.key",
	}
	loaded := auditConfigLoaded("test.yaml")
	root := newRootCommand(&cfg, &outputOptions{})
	show, _, err := root.Find([]string{"config", "show"})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"audit-file":      "flag.jsonl",
		"audit-actor":     "flag-actor",
		"audit-detail":    "full",
		"audit-on-error":  "warn",
		"audit-required":  "true",
		"audit-key-file":  "flag.key",
		"audit-max-bytes": "262144",
		"audit-max-files": "7",
	} {
		if err := root.PersistentFlags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
	}

	report := buildConfigShowReport(show, &cfg, &loaded, true, true)
	assertConfigShowValueSource(t, report, "audit_file", "flag.jsonl", "flag:--audit-file")
	assertConfigShowValueSource(t, report, "audit_actor", "flag-actor", "flag:--audit-actor")
	assertConfigShowValueSource(t, report, "audit_detail", "full", "flag:--audit-detail")
	assertConfigShowValueSource(t, report, "audit_on_error", "fail", "flag:--audit-required")
	assertConfigShowValueSource(t, report, "audit_required", true, "flag:--audit-required")
	assertConfigShowValueSource(t, report, "audit_key_file", "flag.key", "flag:--audit-key-file")
	assertConfigShowValueSource(t, report, "audit_max_bytes", int64(262144), "flag:--audit-max-bytes")
	assertConfigShowValueSource(t, report, "audit_max_files", 7, "flag:--audit-max-files")
}

func TestConfigShowEffectiveAuditExplicitEmptyAndZeroUseRuntimeDefaults(t *testing.T) {
	cfg := appConfig{
		AuditFile:     "configured.jsonl",
		AuditActor:    "configured-actor",
		AuditDetail:   "full",
		AuditOnError:  "fail",
		AuditMaxBytes: 128 << 10,
		AuditMaxFiles: 9,
		AuditKeyFile:  "configured.key",
	}
	loaded := auditConfigLoaded("test.yaml")
	root := newRootCommand(&cfg, &outputOptions{})
	show, _, err := root.Find([]string{"config", "show"})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"audit-file":      "",
		"audit-actor":     "",
		"audit-detail":    "",
		"audit-on-error":  "",
		"audit-required":  "false",
		"audit-key-file":  "",
		"audit-max-bytes": "0",
		"audit-max-files": "0",
	} {
		if err := root.PersistentFlags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
	}

	report := buildConfigShowReport(show, &cfg, &loaded, true, true)
	assertConfigShowValueSource(t, report, "audit_file", "", "flag:--audit-file")
	assertConfigShowValueSource(t, report, "audit_actor", "", "flag:--audit-actor")
	assertConfigShowValueSource(t, report, "audit_detail", "safe", "flag:--audit-detail")
	assertConfigShowValueSource(t, report, "audit_on_error", "deny-mutation", "flag:--audit-on-error")
	assertConfigShowValueSource(t, report, "audit_required", false, "flag:--audit-required")
	assertConfigShowValueSource(t, report, "audit_key_file", "", "flag:--audit-key-file")
	assertConfigShowValueSource(t, report, "audit_max_bytes", audit.DefaultAuditMaxBytes, "flag:--audit-max-bytes")
	assertConfigShowValueSource(t, report, "audit_max_files", audit.DefaultAuditMaxFiles, "flag:--audit-max-files")
}

func auditConfigLoaded(path string) appconfig.Loaded {
	fields := map[string]bool{}
	for _, field := range []string{
		"audit_file", "audit_actor", "audit_detail", "audit_on_error",
		"audit_max_bytes", "audit_max_files", "audit_key_file",
	} {
		fields[field] = true
	}
	return appconfig.Loaded{Path: path, Exists: true, Fields: fields}
}

func assertConfigShowValueSource(t *testing.T, report configShowReport, field string, value any, source string) {
	t.Helper()
	if report.Values[field] != value || report.Sources[field] != source {
		t.Fatalf("%s = %#v source=%q, want %#v source=%q", field, report.Values[field], report.Sources[field], value, source)
	}
}
