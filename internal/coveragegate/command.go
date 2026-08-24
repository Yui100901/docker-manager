package coveragegate

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

type packageThresholds []string

func (values *packageThresholds) String() string {
	return strings.Join(*values, ",")
}

func (values *packageThresholds) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("coverage-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	profilePath := flags.String("profile", "", "Go coverage profile to verify")
	minimumTotal := flags.Float64("total", 0, "minimum global statement coverage percentage")
	var packageMinimums packageThresholds
	flags.Var(&packageMinimums, "package", "package threshold in path=percent form; repeatable")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "coverage-check: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if strings.TrimSpace(*profilePath) == "" {
		fmt.Fprintln(stderr, "coverage-check: -profile is required")
		return 2
	}
	if err := validatePercent("total", *minimumTotal); err != nil {
		fmt.Fprintf(stderr, "coverage-check: %v\n", err)
		return 2
	}

	type requirement struct {
		path    string
		minimum float64
	}
	requirements := make([]requirement, 0, len(packageMinimums))
	seen := make(map[string]bool)
	for _, value := range packageMinimums {
		packagePath, rawMinimum, ok := strings.Cut(value, "=")
		packagePath = strings.TrimSpace(packagePath)
		if !ok || packagePath == "" || strings.TrimSpace(rawMinimum) == "" {
			fmt.Fprintf(stderr, "coverage-check: invalid -package %q; expected path=percent\n", value)
			return 2
		}
		minimum, err := strconv.ParseFloat(strings.TrimSpace(rawMinimum), 64)
		if err != nil {
			fmt.Fprintf(stderr, "coverage-check: invalid percentage in -package %q\n", value)
			return 2
		}
		if err := validatePercent("package "+packagePath, minimum); err != nil {
			fmt.Fprintf(stderr, "coverage-check: %v\n", err)
			return 2
		}
		if seen[packagePath] {
			fmt.Fprintf(stderr, "coverage-check: duplicate package threshold %q\n", packagePath)
			return 2
		}
		seen[packagePath] = true
		requirements = append(requirements, requirement{path: packagePath, minimum: minimum})
	}

	file, err := os.Open(*profilePath)
	if err != nil {
		fmt.Fprintf(stderr, "coverage-check: open profile: %v\n", err)
		return 1
	}
	profile, parseErr := Parse(file)
	closeErr := file.Close()
	if parseErr != nil {
		fmt.Fprintf(stderr, "coverage-check: %v\n", parseErr)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "coverage-check: close profile: %v\n", closeErr)
		return 1
	}

	failed := printResult(stdout, "total", profile.Total, *minimumTotal)
	for _, requirement := range requirements {
		stats, err := profile.Package(requirement.path)
		if err != nil {
			fmt.Fprintf(stderr, "coverage-check: %v\n", err)
			failed = true
			continue
		}
		if printResult(stdout, requirement.path, stats, requirement.minimum) {
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}

func validatePercent(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
		return fmt.Errorf("%s coverage threshold %.2f is outside 0..100", name, value)
	}
	return nil
}

func printResult(writer io.Writer, name string, stats Stats, minimum float64) bool {
	status := "PASS"
	failed := stats.Percent()+1e-9 < minimum
	if failed {
		status = "FAIL"
	}
	fmt.Fprintf(writer, "coverage %-24s %6.2f%% (%d/%d), minimum %6.2f%%: %s\n",
		name, stats.Percent(), stats.Covered, stats.Total, minimum, status)
	return failed
}
