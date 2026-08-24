package diagnostics

import (
	"bytes"
	"io"
	"strings"
	"testing"

	rpt "docker-manager/internal/report"
	"docker-manager/internal/sensitive"
)

func TestDoctorReportRedactionCoversAllFormats(t *testing.T) {
	report := DoctorReport{
		OverallStatus: "warning",
		Checks: []DoctorCheck{{
			Name:    "proxy",
			Status:  "warning",
			Message: "Authorization: Basic dXNlcjpwYXNz",
			Detail:  "HTTPS_PROXY=http://admin:proxy-secret@proxy.example token=doctor-secret",
		}},
	}
	for _, format := range []string{rpt.FormatText, rpt.FormatJSON, rpt.FormatMarkdown, rpt.FormatHTML} {
		t.Run(format, func(t *testing.T) {
			var output bytes.Buffer
			err := rpt.PrintWithProfile(&output, format, report, func(w io.Writer) {
				printDoctorReport(w, report)
			}, sensitive.ProfileBasic)
			if err != nil {
				t.Fatal(err)
			}
			got := output.String()
			for _, leaked := range []string{"dXNlcjpwYXNz", "proxy-secret", "doctor-secret"} {
				if strings.Contains(got, leaked) {
					t.Fatalf("%s doctor output leaked %q: %s", format, leaked, got)
				}
			}
		})
	}
}

func TestDoctorReportKeepsSensitiveTextWithDefaultNone(t *testing.T) {
	report := DoctorReport{Checks: []DoctorCheck{{Name: "proxy", Detail: "proxy=http://admin:default-secret@proxy.example"}}}
	var output bytes.Buffer
	if err := rpt.PrintWithProfile(&output, rpt.FormatJSON, report, func(io.Writer) {}, sensitive.ProfileNone); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "default-secret") {
		t.Fatalf("default doctor output = %q, want administrator-visible value", output.String())
	}
}

func TestStrictDoctorRedactionCoversCookiesAndPrivateKeys(t *testing.T) {
	report := DoctorReport{Checks: []DoctorCheck{{
		Name:   "helper",
		Detail: "Cookie: session=helper-secret\n-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----",
	}}}
	var output bytes.Buffer
	if err := rpt.PrintWithProfile(&output, rpt.FormatText, report, func(w io.Writer) {
		printDoctorReport(w, report)
	}, sensitive.ProfileStrict); err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"helper-secret", "private-material", "BEGIN PRIVATE KEY"} {
		if strings.Contains(output.String(), leaked) {
			t.Fatalf("strict doctor output leaked %q: %s", leaked, output.String())
		}
	}
}

func TestNormalizeRedactProfileInheritsGlobalAndAllowsExplicitNone(t *testing.T) {
	previous := sensitive.DefaultProfile()
	sensitive.SetDefaultProfile(sensitive.ProfileStrict)
	t.Cleanup(func() { sensitive.SetDefaultProfile(previous) })

	profile, err := normalizeRedactProfile("", false)
	if err != nil {
		t.Fatal(err)
	}
	if profile != sensitive.ProfileStrict {
		t.Fatalf("inherited profile = %q, want strict", profile)
	}

	profile, err = normalizeRedactProfile("none", false)
	if err != nil {
		t.Fatal(err)
	}
	if profile != sensitive.ProfileNone {
		t.Fatalf("explicit profile = %q, want none", profile)
	}
}
