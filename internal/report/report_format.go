package report

import (
	"encoding/json"
	"fmt"
	"io"
)

const (
	FormatText     = "text"
	FormatJSON     = "json"
	FormatMarkdown = "markdown"
	FormatHTML     = "html"
)

type FormatOptions struct {
	Format string
}

func Print(w io.Writer, format string, report interface{}, printText func(io.Writer)) error {
	switch format {
	case "", FormatText:
		printText(w)
		return nil
	case FormatJSON:
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	case FormatMarkdown, "md":
		_, err := io.WriteString(w, RenderMarkdown(report))
		return err
	case FormatHTML:
		_, err := io.WriteString(w, RenderHTML(report))
		return err
	default:
		return fmt.Errorf("不支持的输出格式 %q，请使用 text、json、markdown 或 html", format)
	}
}
