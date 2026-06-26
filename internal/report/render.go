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

func RenderMarkdown(report interface{}) string {
	var sb strings.Builder
	title := reportTitle(report)
	sb.WriteString("# ")
	sb.WriteString(markdownEscape(title))
	sb.WriteString("\n\n")
	writeMarkdownValue(&sb, 2, "", reflect.ValueOf(report))
	return sb.String()
}

func RenderHTML(report interface{}) string {
	body := renderHTMLValue(2, "", reflect.ValueOf(report))
	title := reportTitle(report)
	return "<!doctype html>\n<html lang=\"zh-CN\">\n<head>\n<meta charset=\"utf-8\">\n" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n" +
		"<title>" + html.EscapeString(title) + "</title>\n" +
		"<style>body{font-family:-apple-system,BlinkMacSystemFont,\"Segoe UI\",sans-serif;line-height:1.5;margin:32px;color:#17202a}table{border-collapse:collapse;margin:12px 0 24px;width:100%}th,td{border:1px solid #d8dee4;padding:6px 8px;text-align:left;vertical-align:top}th{background:#f6f8fa}code{background:#f6f8fa;padding:1px 4px;border-radius:4px}pre{background:#f6f8fa;padding:12px;overflow:auto}h1,h2,h3,h4{line-height:1.25}.status-ok{color:#116329}.status-warning{color:#9a6700}.status-failed{color:#cf222e}.status-skipped{color:#57606a}</style>\n" +
		"</head>\n<body>\n<h1>" + html.EscapeString(title) + "</h1>\n" + body + "</body>\n</html>\n"
}

func writeMarkdownValue(sb *strings.Builder, level int, name string, value reflect.Value) {
	value = indirectValue(value)
	if !value.IsValid() {
		return
	}
	if name != "" {
		writeMarkdownHeading(sb, level, name)
	}
	switch value.Kind() {
	case reflect.Struct:
		writeMarkdownStruct(sb, level, value)
	case reflect.Slice, reflect.Array:
		writeMarkdownSlice(sb, level, value)
	case reflect.Map:
		writeMarkdownMap(sb, value)
	default:
		sb.WriteString(markdownEscape(formatValue(value)))
		sb.WriteString("\n\n")
	}
}

func writeMarkdownStruct(sb *strings.Builder, level int, value reflect.Value) {
	fields := exportedRenderFields(value)
	var simple []renderField
	var complex []renderField
	for _, field := range fields {
		v := indirectValue(field.Value)
		if !v.IsValid() {
			continue
		}
		if isSimpleValue(v) {
			simple = append(simple, renderField{Name: field.Name, Value: v})
		} else {
			complex = append(complex, renderField{Name: field.Name, Value: v})
		}
	}
	if len(simple) > 0 {
		sb.WriteString("| 字段 | 值 |\n| --- | --- |\n")
		for _, field := range simple {
			sb.WriteString("| ")
			sb.WriteString(markdownEscape(field.Name))
			sb.WriteString(" | ")
			sb.WriteString(markdownEscape(formatValue(field.Value)))
			sb.WriteString(" |\n")
		}
		sb.WriteString("\n")
	}
	for _, field := range complex {
		writeMarkdownValue(sb, level, field.Name, field.Value)
	}
}

