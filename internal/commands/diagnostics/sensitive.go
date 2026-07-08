package diagnostics

import (
	"docker-manager/internal/sensitive"
)

const redactedValue = sensitive.RedactedValue

type sensitiveProfile = sensitive.Profile

func normalizeRedactProfile(profile string, redactSecrets bool) (sensitive.Profile, error) {
	return sensitive.NormalizeProfile(profile, redactSecrets)
}

func isSensitiveKey(key string) bool {
	return sensitive.IsSensitiveKey(key, sensitive.ProfileBasic)
}

func isSensitiveKeyWithProfile(key string, profile sensitive.Profile) bool {
	return sensitive.IsSensitiveKey(key, profile)
}

func redactEnvValue(env string) string {
	return sensitive.RedactEnvValue(env, sensitive.ProfileBasic)
}

func redactStringMap(values map[string]string) map[string]string {
	return sensitive.RedactStringMap(values, sensitive.ProfileBasic)
}

func redactStringMapWithProfile(values map[string]string, profile sensitive.Profile) map[string]string {
	return sensitive.RedactStringMap(values, profile)
}

func redactSensitiveText(text string) string {
	return sensitive.RedactText(text, sensitive.ProfileBasic)
}

func redactSensitiveTextWithProfile(text string, profile sensitive.Profile) string {
	return sensitive.RedactText(text, profile)
}

func redactStringSlice(items []string) []string {
	return sensitive.RedactStringSlice(items, sensitive.ProfileBasic)
}

func redactStringSliceWithProfile(items []string, profile sensitive.Profile) []string {
	return sensitive.RedactStringSlice(items, profile)
}
