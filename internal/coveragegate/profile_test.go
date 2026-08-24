package coveragegate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleProfile = `mode: atomic
docker-manager/internal/appconfig/config.go:10.1,12.2 2 1
docker-manager/internal/appconfig/config.go:14.1,15.2 1 0
docker-manager/internal/appconfig/config.go:16.1,16.1 0 3
docker-manager/internal/docker/client.go:20.1,24.2 2 3
`

func TestParseAndPackageLookup(t *testing.T) {
	profile, err := Parse(strings.NewReader(sampleProfile))
	if err != nil {
		t.Fatal(err)
	}
	if profile.Total != (Stats{Covered: 4, Total: 5}) || profile.Total.Percent() != 80 {
		t.Fatalf("Total = %#v (%.2f%%)", profile.Total, profile.Total.Percent())
	}
	appconfig, err := profile.Package("./internal/appconfig")
	if err != nil {
		t.Fatal(err)
	}
	if appconfig != (Stats{Covered: 2, Total: 3}) {
		t.Fatalf("appconfig = %#v", appconfig)
	}
	if _, err := profile.Package("internal/missing"); err == nil {
		t.Fatal("missing package error = nil")
	}
}

func TestParseRejectsMalformedProfiles(t *testing.T) {
	tests := []string{
		"",
		"not-a-mode\n",
		"mode: atomic\n",
		"mode: atomic\nbad 1\n",
		"mode: atomic\nbad-location 1 1\n",
		"mode: atomic\na.go:1.1,1.2 -1 1\n",
		"mode: atomic\na.go:1.1,1.2 1 nope\n",
	}
	for _, input := range tests {
		if _, err := Parse(strings.NewReader(input)); err == nil {
			t.Fatalf("Parse(%q) error = nil", input)
		}
	}
}

func TestRunPassesAndFailsThresholds(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "coverage.out")
	if err := os.WriteFile(profilePath, []byte(sampleProfile), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"-profile", profilePath,
		"-total", "80",
		"-package", "internal/appconfig=66",
		"-package", "internal/docker=100",
	}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "PASS") {
		t.Fatalf("Run pass code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"-profile", profilePath, "-total", "81", "-package", "internal/missing=1"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), "FAIL") || !strings.Contains(stderr.String(), "missing") {
		t.Fatalf("Run fail code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	tests := [][]string{
		nil,
		{"-profile", "x", "-total", "101"},
		{"-profile", "x", "-total", "NaN"},
		{"-profile", "x", "-package", "bad"},
		{"-profile", "x", "-package", "p=nope"},
		{"-profile", "x", "-package", "p=1", "-package", "p=2"},
		{"-profile", "x", "extra"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("Run(%q) code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}
