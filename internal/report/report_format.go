package report

import (
	"encoding/json"
	"fmt"
	"io"

	"docker-manager/internal/completion"

	"github.com/spf13/cobra"
)

const (
	FormatText = "text"
	FormatJSON = "json"
)

type FormatOptions struct {
	Format string
}

func AddFormatFlag(cmd *cobra.Command, format *string) {
	cmd.Flags().StringVar(format, "format", FormatText, "输出格式: text | json")
	_ = cmd.RegisterFlagCompletionFunc("format", completion.FixedValues(FormatText, FormatJSON))
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
	default:
		return fmt.Errorf("不支持的输出格式 %q，请使用 text 或 json", format)
	}
}
