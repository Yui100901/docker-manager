package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"docker-manager/internal/sensitive"

	"github.com/spf13/cobra"
)

func TestCommandErrorKeepsSensitiveTextByDefault(t *testing.T) {
	t.Cleanup(func() { sensitive.SetDefaultProfile(sensitive.ProfileNone) })
	sensitive.SetDefaultProfile(sensitive.ProfileNone)
	var output bytes.Buffer
	writeCommandError(&output, errors.New("proxy=http://admin:proxy-secret@proxy.example token=error-secret"), outputOptions{})
	for _, want := range []string{"proxy-secret", "error-secret"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("default error output = %q, want %q", output.String(), want)
		}
	}
}

func TestCommandErrorRedactsTextAndJSONWhenEnabled(t *testing.T) {
	t.Cleanup(func() { sensitive.SetDefaultProfile(sensitive.ProfileNone) })
	sensitive.SetDefaultProfile(sensitive.ProfileBasic)
	for _, jsonOutput := range []bool{false, true} {
		var output bytes.Buffer
		writeCommandError(&output, errors.New("Authorization: Basic dXNlcjpwYXNz token=error-secret"), outputOptions{JSON: jsonOutput})
		got := output.String()
		for _, leaked := range []string{"dXNlcjpwYXNz", "error-secret"} {
			if strings.Contains(got, leaked) {
				t.Fatalf("json=%v error leaked %q: %s", jsonOutput, leaked, got)
			}
		}
		if !strings.Contains(got, "redacted") {
			t.Fatalf("json=%v error output = %q, want redaction", jsonOutput, got)
		}
	}
}

func TestJSONLogWriterUsesActiveRedactionProfile(t *testing.T) {
	t.Cleanup(func() { sensitive.SetDefaultProfile(sensitive.ProfileNone) })
	var output bytes.Buffer
	writer := &jsonLogWriter{out: &output}
	sensitive.SetDefaultProfile(sensitive.ProfileStrict)
	_, err := writer.Write([]byte("Cookie: session=log-secret\nAuthorization: Bearer bearer-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, leaked := range []string{"log-secret", "bearer-secret"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("JSON log leaked %q: %s", leaked, got)
		}
	}
}

func TestParsedRedactionFlagsProtectFlagParseErrors(t *testing.T) {
	t.Cleanup(func() { sensitive.SetDefaultProfile(sensitive.ProfileNone) })
	tests := []struct {
		name       string
		args       []string
		secret     string
		wantRedact bool
	}{
		{
			name:       "basic equals form",
			args:       []string{"--redact-profile=basic", "--docker-timeout=token=basic-secret", "version"},
			secret:     "basic-secret",
			wantRedact: true,
		},
		{
			name:       "strict separate form",
			args:       []string{"--redact-profile", "strict", "--docker-timeout", "token=strict-secret", "version"},
			secret:     "strict-secret",
			wantRedact: true,
		},
		{
			name:       "legacy boolean",
			args:       []string{"--redact-secrets", "--docker-timeout=token=legacy-secret", "version"},
			secret:     "legacy-secret",
			wantRedact: true,
		},
		{
			name:       "explicit none beats legacy true",
			args:       []string{"--redact-profile=none", "--redact-secrets", "--docker-timeout=token=admin-secret", "version"},
			secret:     "admin-secret",
			wantRedact: false,
		},
		{
			name:       "last repeated profile wins",
			args:       []string{"--redact-profile=none", "--redact-profile=basic", "--docker-timeout=token=repeated-secret", "version"},
			secret:     "repeated-secret",
			wantRedact: true,
		},
		{
			name:       "last repeated none remains administrator visible",
			args:       []string{"--redact-profile=basic", "--redact-profile=none", "--docker-timeout=token=repeated-admin-secret", "version"},
			secret:     "repeated-admin-secret",
			wantRedact: false,
		},
		{
			name:       "legacy false remains administrator visible",
			args:       []string{"--redact-secrets=false", "--docker-timeout=token=false-secret", "version"},
			secret:     "false-secret",
			wantRedact: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sensitive.SetDefaultProfile(sensitive.ProfileNone)
			cfg := appConfig{}
			opts := outputOptions{}
			cmd := newRootCommand(&cfg, &opts)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)
			preseededProfile, profilePreseeded := preseedRedactionProfileForError(tt.args)

			executedCmd, err := cmd.ExecuteC()
			if err == nil {
				t.Fatal("ExecuteC() error = nil, want flag parse error")
			}
			applyParsedRedactProfileForError(executedCmd)
			if profilePreseeded {
				sensitive.SetDefaultProfile(preseededProfile)
			}
			var output bytes.Buffer
			writeCommandError(&output, err, opts)
			got := output.String()
			if tt.wantRedact {
				if strings.Contains(got, tt.secret) || !strings.Contains(got, sensitive.RedactedValue) {
					t.Fatalf("error output = %q, want %q redacted", got, tt.secret)
				}
			} else if !strings.Contains(got, tt.secret) {
				t.Fatalf("error output = %q, want administrator-visible %q", got, tt.secret)
			}
		})
	}
}

