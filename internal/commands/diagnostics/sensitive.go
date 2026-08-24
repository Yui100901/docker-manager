package diagnostics

import (
	"strings"

	"docker-manager/internal/sensitive"
)

const redactedValue = sensitive.RedactedValue

type sensitiveProfile = sensitive.Profile

func normalizeRedactProfile(profile string, redactSecrets bool) (sensitive.Profile, error) {
	if strings.TrimSpace(profile) == "" && !redactSecrets {
		return sensitive.DefaultProfile(), nil
	}
	return sensitive.NormalizeProfile(profile, redactSecrets)
}

func isSensitiveKeyWithProfile(key string, profile sensitive.Profile) bool {
	return sensitive.IsSensitiveKey(key, profile)
}

func redactStringMapWithProfile(values map[string]string, profile sensitive.Profile) map[string]string {
	return sensitive.RedactStringMap(values, profile)
}

func redactSensitiveTextWithProfile(text string, profile sensitive.Profile) string {
	return sensitive.RedactText(text, profile)
}

func redactStringSliceWithProfile(items []string, profile sensitive.Profile) []string {
	return sensitive.RedactStringSlice(items, profile)
}
