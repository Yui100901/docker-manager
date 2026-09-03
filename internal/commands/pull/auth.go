package pull

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"docker-manager/internal/registryauth"
)

type pullRegistryAuth struct {
	Authorization string
}

type pullRegistryCredential = registryauth.Credential
type pullDockerConfigFile = registryauth.Config

type authChallenge struct {
	Scheme string
	Params map[string]string
}

const maxBearerServiceBytes = 512

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
	credentialDenied := registryCredentialsDenied(info.Registry, effectiveCredentialOperation(opts), opts)
	cred, credErr := r.loadPullRegistryCredential(ctx, info.Registry, opts)
	switch strings.ToLower(challenge.Scheme) {
	case "bearer":
		token, err := r.fetchBearerToken(ctx, challenge, info, cred, opts)
		if err != nil {
			if credentialDenied {
				return nil, fmt.Errorf("获取 Bearer token 失败: %w；%s", err, registryCredentialPolicyDescription(effectiveCredentialOperation(opts)))
			}
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
			if credentialDenied {
				return nil, fmt.Errorf("registry %s 需要 Basic 认证；%s", info.Registry, registryCredentialPolicyDescription(effectiveCredentialOperation(opts)))
			}
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
			if credentialDenied {
				return nil, fmt.Errorf("registry %s 返回 401 但没有 WWW-Authenticate challenge；%s", info.Registry, registryCredentialPolicyDescription(effectiveCredentialOperation(opts)))
			}
			return nil, fmt.Errorf("registry %s 返回 401 但没有 WWW-Authenticate challenge", info.Registry)
		}
		return nil, fmt.Errorf("不支持的 registry 认证方式 %q", challenge.Scheme)
	}
}

func effectiveCredentialOperation(opts PullOptions) string {
	if opts.credentialOperation != "" {
		return opts.credentialOperation
	}
	return registryCredentialPull
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

func (r *PullRunner) fetchBearerToken(ctx context.Context, challenge authChallenge, info *ImageInfo, cred pullRegistryCredential, opts PullOptions) (string, error) {
	realm := challenge.Params["realm"]
	if realm == "" {
		return "", fmt.Errorf("bearer challenge 缺少 realm")
	}
	realmURL, err := validateBearerRealm(realm, info, opts)
	if err != nil {
		return "", err
	}
	query, err := bearerTokenQuery(challenge, info)
	if err != nil {
		return "", err
	}
	headers := map[string]string{}
	if cred.IdentityToken != "" {
		headers["Authorization"] = "Bearer " + cred.IdentityToken
	} else if cred.Username != "" || cred.Password != "" {
		headers["Authorization"] = registryauth.BasicAuthHeader(cred.Username, cred.Password)
	}
	tokenClient := *r.httpClient.Client
	tokenClient.CheckRedirect = sameOriginRedirectPolicy
	tokenHTTPClient := *r.httpClient
	tokenHTTPClient.Client = &tokenClient
	tokenRunner := *r
	tokenRunner.httpClient = &tokenHTTPClient
	limits := effectivePullResourceLimits(opts.Limits)
	respBytes, err := tokenRunner.fetchWithRetryLimit(ctx, realmURL.String(), headers, query, limits.TokenBytes)
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

func bearerTokenQuery(challenge authChallenge, info *ImageInfo) (map[string]string, error) {
	query := map[string]string{
		// Scope describes this client's requested authority, so it must not be
		// widened or redirected by registry-provided challenge parameters.
		"scope": fmt.Sprintf("repository:%s:pull", imagePath(info)),
	}
	if service := challenge.Params["service"]; service != "" {
		// Service is the audience defined by the trusted realm issuer. Preserve
		// custom values used by Harbor and other registries, but bound the input.
		if err := validateBearerService(service); err != nil {
			return nil, err
		}
		query["service"] = service
	}
	return query, nil
}

func validateBearerService(service string) error {
	if !utf8.ValidString(service) {
		return fmt.Errorf("bearer challenge service 必须是有效 UTF-8")
	}
	if len(service) > maxBearerServiceBytes {
		return fmt.Errorf("bearer challenge service 超过 %d 字节", maxBearerServiceBytes)
	}
	if strings.IndexFunc(service, unicode.IsControl) >= 0 {
		return fmt.Errorf("bearer challenge service 不得包含控制字符")
	}
	return nil
}

func validateBearerRealm(rawRealm string, info *ImageInfo, opts PullOptions) (*url.URL, error) {
	realmURL, err := url.ParseRequestURI(strings.TrimSpace(rawRealm))
	if err != nil || realmURL.Scheme == "" || realmURL.Host == "" || realmURL.Hostname() == "" {
		return nil, fmt.Errorf("bearer realm 必须是绝对 HTTPS URL")
	}
	if !strings.EqualFold(realmURL.Scheme, "https") {
		return nil, fmt.Errorf("bearer realm 必须使用 HTTPS: %s", realmURL.Redacted())
	}
	if realmURL.User != nil || realmURL.Fragment != "" {
		return nil, fmt.Errorf("bearer realm 不得包含 userinfo 或 fragment")
	}

	registryURL, err := url.Parse(registryAPIURL(opts, info, "manifests", getReference(info)))
	if err != nil {
		return nil, fmt.Errorf("构造 registry origin 失败: %w", err)
	}
	if sameOriginURL(registryURL, realmURL) || isBuiltInDockerHubRealm(info.Registry, realmURL) {
		return realmURL, nil
	}
	for _, allowed := range opts.AuthRealmAllowlist {
		origin, err := normalizeAuthRealmOrigin(allowed)
		if err != nil {
			return nil, err
		}
		if origin == authRealmOrigin(realmURL) {
			return realmURL, nil
		}
	}
	return nil, fmt.Errorf("bearer realm origin %s 与 registry 不同源且不在 --auth-realm allowlist", authRealmOrigin(realmURL))
}

func validateAuthRealmAllowlist(values []string) error {
	for _, value := range values {
		if _, err := normalizeAuthRealmOrigin(value); err != nil {
			return err
		}
	}
	return nil
}

func normalizeAuthRealmOrigin(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("--auth-realm 不能为空")
	}
	rawURL := value
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("无效 --auth-realm %q: 应为 HTTPS origin 或 host[:port]", value)
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(parsed.Hostname(), "*") || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("无效 --auth-realm %q: 仅允许不含路径、查询、userinfo 的 HTTPS origin", value)
	}
	return authRealmOrigin(parsed), nil
}

