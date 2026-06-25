package report

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
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
