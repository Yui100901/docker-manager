package pull

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

type pullRegistryAuth struct {
	Authorization string
}

type pullRegistryCredential struct {
	Found         bool
	Username      string
	Password      string
	IdentityToken string
	Source        string
	Message       string
}

type pullDockerConfigFile struct {
	Auths       map[string]pullDockerAuthEntry `json:"auths"`
	CredsStore  string                         `json:"credsStore"`
	CredHelpers map[string]string              `json:"credHelpers"`
}

type pullDockerAuthEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

type pullCredentialHelperResponse struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

type authChallenge struct {
	Scheme string
	Params map[string]string
}

func authHeaders(headers map[string]string, auth *pullRegistryAuth) map[string]string {
	result := map[string]string{}
	for key, value := range headers {
		result[key] = value
	}
	if auth != nil && auth.Authorization != "" {
		result["Authorization"] = auth.Authorization
	}
	return result
}

// Registry authentication follows Docker's challenge flow: parse
// WWW-Authenticate, load Docker credentials if present, then exchange them for
// the Authorization header required by the next registry request.
func (r *PullRunner) resolveRegistryAuth(ctx context.Context, header string, info *ImageInfo, opts PullOptions) (*pullRegistryAuth, error) {
	challenge := parseAuthChallenge(header)
	cred, credErr := r.loadPullRegistryCredential(ctx, info.Registry, opts.DockerConfig)
	switch strings.ToLower(challenge.Scheme) {
	case "bearer":
		token, err := r.fetchBearerToken(ctx, challenge, info, cred)
		if err != nil {
			if credErr != nil {
				return nil, fmt.Errorf("获取 Bearer token 失败: %w；读取 Docker 凭据也失败: %v", err, credErr)
			}
			return nil, err
		}
		return &pullRegistryAuth{Authorization: "Bearer " + token}, nil
	case "basic":
		if credErr != nil {
			return nil, credErr
		}
		if cred.Username == "" && cred.Password == "" {
			return nil, fmt.Errorf("registry %s 需要 Basic 认证，但未找到 Docker 凭据", info.Registry)
		}
		return &pullRegistryAuth{Authorization: basicAuthHeader(cred.Username, cred.Password)}, nil
	default:
		if credErr == nil {
			if cred.IdentityToken != "" {
				return &pullRegistryAuth{Authorization: "Bearer " + cred.IdentityToken}, nil
			}
			if cred.Username != "" || cred.Password != "" {
				return &pullRegistryAuth{Authorization: basicAuthHeader(cred.Username, cred.Password)}, nil
			}
		}
		if strings.TrimSpace(header) == "" {
			return nil, fmt.Errorf("registry %s 返回 401 但没有 WWW-Authenticate challenge", info.Registry)
		}
		return nil, fmt.Errorf("不支持的 registry 认证方式 %q", challenge.Scheme)
	}
}

func parseAuthChallenge(header string) authChallenge {
	header = strings.TrimSpace(header)
	if header == "" {
		return authChallenge{Params: map[string]string{}}
	}
	scheme, rest, _ := strings.Cut(header, " ")
	return authChallenge{
		Scheme: strings.TrimSpace(scheme),
		Params: parseChallengeParams(rest),
	}
}

