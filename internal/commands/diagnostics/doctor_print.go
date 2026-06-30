package diagnostics

import (
	"fmt"
	"io"
)

func doctorOverallStatus(checks []DoctorCheck) string {
	hasWarning := false
	for _, check := range checks {
		switch check.Status {
		case "failed":
			return "failed"
		case "warning":
			hasWarning = true
		}
	}
	if hasWarning {
		return "warning"
	}
	return "ok"
}

func doctorRecommendations(checks []DoctorCheck) []string {
	seen := map[string]bool{}
	var recommendations []string
	for _, check := range checks {
		if check.Recommended == "" || seen[check.Recommended] {
			continue
		}
		seen[check.Recommended] = true
		recommendations = append(recommendations, check.Recommended)
	}
	return recommendations
}

func printDoctorReport(w io.Writer, report DoctorReport) {
	fmt.Fprintln(w, "Docker manager doctor")
	fmt.Fprintf(w, "Platform: %s\n", report.Platform)
	fmt.Fprintf(w, "Overall: %s\n", report.OverallStatus)
	for _, check := range report.Checks {
		fmt.Fprintf(w, "- [%s] %s: %s", check.Status, check.Name, check.Message)
		if check.Detail != "" {
			fmt.Fprintf(w, " (%s)", check.Detail)
		}
		fmt.Fprintln(w)
		if check.Recommended != "" {
			fmt.Fprintf(w, "  建议: %s\n", check.Recommended)
		}
	}
}
