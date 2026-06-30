package report

import (
	"encoding/json"
	"fmt"
	"html"
	"reflect"
	"sort"
	"strings"
	"time"
)

type renderField struct {
	Name  string
	Value reflect.Value
}

func exportedRenderFields(value reflect.Value) []renderField {
	value = indirectValue(value)
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return nil
	}
	valueType := value.Type()
	var fields []renderField
	for i := 0; i < value.NumField(); i++ {
		field := valueType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, omitEmpty, skip := jsonFieldName(field)
		if skip {
			continue
		}
		fieldValue := value.Field(i)
		if omitEmpty && isEmptyValue(fieldValue) {
			continue
		}
		fields = append(fields, renderField{Name: name, Value: fieldValue})
	}
	return fields
}

func jsonFieldName(field reflect.StructField) (string, bool, bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name := strings.TrimSpace(parts[0])
	if name == "" {
		name = field.Name
	}
	omitEmpty := false
	for _, part := range parts[1:] {
		if part == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}

func canRenderStructTable(value reflect.Value) bool {
	if value.Len() == 0 {
		return false
	}
	first := indirectValue(value.Index(0))
	if !first.IsValid() || first.Kind() != reflect.Struct || len(simpleStructHeaders(first)) == 0 {
		return false
	}
	for i := 0; i < value.Len(); i++ {
		item := indirectValue(value.Index(i))
		if !item.IsValid() || item.Kind() != reflect.Struct {
			return false
		}
		for _, field := range exportedRenderFields(item) {
			if !isSimpleValue(indirectValue(field.Value)) {
				return false
			}
		}
	}
	return true
}

func simpleStructHeaders(value reflect.Value) []string {
	var headers []string
	for _, field := range exportedRenderFields(value) {
		if isSimpleValue(indirectValue(field.Value)) {
			headers = append(headers, field.Name)
		}
	}
	return headers
}

func simpleStructValues(value reflect.Value, headers []string) []string {
	valuesByName := map[string]string{}
	for _, field := range exportedRenderFields(value) {
		if isSimpleValue(indirectValue(field.Value)) {
			valuesByName[field.Name] = formatValue(field.Value)
		}
	}
	var values []string
	for _, header := range headers {
		values = append(values, valuesByName[header])
	}
	return values
}

func isSimpleValue(value reflect.Value) bool {
	value = indirectValue(value)
	if !value.IsValid() {
		return true
	}
	if value.Type() == reflect.TypeOf(time.Time{}) {
		return true
	}
	switch value.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.String:
		return true
	default:
		return false
	}
}

func isEmptyValue(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return value.Len() == 0
	case reflect.Bool:
		return !value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return value.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return value.IsNil()
	default:
		return false
	}
}

func indirectValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

func formatValue(value reflect.Value) string {
	value = indirectValue(value)
	if !value.IsValid() {
		return ""
	}
	if value.Type() == reflect.TypeOf(time.Time{}) {
		t := value.Interface().(time.Time)
		if t.IsZero() {
			return ""
		}
		return t.Format(time.RFC3339)
	}
	if value.CanInterface() {
		return fmt.Sprint(value.Interface())
	}
	return fmt.Sprint(value)
}

func htmlValue(value reflect.Value) string {
	text := html.EscapeString(formatValue(value))
	status := strings.ToLower(formatValue(value))
	switch status {
	case "ok", "warning", "failed", "skipped":
		return "<span class=\"status-" + status + "\">" + text + "</span>"
	default:
		return text
	}
}

func markdownEscape(value string) string {
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	return value
}

func sortedMapKeys(value reflect.Value) []reflect.Value {
	keys := value.MapKeys()
	sort.Slice(keys, func(i, j int) bool {
		return formatValue(keys[i]) < formatValue(keys[j])
	})
	return keys
}

func compactJSON(value interface{}) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

func reportTitle(report interface{}) string {
	value := indirectValue(reflect.ValueOf(report))
	if value.IsValid() && value.Kind() == reflect.Struct {
		return humanizeName(value.Type().Name())
	}
	return "Report"
}

func humanizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return name
	}
	name = strings.ReplaceAll(name, "_", " ")
	var words []string
	var current strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' && current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	if len(words) == 0 {
		return name
	}
	result := strings.Join(words, " ")
	if result == "" {
		return result
	}
	return strings.ToUpper(result[:1]) + result[1:]
}
