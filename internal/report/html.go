package report

import (
	"fmt"
	"html"
	"reflect"
	"strings"
)

func RenderHTML(report interface{}) string {
	body := renderHTMLValue(2, "", reflect.ValueOf(report))
	title := reportTitle(report)
	return "<!doctype html>\n<html lang=\"zh-CN\">\n<head>\n<meta charset=\"utf-8\">\n" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n" +
		"<title>" + html.EscapeString(title) + "</title>\n" +
		"<style>body{font-family:-apple-system,BlinkMacSystemFont,\"Segoe UI\",sans-serif;line-height:1.5;margin:32px;color:#17202a}table{border-collapse:collapse;margin:12px 0 24px;width:100%}th,td{border:1px solid #d8dee4;padding:6px 8px;text-align:left;vertical-align:top}th{background:#f6f8fa}code{background:#f6f8fa;padding:1px 4px;border-radius:4px}pre{background:#f6f8fa;padding:12px;overflow:auto}h1,h2,h3,h4{line-height:1.25}.status-ok{color:#116329}.status-warning{color:#9a6700}.status-failed{color:#cf222e}.status-skipped{color:#57606a}</style>\n" +
		"</head>\n<body>\n<h1>" + html.EscapeString(title) + "</h1>\n" + body + "</body>\n</html>\n"
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
