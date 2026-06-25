package main

import (
	"regexp"
	"strings"
)

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

var sensitiveAssignmentPattern = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|secret|token|credential|authorization|auth|private_key|apikey|api_key)[a-z0-9_.-]*)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)
var authorizationHeaderPattern = regexp.MustCompile(`(?i)\b(authorization)(\s*:\s*)([^\r\n]+)`)
var urlCredentialPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/\s:@]+):([^@\s/]+)@`)

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

func redactSensitiveText(text string) string {
	text = authorizationHeaderPattern.ReplaceAllString(text, `${1}${2}`+redactedValue)
	text = sensitiveAssignmentPattern.ReplaceAllString(text, `${1}${2}`+redactedValue)
	return urlCredentialPattern.ReplaceAllString(text, `${1}:`+redactedValue+`@`)
}

func redactStringSlice(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	result := make([]string, len(items))
	for i, item := range items {
		result[i] = redactSensitiveText(item)
	}
	return result
}