func writeMarkdownSlice(sb *strings.Builder, level int, value reflect.Value) {
	if value.Len() == 0 {
		sb.WriteString("_无数据_\n\n")
		return
	}
	if canRenderStructTable(value) {
		headers := simpleStructHeaders(indirectValue(value.Index(0)))
		sb.WriteString("|")
		for _, header := range headers {
			sb.WriteString(" ")
			sb.WriteString(markdownEscape(header))
			sb.WriteString(" |")
		}
		sb.WriteString("\n|")
		for range headers {
			sb.WriteString(" --- |")
		}
		sb.WriteString("\n")
		for i := 0; i < value.Len(); i++ {
			row := simpleStructValues(indirectValue(value.Index(i)), headers)
			sb.WriteString("|")
			for _, cell := range row {
				sb.WriteString(" ")
				sb.WriteString(markdownEscape(cell))
				sb.WriteString(" |")
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
		return
	}
	for i := 0; i < value.Len(); i++ {
		item := indirectValue(value.Index(i))
		if isSimpleValue(item) {
			sb.WriteString("- ")
			sb.WriteString(markdownEscape(formatValue(item)))
			sb.WriteString("\n")
			continue
		}
		writeMarkdownValue(sb, level+1, fmt.Sprintf("项目 %d", i+1), item)
	}
	sb.WriteString("\n")
}

func writeMarkdownMap(sb *strings.Builder, value reflect.Value) {
	if value.Len() == 0 {
		sb.WriteString("_无数据_\n\n")
		return
	}
	keys := sortedMapKeys(value)
	sb.WriteString("| 键 | 值 |\n| --- | --- |\n")
	for _, key := range keys {
		v := indirectValue(value.MapIndex(key))
		sb.WriteString("| ")
		sb.WriteString(markdownEscape(formatValue(key)))
		sb.WriteString(" | ")
		if isSimpleValue(v) {
			sb.WriteString(markdownEscape(formatValue(v)))
		} else {
			sb.WriteString(markdownEscape(compactJSON(v.Interface())))
		}
		sb.WriteString(" |\n")
	}
	sb.WriteString("\n")
}

func writeMarkdownHeading(sb *strings.Builder, level int, name string) {
	if level < 2 {
		level = 2
	}
	if level > 6 {
		level = 6
	}
	sb.WriteString(strings.Repeat("#", level))
	sb.WriteByte(' ')
	sb.WriteString(markdownEscape(humanizeName(name)))
	sb.WriteString("\n\n")
}

func renderHTMLValue(level int, name string, value reflect.Value) string {
	value = indirectValue(value)
	if !value.IsValid() {
		return ""
	}
	var sb strings.Builder
	if name != "" {
		writeHTMLHeading(&sb, level, name)
	}
	switch value.Kind() {
	case reflect.Struct:
		sb.WriteString(renderHTMLStruct(level, value))
	case reflect.Slice, reflect.Array:
		sb.WriteString(renderHTMLSlice(level, value))
	case reflect.Map:
		sb.WriteString(renderHTMLMap(value))
	default:
		sb.WriteString("<p>")
		sb.WriteString(html.EscapeString(formatValue(value)))
		sb.WriteString("</p>\n")
	}
	return sb.String()
}

func renderHTMLStruct(level int, value reflect.Value) string {
	fields := exportedRenderFields(value)
	var simple []renderField
	var complex []renderField
	for _, field := range fields {
		v := indirectValue(field.Value)
		if !v.IsValid() {
			continue
		}
		if isSimpleValue(v) {
			simple = append(simple, renderField{Name: field.Name, Value: v})
		} else {
			complex = append(complex, renderField{Name: field.Name, Value: v})
		}
	}
	var sb strings.Builder
	if len(simple) > 0 {
		sb.WriteString("<table><thead><tr><th>字段</th><th>值</th></tr></thead><tbody>\n")
		for _, field := range simple {
			sb.WriteString("<tr><td>")
			sb.WriteString(html.EscapeString(humanizeName(field.Name)))
			sb.WriteString("</td><td>")
			sb.WriteString(htmlValue(field.Value))
			sb.WriteString("</td></tr>\n")
		}
		sb.WriteString("</tbody></table>\n")
	}
	for _, field := range complex {
		sb.WriteString(renderHTMLValue(level, field.Name, field.Value))
	}
	return sb.String()
}

func renderHTMLSlice(level int, value reflect.Value) string {
	if value.Len() == 0 {
		return "<p><em>无数据</em></p>\n"
	}
	var sb strings.Builder
	if canRenderStructTable(value) {
		headers := simpleStructHeaders(indirectValue(value.Index(0)))
		sb.WriteString("<table><thead><tr>")
		for _, header := range headers {
			sb.WriteString("<th>")
			sb.WriteString(html.EscapeString(humanizeName(header)))
			sb.WriteString("</th>")
		}
		sb.WriteString("</tr></thead><tbody>\n")
		for i := 0; i < value.Len(); i++ {
			row := simpleStructValues(indirectValue(value.Index(i)), headers)
			sb.WriteString("<tr>")
			for _, cell := range row {
				sb.WriteString("<td>")
				sb.WriteString(html.EscapeString(cell))
				sb.WriteString("</td>")
			}
			sb.WriteString("</tr>\n")
		}
		sb.WriteString("</tbody></table>\n")
		return sb.String()
	}
	sb.WriteString("<ul>\n")
	for i := 0; i < value.Len(); i++ {
		item := indirectValue(value.Index(i))
		if isSimpleValue(item) {
			sb.WriteString("<li>")
			sb.WriteString(html.EscapeString(formatValue(item)))
			sb.WriteString("</li>\n")
			continue
		}
		sb.WriteString("<li>")
		sb.WriteString(renderHTMLValue(level+1, fmt.Sprintf("项目 %d", i+1), item))
		sb.WriteString("</li>\n")
	}
	sb.WriteString("</ul>\n")
	return sb.String()
}

func renderHTMLMap(value reflect.Value) string {
	if value.Len() == 0 {
		return "<p><em>无数据</em></p>\n"
	}
	var sb strings.Builder
	sb.WriteString("<table><thead><tr><th>键</th><th>值</th></tr></thead><tbody>\n")
	for _, key := range sortedMapKeys(value) {
		v := indirectValue(value.MapIndex(key))
		sb.WriteString("<tr><td>")
		sb.WriteString(html.EscapeString(formatValue(key)))
		sb.WriteString("</td><td>")
		if isSimpleValue(v) {
			sb.WriteString(htmlValue(v))
		} else {
			sb.WriteString("<code>")
			sb.WriteString(html.EscapeString(compactJSON(v.Interface())))
			sb.WriteString("</code>")
		}
		sb.WriteString("</td></tr>\n")
	}
	sb.WriteString("</tbody></table>\n")
	return sb.String()
}

func writeHTMLHeading(sb *strings.Builder, level int, name string) {
	if level < 2 {
		level = 2
	}
	if level > 6 {
		level = 6
	}
	tag := fmt.Sprintf("h%d", level)
	sb.WriteString("<")
	sb.WriteString(tag)
	sb.WriteString(">")
	sb.WriteString(html.EscapeString(humanizeName(name)))
	sb.WriteString("</")
	sb.WriteString(tag)
	sb.WriteString(">\n")
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
