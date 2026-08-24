package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"docker-manager/internal/sensitive"
)

const (
	FormatText     = "text"
	FormatJSON     = "json"
	FormatMarkdown = "markdown"
	FormatHTML     = "html"
)

func Print(w io.Writer, format string, report interface{}, printText func(io.Writer)) error {
	return PrintWithProfile(w, format, report, printText, sensitive.DefaultProfile())
}

func PrintWithProfile(w io.Writer, format string, report interface{}, printText func(io.Writer), profile sensitive.Profile) error {
	redactedReport := sensitive.RedactValue(report, profile)
	switch format {
	case "", FormatText:
		if profile == sensitive.ProfileNone || profile == "" {
			printText(w)
			return nil
		}
		var output bytes.Buffer
		printText(&output)
		_, err := io.WriteString(w, sensitive.RedactText(output.String(), profile))
		return err
	case FormatJSON:
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(redactedReport)
	case FormatMarkdown, "md":
		_, err := io.WriteString(w, RenderMarkdown(redactedReport))
		return err
	case FormatHTML:
		_, err := io.WriteString(w, RenderHTML(redactedReport))
		return err
	default:
		return fmt.Errorf("不支持的输出格式 %q，请使用 text、json、markdown 或 html", format)
	}
}
