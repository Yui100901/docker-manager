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
	FormatSARIF    = "sarif"
)

func Print(w io.Writer, format string, report interface{}, printText func(io.Writer)) error {
	return PrintWithProfile(w, format, report, printText, sensitive.DefaultProfile())
}

func PrintWithProfile(w io.Writer, format string, report interface{}, printText func(io.Writer), profile sensitive.Profile) error {
	return printWithEvaluation(w, format, report, nil, printText, profile, false)
}

func PrintEvaluated(w io.Writer, format string, report interface{}, evaluation *Evaluation, printText func(io.Writer)) error {
	return PrintEvaluatedWithProfile(w, format, report, evaluation, printText, sensitive.DefaultProfile())
}

func PrintEvaluatedWithProfile(w io.Writer, format string, report interface{}, evaluation *Evaluation, printText func(io.Writer), profile sensitive.Profile) error {
	return printWithEvaluation(w, format, report, evaluation, printText, profile, true)
}

func ValidateFormat(format string, allowSARIF bool) error {
	switch format {
	case "", FormatText, FormatJSON, FormatMarkdown, "md", FormatHTML:
		return nil
	case FormatSARIF:
		if allowSARIF {
			return nil
		}
	}
	formats := "text、json、markdown 或 html"
	if allowSARIF {
		formats = "text、json、markdown、html 或 sarif"
	}
	return fmt.Errorf("不支持的输出格式 %q，请使用 %s", format, formats)
}

func printWithEvaluation(w io.Writer, format string, value interface{}, evaluation *Evaluation, printText func(io.Writer), profile sensitive.Profile, allowSARIF bool) error {
	if err := ValidateFormat(format, allowSARIF); err != nil {
		return err
	}
	redactedReport := sensitive.RedactValue(value, profile)
	switch format {
	case "", FormatText:
		if profile == sensitive.ProfileNone || profile == "" {
			printText(w)
			if evaluation != nil {
				PrintEvaluationText(w, evaluation)
			}
			return nil
		}
		var output bytes.Buffer
		printText(&output)
		if evaluation != nil {
			PrintEvaluationText(&output, evaluation)
		}
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
	case FormatSARIF:
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(sensitive.RedactValue(buildSARIF(evaluation), profile))
	}
	return nil
}
