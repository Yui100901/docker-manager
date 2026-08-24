package registryauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"docker-manager/internal/sensitive"
)

const (
	DefaultCredentialHelperTimeout = 5 * time.Second
	maxCredentialHelperStdout      = 1 << 20
	maxCredentialHelperStderr      = 32 * 1024
)

type Config struct {
	Auths       map[string]AuthEntry `json:"auths"`
	CredsStore  string               `json:"credsStore"`
	CredHelpers map[string]string    `json:"credHelpers"`
}

type AuthEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

type Credential struct {
	Found         bool
	Source        string
	Helper        string
	HelperSource  string
	HelperPath    string
	Username      string
	Password      string
	IdentityToken string
	ServerAddress string
	Message       string
}

type HelperResponse struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

type HelperRunner func(ctx context.Context, helper, server string) (Credential, error)

type ResolveOptions struct {
	DisableHelpers bool
	HelperTimeout  time.Duration
	RunHelper      HelperRunner
}

func DefaultConfigPath() string {
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".docker", "config.json")
	}
	return filepath.Join(home, ".docker", "config.json")
}

func ReadConfig(path string) (Config, bool, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, false, nil
		}
		return cfg, false, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, true, err
	}
	return cfg, true, nil
}

func ResolveCredential(ctx context.Context, cfg Config, registryName string, runHelper HelperRunner) Credential {
	return ResolveCredentialWithOptions(ctx, cfg, registryName, ResolveOptions{RunHelper: runHelper})
}

func ResolveCredentialWithOptions(ctx context.Context, cfg Config, registryName string, opts ResolveOptions) Credential {
	runHelper := opts.RunHelper
	if runHelper == nil {
		runHelper = DefaultRunCredentialHelper
	}
	keys := ConfigKeys(registryName)
	helper, server, helperSource := FindCredentialHelperSource(cfg, keys)
	if helper != "" && !opts.DisableHelpers {
		timeout := opts.HelperTimeout
		if timeout <= 0 {
			timeout = DefaultCredentialHelperTimeout
		}
		helperCtx, cancel := context.WithTimeout(ctx, timeout)
		cred, err := runHelper(helperCtx, helper, server)
		cancel()
		if err != nil {
			return Credential{
				Source:        "credential-helper",
				Helper:        helper,
				HelperSource:  helperSource,
				HelperPath:    credentialHelperPath(helper),
				ServerAddress: server,
				Message:       credentialHelperFailureMessage(err),
			}
		}
		cred.Found = cred.Username != "" || cred.Password != "" || cred.IdentityToken != ""
		cred.Source = "credential-helper"
		cred.Helper = helper
		cred.HelperSource = helperSource
		if cred.HelperPath == "" {
			cred.HelperPath = credentialHelperPath(helper)
		}
		if cred.ServerAddress == "" {
			cred.ServerAddress = server
		}
		if !cred.Found {
			cred.Message = "credential helper returned no usable credential"
		}
		return cred
	}
	for _, key := range keys {
		entry, ok := cfg.Auths[key]
		if !ok {
			continue
		}
		cred := CredentialFromAuthEntry(entry)
		cred.Found = cred.Username != "" || cred.Password != "" || cred.IdentityToken != ""
		cred.Source = "auths"
		cred.ServerAddress = key
		if !cred.Found {
			cred.Message = "auths entry exists but contains no usable credential"
		}
		return cred
	}
	if helper != "" && opts.DisableHelpers {
		return Credential{
			Helper:        helper,
			HelperSource:  helperSource,
			HelperPath:    credentialHelperPath(helper),
			ServerAddress: server,
			Message:       "credential helpers are disabled; no matching auths entry",
		}
	}
	return Credential{Message: "no matching auths, credHelpers or credsStore entry"}
}

