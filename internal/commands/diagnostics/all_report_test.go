package diagnostics

import (
	"bytes"
	"strings"
	"testing"
)

func TestSelectReportAllKindsHonorsIncludeAndSkip(t *testing.T) {
	got, err := selectReportAllKinds([]string{"logs,health", "volume"}, []string{"logs"})
	if err != nil {
		t.Fatalf("selectReportAllKinds() error = %v", err)
	}
	want := []string{"health", "volumes"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("selected = %#v, want %#v", got, want)
	}
}

func TestSelectReportAllKindsRejectsUnknownKind(t *testing.T) {
	if _, err := selectReportAllKinds([]string{"health,bad"}, nil); err == nil {
		t.Fatal("selectReportAllKinds() error = nil, want error")
	}
}

func TestPrintReportAllIncludesSectionStatus(t *testing.T) {
	report := ReportAllReport{
		GeneratedAt:    "2026-07-08T00:00:00Z",
		DockerEndpoint: "unix:///var/run/docker.sock",
		Selected:       []string{"health", "network"},
		Sections: []ReportAllSection{
			{Name: "health", Status: "ok", DurationMillis: 12},
			{Name: "network", Status: "failed", DurationMillis: 5, Error: "boom"},
		},
		Health: &HealthReport{GeneratedAt: "2026-07-08T00:00:00Z"},
	}
	var out bytes.Buffer
	printReportAll(&out, report, ReportAllOptions{})
	text := out.String()
	for _, want := range []string{"Docker 聚合报告", "摘要: 报告=2 成功=1 失败=1", "## health [ok]", "## network [failed]", "错误: boom"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}
