package reverse

import (
	"strings"
	"testing"
)

func TestReverseRerunRequiresConfirm(t *testing.T) {
	cmd := NewReverseCommand()
	cmd.SetArgs([]string{"demo", "--rerun"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want confirmation error")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("Execute() error = %q, want --confirm hint", err.Error())
	}
}

func TestReverseRerunDryRunDoesNotRequireConfirm(t *testing.T) {
	cmd := NewReverseCommand()
	cmd.SetArgs([]string{"demo", "--rerun", "--dry-run", "--reverse-type", "invalid"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid type error after confirm gate")
	}
	if strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("Execute() error = %q, did not expect --confirm gate for dry-run", err.Error())
	}
	if !strings.Contains(err.Error(), "无效的输出类型") {
		t.Fatalf("Execute() error = %q, want invalid type error", err.Error())
	}
}

func TestReverseRerunConfirmPassesConfirmGate(t *testing.T) {
	cmd := NewReverseCommand()
	cmd.SetArgs([]string{"demo", "--rerun", "--confirm", "--reverse-type", "invalid"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid type error after confirm gate")
	}
	if strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("Execute() error = %q, did not expect --confirm gate", err.Error())
	}
	if !strings.Contains(err.Error(), "无效的输出类型") {
		t.Fatalf("Execute() error = %q, want invalid type error", err.Error())
	}
}
