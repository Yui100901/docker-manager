package sensitive

import (
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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
var authSchemeTokenPattern = regexp.MustCompile(`(?i)\b((?:basic|bearer)\s+)([a-z0-9._~+/\-]+=*)`)
var jwtPattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
var privateKeyBlockPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

var defaultProfile atomic.Uint32

const (
	profileNoneValue uint32 = iota
	profileBasicValue
	profileStrictValue
)

// SetDefaultProfile changes the process-wide output policy. The CLI executes
// one command per process; atomic storage also keeps package tests and report
// rendering race-free.
func SetDefaultProfile(profile Profile) {
	switch profile {
	case ProfileBasic:
		defaultProfile.Store(profileBasicValue)
	case ProfileStrict:
		defaultProfile.Store(profileStrictValue)
	default:
		defaultProfile.Store(profileNoneValue)
	}
}

func DefaultProfile() Profile {
	switch defaultProfile.Load() {
	case profileBasicValue:
		return ProfileBasic
	case profileStrictValue:
		return ProfileStrict
	default:
		return ProfileNone
	}
}

type dynamicWriter struct {
	out io.Writer
	mu  sync.Mutex
}

// NewDynamicWriter redacts each write using the profile active at write time.
// This lets persistent CLI logging be configured before command flags are
// resolved without buffering sensitive log output.
func NewDynamicWriter(out io.Writer) io.Writer {
	if out == nil {
		out = io.Discard
	}
	return &dynamicWriter{out: out}
}

func (w *dynamicWriter) Write(p []byte) (int, error) {
	redacted := []byte(RedactText(string(p), DefaultProfile()))
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.out.Write(redacted)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

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
		text = authSchemeTokenPattern.ReplaceAllString(text, `${1}`+RedactedValue)
		if profile == ProfileStrict {
			text = cookieHeaderPattern.ReplaceAllString(text, `${1}${2}`+RedactedValue)
			text = jwtPattern.ReplaceAllString(text, RedactedValue)
			text = privateKeyBlockPattern.ReplaceAllString(text, RedactedValue)
		}
		return text
	default:
		return RedactText(text, ProfileBasic)
	}
}

// RedactValue returns a deep copy with string values redacted according to
// their field/map key and content. The concrete report type is preserved so
// Markdown and HTML rendering retain their normal titles and table layouts.
func RedactValue(value interface{}, profile Profile) interface{} {
	if value == nil || profile == ProfileNone || profile == "" {
		return value
	}
	return redactReflectValue(reflect.ValueOf(value), "", profile).Interface()
}

func redactReflectValue(value reflect.Value, key string, profile Profile) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		item := redactReflectValue(value.Elem(), key, profile)
		result := reflect.New(value.Type()).Elem()
		result.Set(item)
		return result
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.New(value.Type().Elem())
		result.Elem().Set(redactReflectValue(value.Elem(), key, profile))
		return result
	case reflect.Struct:
		result := reflect.New(value.Type()).Elem()
		result.Set(value)
		typeInfo := value.Type()
		for i := 0; i < value.NumField(); i++ {
			fieldInfo := typeInfo.Field(i)
			if !fieldInfo.IsExported() || !result.Field(i).CanSet() {
				continue
			}
			fieldKey := fieldInfo.Name
			if jsonName := strings.Split(fieldInfo.Tag.Get("json"), ",")[0]; jsonName != "" && jsonName != "-" {
				fieldKey = jsonName
			}
			result.Field(i).Set(redactReflectValue(value.Field(i), fieldKey, profile))
		}
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			mapKey := iterator.Key()
			itemKey := key
			if mapKey.Kind() == reflect.String {
				itemKey = mapKey.String()
			}
			result.SetMapIndex(mapKey, redactReflectValue(iterator.Value(), itemKey, profile))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(redactReflectValue(value.Index(i), key, profile))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(redactReflectValue(value.Index(i), key, profile))
		}
		return result
	case reflect.String:
		text := value.String()
		if IsSensitiveKey(key, profile) {
			text = RedactedValue
		} else {
			text = RedactText(text, profile)
		}
		result := reflect.New(value.Type()).Elem()
		result.SetString(text)
		return result
	default:
		return value
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
