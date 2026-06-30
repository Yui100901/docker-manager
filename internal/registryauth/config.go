package registryauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if runHelper == nil {
		runHelper = DefaultRunCredentialHelper
	}
	keys := ConfigKeys(registryName)
	if helper, server := FindCredentialHelper(cfg, keys); helper != "" {
		cred, err := runHelper(ctx, helper, server)
		if err != nil {
			return Credential{
				Source:        "credential-helper",
				Helper:        helper,
				ServerAddress: server,
				Message:       err.Error(),
			}
		}
		cred.Found = true
		cred.Source = "credential-helper"
		cred.Helper = helper
		if cred.ServerAddress == "" {
			cred.ServerAddress = server
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
	return Credential{Message: "no matching auths, credHelpers or credsStore entry"}
}

func FindCredentialHelper(cfg Config, keys []string) (string, string) {
	for _, key := range keys {
		if helper := strings.TrimSpace(cfg.CredHelpers[key]); helper != "" {
			return helper, key
		}
	}
	if helper := strings.TrimSpace(cfg.CredsStore); helper != "" && len(keys) > 0 {
		return helper, keys[0]
	}
	return "", ""
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
	cmd := exec.CommandContext(ctx, "docker-credential-"+helper, "get")
	cmd.Stdin = strings.NewReader(server)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return Credential{}, fmt.Errorf("docker-credential-%s get failed: %s", helper, msg)
	}
	var resp HelperResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return Credential{}, err
	}
	cred := Credential{
		Username:      resp.Username,
		Password:      resp.Secret,
		ServerAddress: resp.ServerURL,
	}
	if resp.Username == "<token>" {
		cred.Username = ""
		cred.Password = ""
		cred.IdentityToken = resp.Secret
	}
	return cred, nil
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
