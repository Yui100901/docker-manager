package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types"
)

type fakeDoctorDockerService struct {
	pingErr    error
	versionErr error
}

func (f fakeDoctorDockerService) Ping(ctx context.Context) (types.Ping, error) {
	if f.pingErr != nil {
		return types.Ping{}, f.pingErr
	}
	return types.Ping{APIVersion: "1.48", OSType: "linux"}, nil
}

func (f fakeDoctorDockerService) ServerVersion(ctx context.Context) (types.Version, error) {
	if f.versionErr != nil {
		return types.Version{}, f.versionErr
	}
	return types.Version{Version: "28.1.1", APIVersion: "1.48", Os: "linux", Arch: "amd64"}, nil
}

func (f fakeDoctorDockerService) DaemonHost() string {
	return "unix:///var/run/docker.sock"
}

func (f fakeDoctorDockerService) ClientVersion() string {
	return "1.48"
}

func TestDoctorOverallStatus(t *testing.T) {
	if got := doctorOverallStatus([]DoctorCheck{{Status: "ok"}, {Status: "skipped"}}); got != "ok" {
		t.Fatalf("doctorOverallStatus ok = %q", got)
	}
	if got := doctorOverallStatus([]DoctorCheck{{Status: "ok"}, {Status: "warning"}}); got != "warning" {
		t.Fatalf("doctorOverallStatus warning = %q", got)
	}
	if got := doctorOverallStatus([]DoctorCheck{{Status: "warning"}, {Status: "failed"}}); got != "failed" {
		t.Fatalf("doctorOverallStatus failed = %q", got)
	}
}

func TestCheckDoctorConfigParsesYaml(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dm.yaml")
	t.Setenv("HTTP_PROXY", "")
	if err := os.WriteFile(path, []byte("proxy: http://127.0.0.1:7890\nos: linux\narch: amd64\noutput_dir: dist\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, checks := checkDoctorConfig(path)
	if cfg.Proxy != "http://127.0.0.1:7890" {
		t.Fatalf("cfg.Proxy = %q", cfg.Proxy)
	}
	if len(checks) != 1 || checks[0].Status != "ok" {
		t.Fatalf("checks = %#v, want ok", checks)
	}
}

func TestCheckDoctorProxyWarnsOnInvalidURL(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")

	checks := checkDoctorProxy(doctorConfig{Proxy: "127.0.0.1:7890"})

	if len(checks) != 1 || checks[0].Status != "warning" {
		t.Fatalf("checks = %#v, want proxy warning", checks)
	}
}

func TestCheckDoctorCAReportsConfiguredPaths(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	t.Setenv("DOCKER_CERT_PATH", "")

	checks := checkDoctorCA(doctorConfig{CAFile: caFile})

	if len(checks) != 1 || checks[0].Status != "ok" {
		t.Fatalf("checks = %#v, want private-ca ok", checks)
	}
}

func TestCheckDoctorCAWarnsOnMissingPath(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	t.Setenv("DOCKER_CERT_PATH", "")

	checks := checkDoctorCA(doctorConfig{CAFile: filepath.Join(t.TempDir(), "missing.pem")})

	if len(checks) != 1 || checks[0].Status != "warning" {
		t.Fatalf("checks = %#v, want private-ca warning", checks)
	}
}

func TestCheckDoctorDiskProbesWritableOutputDir(t *testing.T) {
	check := checkDoctorDisk(t.TempDir(), 1)

	if check.Status != "ok" {
		t.Fatalf("check = %#v, want ok", check)
	}
	if check.Detail == "" {
		t.Fatalf("check = %#v, want detail", check)
	}
}

func TestRunDoctorReportsDockerFailureAndSkippedRegistry(t *testing.T) {
	old := newDoctorDockerService
	newDoctorDockerService = func() (doctorDockerService, error) {
		return fakeDoctorDockerService{pingErr: errors.New("daemon down")}, nil
	}
	defer func() { newDoctorDockerService = old }()

	report := runDoctor(context.Background(), DoctorOptions{
		ConfigPath:    t.TempDir() + "/missing.yaml",
		OutputDir:     t.TempDir(),
		Timeout:       1,
		CheckE2E:      false,
		MinDiskFreeMB: 1,
	})
	if report.OverallStatus != "failed" {
		t.Fatalf("OverallStatus = %q, want failed", report.OverallStatus)
	}
	if !hasDoctorCheck(report.Checks, "docker-daemon", "failed") {
		t.Fatalf("checks = %#v, want docker-daemon failed", report.Checks)
	}
	if !hasDoctorCheck(report.Checks, "registry", "skipped") {
		t.Fatalf("checks = %#v, want registry skipped", report.Checks)
	}
}

func TestDoctorCommandSupportsJSON(t *testing.T) {
	old := newDoctorDockerService
	newDoctorDockerService = func() (doctorDockerService, error) {
		return fakeDoctorDockerService{}, nil
	}
	defer func() { newDoctorDockerService = old }()

	cmd := NewDoctorCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "--dm-config", t.TempDir() + "/missing.yaml", "--output-dir", t.TempDir(), "--check-e2e=false"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var report DoctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal() error = %v output=%q", err, out.String())
	}
	if report.OverallStatus == "" || len(report.Checks) == 0 {
		t.Fatalf("report = %#v, want status and checks", report)
	}
}

func hasDoctorCheck(checks []DoctorCheck, name, status string) bool {
	for _, check := range checks {
		if check.Name == name && check.Status == status {
			return true
		}
	}
	return false
}