func TestPreseedRedactionFlagsAcrossParseErrors(t *testing.T) {
	t.Cleanup(func() { sensitive.SetDefaultProfile(sensitive.ProfileNone) })
	tests := []struct {
		name        string
		args        []string
		wantProfile sensitive.Profile
		wantFound   bool
		secret      string
	}{
		{
			name:        "missing profile value",
			args:        []string{"--redact-profile"},
			wantProfile: sensitive.ProfileNone,
			wantFound:   false,
		},
		{
			name:        "profile after earlier parse error",
			args:        []string{"--docker-timeout=token=early-secret", "--redact-profile=basic", "version"},
			wantProfile: sensitive.ProfileBasic,
			wantFound:   true,
			secret:      "early-secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sensitive.SetDefaultProfile(sensitive.ProfileNone)
			preseededProfile, profilePreseeded := preseedRedactionProfileForError(tt.args)
			if preseededProfile != tt.wantProfile || profilePreseeded != tt.wantFound {
				t.Fatalf("preseed = (%q, %v), want (%q, %v)", preseededProfile, profilePreseeded, tt.wantProfile, tt.wantFound)
			}
			cfg := appConfig{}
			opts := outputOptions{}
			cmd := newRootCommand(&cfg, &opts)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)

			executedCmd, err := cmd.ExecuteC()
			if err == nil {
				t.Fatal("ExecuteC() error = nil, want parse error")
			}
			applyParsedRedactProfileForError(executedCmd)
			if profilePreseeded {
				sensitive.SetDefaultProfile(preseededProfile)
			}
			if got := sensitive.DefaultProfile(); got != tt.wantProfile {
				t.Fatalf("default profile = %q, want %q", got, tt.wantProfile)
			}
			if tt.secret != "" {
				var output bytes.Buffer
				writeCommandError(&output, err, opts)
				if got := output.String(); strings.Contains(got, tt.secret) || !strings.Contains(got, sensitive.RedactedValue) {
					t.Fatalf("error output = %q, want %q redacted", got, tt.secret)
				}
			}
		})
	}
}

func TestPreseedRedactionProfileSemantics(t *testing.T) {
	t.Cleanup(func() { sensitive.SetDefaultProfile(sensitive.ProfileNone) })
	tests := []struct {
		name        string
		args        []string
		wantProfile sensitive.Profile
		wantFound   bool
	}{
		{name: "equals profile", args: []string{"--redact-profile=strict"}, wantProfile: sensitive.ProfileStrict, wantFound: true},
		{name: "separate profile", args: []string{"--redact-profile", "basic"}, wantProfile: sensitive.ProfileBasic, wantFound: true},
		{name: "legacy true", args: []string{"--redact-secrets"}, wantProfile: sensitive.ProfileBasic, wantFound: true},
		{name: "legacy false", args: []string{"--redact-secrets=false"}, wantProfile: sensitive.ProfileNone, wantFound: true},
		{name: "profile beats later legacy", args: []string{"--redact-profile=none", "--redact-secrets"}, wantProfile: sensitive.ProfileNone, wantFound: true},
		{name: "profile beats earlier legacy", args: []string{"--redact-secrets=false", "--redact-profile=strict"}, wantProfile: sensitive.ProfileStrict, wantFound: true},
		{name: "last valid profile wins", args: []string{"--redact-profile=basic", "--redact-profile=invalid", "--redact-profile", "strict"}, wantProfile: sensitive.ProfileStrict, wantFound: true},
		{name: "invalid profile keeps earlier valid", args: []string{"--redact-profile=basic", "--redact-profile=invalid"}, wantProfile: sensitive.ProfileBasic, wantFound: true},
		{name: "terminator ignores following profile", args: []string{"--", "--redact-profile=strict"}, wantProfile: sensitive.ProfileNone, wantFound: false},
		{name: "terminator keeps preceding profile", args: []string{"--redact-profile=basic", "--", "--redact-profile=none"}, wantProfile: sensitive.ProfileBasic, wantFound: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sensitive.SetDefaultProfile(sensitive.ProfileNone)
			gotProfile, gotFound := preseedRedactionProfileForError(tt.args)
			if gotProfile != tt.wantProfile || gotFound != tt.wantFound {
				t.Fatalf("preseed = (%q, %v), want (%q, %v)", gotProfile, gotFound, tt.wantProfile, tt.wantFound)
			}
			if got := sensitive.DefaultProfile(); got != tt.wantProfile {
				t.Fatalf("default profile = %q, want %q", got, tt.wantProfile)
			}
		})
	}
}

