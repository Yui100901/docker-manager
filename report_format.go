package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

const (
	reportFormatText = "text"
	reportFormatJSON = "json"
)

type ReportFormatOptions struct {
	Format string
}

func addReportFormatFlag(cmd *cobra.Command, format *string) {
	cmd.Flags().StringVar(format, "format", reportFormatText, "输出格式: text | json")
}

func printReport(w io.Writer, format string, report interface{}, printText func(io.Writer)) error {
	switch format {
	case "", reportFormatText:
		printText(w)
		return nil
	case reportFormatJSON:
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	default:
		return fmt.Errorf("unsupported report format %q, want text or json", format)
	}
}
