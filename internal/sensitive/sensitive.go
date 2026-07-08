package sensitive

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

const RedactedValue = "<redacted>"

type Profile string

const (
	ProfileNone   Profile = "none"
	ProfileBasic  Profile = "basic"
	ProfileStrict Profile = "strict"
)

var basicKeyNeedles = []string{
	"password",
	"passwd",
	"secret",
	"token",
	"credential",
	"authorization",
	"auth",
	"private_key",
	"apikey",
	"api_key",
}

var strictKeyNeedles = []string{
	"accesskey",
	"access_key",
	"secretkey",
	"secret_key",
	"clientsecret",
	"client_secret",
	"refresh_token",
	"id_token",
	"session",
	"cookie",
	"jwt",
	"oauth",
	"bearer",
	"passphrase",
	"registry_auth",
	"docker_auth",
}

var strictKeyTokens = map[string]bool{
	"cert":        true,
	"certificate": true,
	"key":         true,
	"keystore":    true,
	"truststore":  true,
}

var sensitiveAssignmentPattern = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|secret|token|credential|authorization|auth|private_key|apikey|api_key|access[_-]?key|secret[_-]?key|client[_-]?secret|refresh[_-]?token|id[_-]?token|session|cookie|jwt|oauth|bearer|passphrase)[a-z0-9_.-]*)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)
var sensitiveJSONFieldPattern = regexp.MustCompile(`(?i)("?[a-z0-9_.-]*(?:password|passwd|secret|token|credential|authorization|auth|private_key|apikey|api_key|access[_-]?key|secret[_-]?key|client[_-]?secret|refresh[_-]?token|id[_-]?token|session|cookie|jwt|oauth|bearer|passphrase)"?\s*:\s*)("[^"]*"|[^\s,;}]+)`)
var authorizationHeaderPattern = regexp.MustCompile(`(?i)\b(authorization)(\s*:\s*)([^\r\n]+)`)
var cookieHeaderPattern = regexp.MustCompile(`(?i)\b(cookie|set-cookie)(\s*:\s*)([^\r\n]+)`)
var urlCredentialPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/\s:@]+):([^@\s/]+)@`)
var sensitiveQueryPattern = regexp.MustCompile(`(?i)([?&](?:password|passwd|secret|token|access[_-]?key|secret[_-]?key|client[_-]?secret|api[_-]?key|apikey|auth|session|jwt)=)([^&#\s]+)`)
var bearerTokenPattern = regexp.MustCompile(`(?i)\b(bearer\s+)([a-z0-9._~+/\-]+=*)`)
var jwtPattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
var privateKeyBlockPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

func NormalizeProfile(value string, redactSecrets bool) (Profile, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		if redactSecrets {
			return ProfileBasic, nil
		}
		return ProfileNone, nil
	}
	switch Profile(value) {
	case ProfileNone, ProfileBasic, ProfileStrict:
		return Profile(value), nil
	default:
		return "", fmt.Errorf("unsupported redact profile %q, use none, basic or strict", value)
	}
}

func IsSensitiveKey(key string, profile Profile) bool {
	switch profile {
	case ProfileNone, "":
		return false
	case ProfileBasic:
		return containsAnyFold(key, basicKeyNeedles)
	case ProfileStrict:
		return containsAnyFold(key, basicKeyNeedles) || containsAnyFold(key, strictKeyNeedles) || containsStrictToken(key)
	default:
		return containsAnyFold(key, basicKeyNeedles)
	}
}

func RedactEnvValue(env string, profile Profile) string {
	key, _, found := strings.Cut(env, "=")
	if !found || !IsSensitiveKey(key, profile) {
		if profile == ProfileStrict {
			return RedactText(env, profile)
		}
		return env
	}
	return key + "=" + RedactedValue
}

func RedactStringMap(values map[string]string, profile Profile) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		if IsSensitiveKey(key, profile) {
			value = RedactedValue
		} else {
			value = RedactText(value, profile)
		}
		result[key] = value
	}
	return result
}

func RedactText(text string, profile Profile) string {
	switch profile {
	case ProfileNone, "":
		return text
	case ProfileBasic, ProfileStrict:
		text = authorizationHeaderPattern.ReplaceAllString(text, `${1}${2}`+RedactedValue)
		text = sensitiveJSONFieldPattern.ReplaceAllString(text, `${1}"`+RedactedValue+`"`)
		text = sensitiveAssignmentPattern.ReplaceAllString(text, `${1}${2}`+RedactedValue)
		text = urlCredentialPattern.ReplaceAllString(text, `${1}:`+RedactedValue+`@`)
		text = sensitiveQueryPattern.ReplaceAllString(text, `${1}`+RedactedValue)
		if profile == ProfileStrict {
			text = cookieHeaderPattern.ReplaceAllString(text, `${1}${2}`+RedactedValue)
			text = bearerTokenPattern.ReplaceAllString(text, `${1}`+RedactedValue)
			text = jwtPattern.ReplaceAllString(text, RedactedValue)
			text = privateKeyBlockPattern.ReplaceAllString(text, RedactedValue)
		}
		return text
	default:
		return RedactText(text, ProfileBasic)
	}
}

func RedactStringSlice(items []string, profile Profile) []string {
	if len(items) == 0 {
		return nil
	}
	result := make([]string, len(items))
	for i, item := range items {
		result[i] = RedactText(item, profile)
	}
	return result
}

func containsAnyFold(value string, needles []string) bool {
	value = strings.ToLower(value)
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func containsStrictToken(value string) bool {
	for _, token := range splitKeyTokens(value) {
		if strictKeyTokens[token] {
			return true
		}
	}
	return false
}

func splitKeyTokens(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
