package runconfig

import "docker-manager/internal/sensitive"

const redactedValue = sensitive.RedactedValue

func normalizeRedactProfile(profile string, redactSecrets bool) (sensitive.Profile, error) {
	return sensitive.NormalizeProfile(profile, redactSecrets)
}

func isSensitiveKey(key string) bool {
	return sensitive.IsSensitiveKey(key, sensitive.ProfileBasic)
}

func redactEnvValue(env string) string {
	return sensitive.RedactEnvValue(env, sensitive.ProfileBasic)
}

func redactEnvValueWithProfile(env string, profile sensitive.Profile) string {
	return sensitive.RedactEnvValue(env, profile)
}

func redactStringMap(values map[string]string) map[string]string {
	return sensitive.RedactStringMap(values, sensitive.ProfileBasic)
}

func redactStringMapWithProfile(values map[string]string, profile sensitive.Profile) map[string]string {
	return sensitive.RedactStringMap(values, profile)
}
