package commandflags

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func thresholdCompletion(metrics []string) cobra.CompletionFunc {
	values := append([]string(nil), metrics...)
	sort.Strings(values)
	return func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if strings.Contains(toComplete, "=") {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var result []string
		for _, metric := range values {
			value := metric + "="
			if strings.HasPrefix(value, toComplete) {
				result = append(result, value)
			}
		}
		return result, cobra.ShellCompDirectiveNoFileComp
	}
}