func authRealmOrigin(value *url.URL) string {
	return "https://" + canonicalURLHost(value)
}

func isBuiltInDockerHubRealm(registryName string, realm *url.URL) bool {
	switch strings.ToLower(registryName) {
	case dockerHubDomain, defaultRegistry, "index.docker.io":
		return authRealmOrigin(realm) == "https://auth.docker.io:443"
	default:
		return false
	}
}

func (r *PullRunner) loadPullRegistryCredential(ctx context.Context, registryName string, opts PullOptions) (pullRegistryCredential, error) {
	if !registryCredentialsAllowed(registryName, opts) {
		return pullRegistryCredential{}, nil
	}
	configPath := opts.DockerConfig
	if configPath == "" {
		configPath = defaultPullDockerConfigPath()
	}
	cfg, err := readPullDockerConfig(configPath)
	if err != nil {
		return pullRegistryCredential{}, err
	}
	cred := registryauth.ResolveCredentialWithOptions(ctx, cfg, registryName, registryauth.ResolveOptions{
		DisableHelpers: opts.DisableCredentialHelpers,
		HelperTimeout:  opts.CredentialHelperTimeout,
		RunHelper:      r.runCredentialHelper,
	})
	if !cred.Found && cred.Source == "credential-helper" {
		return pullRegistryCredential{}, fmt.Errorf("docker credential helper %q failed: %s", cred.Helper, cred.Message)
	}
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

func defaultRunPullCredentialHelper(ctx context.Context, helper, server string) (pullRegistryCredential, error) {
	return registryauth.DefaultRunCredentialHelper(ctx, helper, server)
}

func basicAuthHeader(username, password string) string {
	return registryauth.BasicAuthHeader(username, password)
}
