package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestFilterCompletionValuesSortsDeduplicatesAndMatchesPrefix(t *testing.T) {
	got := filterCompletionValues([]string{"worker", "api", "api", "db"}, "a")
	if strings.Join(got, ",") != "api" {
		t.Fatalf("filterCompletionValues() = %#v, want api", got)
	}
}

func TestCompleteFixedValuesDisablesFileCompletion(t *testing.T) {
	fn := completeFixedValues("json", "text")
	values, directive := fn(&cobra.Command{}, nil, "j")
	if strings.Join(values, ",") != "json" {
		t.Fatalf("values = %#v, want json", values)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v, want NoFileComp", directive)
	}
}
