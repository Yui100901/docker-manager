package reverse

import "strings"

const redactedValue = "<redacted>"

var sensitiveKeyNeedles = []string{
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

func isSensitiveKey(key string) bool {
	key = strings.ToLower(key)
	for _, needle := range sensitiveKeyNeedles {
		if strings.Contains(key, needle) {
			return true
		}
	}
	return false
}

func redactEnvValue(env string) string {
	key, _, found := strings.Cut(env, "=")
	if !found || !isSensitiveKey(key) {
		return env
	}
	return key + "=" + redactedValue
}

func redactStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		if isSensitiveKey(key) {
			value = redactedValue
		}
		result[key] = value
	}
	return result
}
