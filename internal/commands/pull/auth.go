package pull

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"docker-manager/internal/registryauth"
)

type pullRegistryAuth struct {
	Authorization string
}

type pullRegistryCredential = registryauth.Credential
type pullDockerConfigFile = registryauth.Config
type pullDockerAuthEntry = registryauth.AuthEntry

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
		return &pullRegistryAuth{Authorization: registryauth.BasicAuthHeader(cred.Username, cred.Password)}, nil
	default:
		if credErr == nil {
			if cred.IdentityToken != "" {
				return &pullRegistryAuth{Authorization: "Bearer " + cred.IdentityToken}, nil
			}
			if cred.Username != "" || cred.Password != "" {
				return &pullRegistryAuth{Authorization: registryauth.BasicAuthHeader(cred.Username, cred.Password)}, nil
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
		headers["Authorization"] = registryauth.BasicAuthHeader(cred.Username, cred.Password)
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
	cred := registryauth.ResolveCredential(ctx, cfg, registryName, r.runCredentialHelper)
	if !cred.Found && cred.Source == "" {
		return pullRegistryCredential{}, nil
	}
	return cred, nil
}

func defaultPullDockerConfigPath() string {
	return registryauth.DefaultConfigPath()
}

func readPullDockerConfig(path string) (pullDockerConfigFile, error) {
	cfg, _, err := registryauth.ReadConfig(path)
	return cfg, err
}

func findPullCredentialHelper(cfg pullDockerConfigFile, keys []string) (string, string) {
	return registryauth.FindCredentialHelper(cfg, keys)
}

func pullRegistryConfigKeys(registryName string) []string {
	return registryauth.ConfigKeys(registryName)
}

func pullCredentialFromAuthEntry(entry pullDockerAuthEntry) pullRegistryCredential {
	return registryauth.CredentialFromAuthEntry(entry)
}

func defaultRunPullCredentialHelper(ctx context.Context, helper, server string) (pullRegistryCredential, error) {
	return registryauth.DefaultRunCredentialHelper(ctx, helper, server)
}

func basicAuthHeader(username, password string) string {
	return registryauth.BasicAuthHeader(username, password)
}

func uniquePullStrings(values []string) []string {
	return registryauth.UniqueStrings(values)
}
