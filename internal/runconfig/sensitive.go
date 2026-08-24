package runconfig

import (
	"strings"

	"docker-manager/internal/sensitive"
)

func normalizeRedactProfile(profile string, redactSecrets bool) (sensitive.Profile, error) {
	if strings.TrimSpace(profile) == "" && !redactSecrets {
		return sensitive.DefaultProfile(), nil
	}
	return sensitive.NormalizeProfile(profile, redactSecrets)
}

func redactEnvValueWithProfile(env string, profile sensitive.Profile) string {
	return sensitive.RedactEnvValue(env, profile)
}

func redactStringMapWithProfile(values map[string]string, profile sensitive.Profile) map[string]string {
	return sensitive.RedactStringMap(values, profile)
}