func credentialHelperFailureMessage(err error) string {
	if err == nil {
		return ""
	}
	if sensitive.DefaultProfile() == sensitive.ProfileNone {
		return err.Error()
	}
	// Helper errors can contain arbitrary external stderr, which cannot be
	// safely handled by pattern-based redaction.
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "credential helper timed out"
	case errors.Is(err, context.Canceled):
		return "credential helper canceled"
	default:
		return "credential helper failed: " + sensitive.RedactedValue
	}
}

func FindCredentialHelper(cfg Config, keys []string) (string, string) {
	helper, server, _ := FindCredentialHelperSource(cfg, keys)
	return helper, server
}

func FindCredentialHelperSource(cfg Config, keys []string) (string, string, string) {
	for _, key := range keys {
		if helper := strings.TrimSpace(cfg.CredHelpers[key]); helper != "" {
			return helper, key, "credHelpers[" + key + "]"
		}
	}
	if helper := strings.TrimSpace(cfg.CredsStore); helper != "" && len(keys) > 0 {
		return helper, credentialStoreServer(keys), "credsStore"
	}
	return "", "", ""
}

func credentialStoreServer(keys []string) string {
	for _, key := range keys {
		if key == "https://index.docker.io/v1/" {
			return key
		}
	}
	return keys[0]
}

func ConfigKeys(registryName string) []string {
	keys := []string{
		registryName,
		"https://" + registryName,
		"http://" + registryName,
		"https://" + registryName + "/v1/",
	}
	if registryName == "docker.io" || registryName == "registry-1.docker.io" || registryName == "index.docker.io" {
		keys = append(keys, "https://index.docker.io/v1/", "index.docker.io", "docker.io", "registry-1.docker.io")
	}
	return UniqueStrings(keys)
}

func CredentialFromAuthEntry(entry AuthEntry) Credential {
	cred := Credential{
		Username:      entry.Username,
		Password:      entry.Password,
		IdentityToken: entry.IdentityToken,
	}
	if cred.Username == "" && cred.Password == "" && entry.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err == nil {
			username, password, ok := strings.Cut(string(decoded), ":")
			if ok {
				cred.Username = username
				cred.Password = password
			}
		}
	}
	return cred
}

func DefaultRunCredentialHelper(ctx context.Context, helper, server string) (Credential, error) {
	name := "docker-credential-" + helper
	path, err := exec.LookPath(name)
	if err != nil {
		return Credential{}, fmt.Errorf("%s not found in PATH: %w", name, err)
	}
	cmd := exec.CommandContext(ctx, path, "get")
	cmd.Stdin = strings.NewReader(server + "\n")
	stdout := &limitedBuffer{remaining: maxCredentialHelperStdout}
	stderr := &limitedBuffer{remaining: maxCredentialHelperStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return Credential{}, fmt.Errorf("docker-credential-%s get failed: %s", helper, msg)
	}
	if stdout.truncated {
		return Credential{}, fmt.Errorf("docker-credential-%s output exceeds %d bytes", helper, maxCredentialHelperStdout)
	}
	var resp HelperResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return Credential{}, err
	}
	cred := Credential{
		Username:      resp.Username,
		Password:      resp.Secret,
		ServerAddress: resp.ServerURL,
		HelperPath:    path,
	}
	if resp.Username == "<token>" {
		cred.Username = ""
		cred.Password = ""
		cred.IdentityToken = resp.Secret
	}
	return cred, nil
}

func credentialHelperPath(helper string) string {
	path, err := exec.LookPath("docker-credential-" + helper)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		return abs
	}
	return path
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	originalLength := len(p)
	if w.remaining <= 0 {
		if originalLength > 0 {
			w.truncated = true
		}
		return originalLength, nil
	}
	if len(p) > w.remaining {
		w.truncated = true
		p = p[:w.remaining]
	}
	written, err := w.buffer.Write(p)
	w.remaining -= written
	if err != nil && err != io.ErrShortWrite {
		return written, err
	}
	return originalLength, nil
}

func (w *limitedBuffer) String() string {
	return w.buffer.String()
}

func (w *limitedBuffer) Bytes() []byte {
	return w.buffer.Bytes()
}

func BasicAuthHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func UniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