func TestParsedLocalRedactionFlagsOverrideRootOnParseError(t *testing.T) {
	t.Cleanup(func() { sensitive.SetDefaultProfile(sensitive.ProfileNone) })
	tests := []struct {
		name        string
		rootArgs    []string
		localArgs   []string
		secret      string
		wantProfile sensitive.Profile
		wantRedact  bool
	}{
		{
			name:        "explicit profile beats legacy false",
			rootArgs:    []string{"--redact-profile=strict"},
			localArgs:   []string{"--redact-secrets=false"},
			secret:      "local-admin-secret",
			wantProfile: sensitive.ProfileStrict,
			wantRedact:  true,
		},
		{
			name:        "local basic overrides root none",
			rootArgs:    []string{"--redact-profile=none"},
			localArgs:   []string{"--redact-profile=basic"},
			secret:      "local-basic-secret",
			wantProfile: sensitive.ProfileBasic,
			wantRedact:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sensitive.SetDefaultProfile(sensitive.ProfileNone)
			root := newRedactionErrorTestCommand()
			args := append(append(append([]string(nil), tt.rootArgs...), "child"), tt.localArgs...)
			args = append(args, "--duration=token="+tt.secret)
			root.SetArgs(args)
			preseededProfile, profilePreseeded := preseedRedactionProfileForError(args)

			executedCmd, err := root.ExecuteC()
			if err == nil {
				t.Fatal("ExecuteC() error = nil, want local flag parse error")
			}
			applyParsedRedactProfileForError(executedCmd)
			if profilePreseeded {
				sensitive.SetDefaultProfile(preseededProfile)
			}
			if got := sensitive.DefaultProfile(); got != tt.wantProfile {
				t.Fatalf("default profile = %q, want %q", got, tt.wantProfile)
			}

			var output bytes.Buffer
			writeCommandError(&output, err, outputOptions{})
			got := output.String()
			if tt.wantRedact {
				if strings.Contains(got, tt.secret) || !strings.Contains(got, sensitive.RedactedValue) {
					t.Fatalf("error output = %q, want %q redacted", got, tt.secret)
				}
			} else if !strings.Contains(got, tt.secret) {
				t.Fatalf("error output = %q, want administrator-visible %q", got, tt.secret)
			}
		})
	}
}

func TestParsedLocalRedactionFlagsProtectArgsValidationErrors(t *testing.T) {
	t.Cleanup(func() { sensitive.SetDefaultProfile(sensitive.ProfileNone) })
	sensitive.SetDefaultProfile(sensitive.ProfileNone)
	root := newRedactionErrorTestCommand()
	args := []string{"child", "--redact-profile=strict", "token=args-secret"}
	root.SetArgs(args)
	preseededProfile, profilePreseeded := preseedRedactionProfileForError(args)

	executedCmd, err := root.ExecuteC()
	if err == nil {
		t.Fatal("ExecuteC() error = nil, want args validation error")
	}
	applyParsedRedactProfileForError(executedCmd)
	if profilePreseeded {
		sensitive.SetDefaultProfile(preseededProfile)
	}
	if got := sensitive.DefaultProfile(); got != sensitive.ProfileStrict {
		t.Fatalf("default profile = %q, want strict", got)
	}

	var output bytes.Buffer
	writeCommandError(&output, err, outputOptions{})
	got := output.String()
	if strings.Contains(got, "args-secret") || !strings.Contains(got, sensitive.RedactedValue) {
		t.Fatalf("error output = %q, want args secret redacted", got)
	}
}

func newRedactionErrorTestCommand() *cobra.Command {
	root := &cobra.Command{Use: "root", SilenceErrors: true, SilenceUsage: true}
	root.PersistentFlags().String("redact-profile", "", "")
	root.PersistentFlags().Bool("redact-secrets", false, "")

	child := &cobra.Command{Use: "child", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {}}
	child.Flags().String("redact-profile", "", "")
	child.Flags().Bool("redact-secrets", false, "")
	child.Flags().Duration("duration", 0, "")
	root.AddCommand(child)
	return root
}