func parseChallengeParams(input string) map[string]string {
	params := map[string]string{}
	for len(input) > 0 {
		input = strings.TrimLeft(input, " ,")
		if input == "" {
			break
		}
		key, rest, ok := strings.Cut(input, "=")
		if !ok {
			break
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimLeft(rest, " ")
		var value string
		if strings.HasPrefix(rest, "\"") {
			value, rest = readQuotedChallengeValue(rest[1:])
		} else {
			value, rest, _ = strings.Cut(rest, ",")
		}
		if key != "" {
			params[strings.ToLower(key)] = value
		}
		input = rest
	}
	return params
}

func readQuotedChallengeValue(input string) (string, string) {
	var sb strings.Builder
	escaped := false
	for i, r := range input {
		if escaped {
			sb.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			return sb.String(), input[i+1:]
		}
		sb.WriteRune(r)
	}
	return sb.String(), ""
}

func (r *PullRunner) fetchBearerToken(ctx context.Context, challenge authChallenge, info *ImageInfo, cred pullRegistryCredential) (string, error) {
	realm := challenge.Params["realm"]
	if realm == "" {
		return "", fmt.Errorf("Bearer challenge 缺少 realm")
	}
	query := map[string]string{}
	if service := challenge.Params["service"]; service != "" {
		query["service"] = service
	}
	scope := challenge.Params["scope"]
	if scope == "" {
		scope = fmt.Sprintf("repository:%s:pull", imagePath(info))
	}
	query["scope"] = scope
	headers := map[string]string{}
	if cred.IdentityToken != "" {
		headers["Authorization"] = "Bearer " + cred.IdentityToken
	} else if cred.Username != "" || cred.Password != "" {
		headers["Authorization"] = basicAuthHeader(cred.Username, cred.Password)
	}
	respBytes, err := r.fetchWithRetry(ctx, realm, headers, query)
	if err != nil {
		return "", fmt.Errorf("认证请求失败: %w", err)
	}
	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(respBytes, &token); err != nil {
		return "", fmt.Errorf("解析 token 失败: %w", err)
	}
	if token.Token != "" {
		return token.Token, nil
	}
	if token.AccessToken != "" {
		return token.AccessToken, nil
	}
	return "", fmt.Errorf("认证响应不包含 token")
}

func (r *PullRunner) loadPullRegistryCredential(ctx context.Context, registryName, configPath string) (pullRegistryCredential, error) {
	if configPath == "" {
		configPath = defaultPullDockerConfigPath()
	}
	cfg, err := readPullDockerConfig(configPath)
	if err != nil {
		return pullRegistryCredential{}, err
	}
	keys := pullRegistryConfigKeys(registryName)
	if helper, server := findPullCredentialHelper(cfg, keys); helper != "" {
		cred, err := r.runCredentialHelper(ctx, helper, server)
		if err != nil {
			return pullRegistryCredential{Source: "credential-helper", Message: err.Error()}, err
		}
		cred.Found = true
		cred.Source = "credential-helper"
		return cred, nil
	}
	for _, key := range keys {
		entry, ok := cfg.Auths[key]
		if !ok {
			continue
		}
		cred := pullCredentialFromAuthEntry(entry)
		cred.Found = cred.Username != "" || cred.Password != "" || cred.IdentityToken != ""
		cred.Source = "auths"
		return cred, nil
	}
	return pullRegistryCredential{}, nil
}

func defaultPullDockerConfigPath() string {
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".docker", "config.json")
	}
	return filepath.Join(home, ".docker", "config.json")
}

func readPullDockerConfig(path string) (pullDockerConfigFile, error) {
	var cfg pullDockerConfigFile
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func findPullCredentialHelper(cfg pullDockerConfigFile, keys []string) (string, string) {
	for _, key := range keys {
		if helper := strings.TrimSpace(cfg.CredHelpers[key]); helper != "" {
			return helper, key
		}
	}
	if helper := strings.TrimSpace(cfg.CredsStore); helper != "" {
		return helper, keys[0]
	}
	return "", ""
}

func pullRegistryConfigKeys(registryName string) []string {
	keys := []string{
		registryName,
		"https://" + registryName,
		"http://" + registryName,
		"https://" + registryName + "/v1/",
	}
	if registryName == "docker.io" || registryName == "registry-1.docker.io" || registryName == "index.docker.io" {
		keys = append(keys, "https://index.docker.io/v1/", "index.docker.io", "docker.io", "registry-1.docker.io")
	}
	return uniquePullStrings(keys)
}

func pullCredentialFromAuthEntry(entry pullDockerAuthEntry) pullRegistryCredential {
	cred := pullRegistryCredential{
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

func defaultRunPullCredentialHelper(ctx context.Context, helper, server string) (pullRegistryCredential, error) {
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
		return pullRegistryCredential{}, fmt.Errorf("docker-credential-%s get failed: %s", helper, msg)
	}
	var resp pullCredentialHelperResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return pullRegistryCredential{}, err
	}
	cred := pullRegistryCredential{Username: resp.Username, Password: resp.Secret}
	if resp.Username == "<token>" {
		cred.Username = ""
		cred.Password = ""
		cred.IdentityToken = resp.Secret
	}
	return cred, nil
}

func basicAuthHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func uniquePullStrings(values []string) []string {
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
