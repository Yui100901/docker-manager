package report

import (
	"fmt"
	"reflect"
	"strings"
)

func RenderMarkdown(report interface{}) string {
	var sb strings.Builder
	title := reportTitle(report)
	sb.WriteString("# ")
	sb.WriteString(markdownEscape(title))
	sb.WriteString("\n\n")
	writeMarkdownValue(&sb, 2, "", reflect.ValueOf(report))
	return sb.String()
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
