package report

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestPrintReportJSON(t *testing.T) {
	var out bytes.Buffer
	report := map[string]string{"status": "ok"}

	if err := Print(&out, FormatJSON, report, func(w io.Writer) {
		t.Fatal("text printer should not run for json format")
	}); err != nil {
		t.Fatalf("Print() error = %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v output=%q", err, out.String())
	}
	if got["status"] != "ok" {
		t.Fatalf("json = %#v, want status=ok", got)
	}
}

func TestAddFormatFlagDescribesBusinessReportOutput(t *testing.T) {
	var format string
	cmd := &cobra.Command{Use: "demo"}
	AddFormatFlag(cmd, &format)
	flag := cmd.Flags().Lookup("format")
	if flag == nil {
		t.Fatal("format flag missing")
	}
	if !strings.Contains(flag.Usage, "业务报告输出格式") {
		t.Fatalf("format usage = %q, want business report wording", flag.Usage)
	}
}

func TestPrintReportText(t *testing.T) {
	var out bytes.Buffer
	if err := Print(&out, FormatText, map[string]string{"status": "ok"}, func(w io.Writer) {
		_, _ = w.Write([]byte("plain"))
	}); err != nil {
		t.Fatalf("Print() error = %v", err)
	}
	if out.String() != "plain" {
		t.Fatalf("output = %q, want plain", out.String())
	}
}

func TestPrintReportRejectsUnknownFormat(t *testing.T) {
	var out bytes.Buffer
	err := Print(&out, "xml", map[string]string{}, func(w io.Writer) {})
	if err == nil {
		t.Fatal("Print() error = nil, want unsupported format")
	}
	if !strings.Contains(err.Error(), "不支持的输出格式") {
		t.Fatalf("error = %v", err)
	}
}

func TestPrintReportMarkdown(t *testing.T) {
	var out bytes.Buffer
	report := sampleReport{
		GeneratedAt: "2026-06-26T12:00:00Z",
		Status:      "warning",
		Items: []sampleItem{
			{Name: "api", Status: "ok", Count: 2},
			{Name: "db", Status: "failed", Count: 1},
		},
	}

	if err := Print(&out, FormatMarkdown, report, func(w io.Writer) {
		t.Fatal("text printer should not run for markdown format")
	}); err != nil {
		t.Fatalf("Print() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "# Sample Report") || !strings.Contains(got, "| name | status | count |") || !strings.Contains(got, "| api | ok | 2 |") {
		t.Fatalf("markdown output = %q", got)
	}
}

func TestPrintReportHTML(t *testing.T) {
	var out bytes.Buffer
	report := sampleReport{Status: "ok", Items: []sampleItem{{Name: "api", Status: "ok"}}}

	if err := Print(&out, FormatHTML, report, func(w io.Writer) {
		t.Fatal("text printer should not run for html format")
	}); err != nil {
		t.Fatalf("Print() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "<!doctype html>") || !strings.Contains(got, "<h1>Sample Report</h1>") || !strings.Contains(got, "status-ok") {
		t.Fatalf("html output = %q", got)
	}
}

func TestPrintReportMarkdownAlias(t *testing.T) {
	var out bytes.Buffer
	if err := Print(&out, "md", sampleReport{Status: "ok"}, func(w io.Writer) {}); err != nil {
		t.Fatalf("Print() error = %v", err)
	}
	if !strings.Contains(out.String(), "# Sample Report") {
		t.Fatalf("markdown alias output = %q", out.String())
	}
}

type sampleReport struct {
	GeneratedAt string       `json:"generated_at,omitempty"`
	Status      string       `json:"status"`
	Items       []sampleItem `json:"items,omitempty"`
}

type sampleItem struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Count  int    `json:"count,omitempty"`
}
