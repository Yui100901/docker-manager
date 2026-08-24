package coveragegate

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

type Stats struct {
	Covered int
	Total   int
}

func (s Stats) Percent() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Covered) * 100 / float64(s.Total)
}

type Profile struct {
	Total    Stats
	Packages map[string]Stats
}

func Parse(reader io.Reader) (Profile, error) {
	profile := Profile{Packages: make(map[string]Stats)}
	scanner := bufio.NewScanner(reader)
	lineNumber := 0
	modeSeen := false
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !modeSeen {
			if !strings.HasPrefix(line, "mode: ") || strings.TrimSpace(strings.TrimPrefix(line, "mode: ")) == "" {
				return Profile{}, fmt.Errorf("coverage profile line %d: missing mode header", lineNumber)
			}
			modeSeen = true
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 3 {
			return Profile{}, fmt.Errorf("coverage profile line %d: expected location, statement count, and execution count", lineNumber)
		}
		separator := strings.LastIndex(fields[0], ":")
		if separator <= 0 {
			return Profile{}, fmt.Errorf("coverage profile line %d: invalid source location %q", lineNumber, fields[0])
		}
		statementCount, err := strconv.Atoi(fields[1])
		if err != nil || statementCount < 0 {
			return Profile{}, fmt.Errorf("coverage profile line %d: invalid statement count %q", lineNumber, fields[1])
		}
		executionCount, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return Profile{}, fmt.Errorf("coverage profile line %d: invalid execution count %q", lineNumber, fields[2])
		}

		if statementCount == 0 {
			continue
		}
		packagePath := path.Dir(strings.ReplaceAll(fields[0][:separator], "\\", "/"))
		stats := profile.Packages[packagePath]
		stats.Total += statementCount
		profile.Total.Total += statementCount
		if executionCount > 0 {
			stats.Covered += statementCount
			profile.Total.Covered += statementCount
		}
		profile.Packages[packagePath] = stats
	}
	if err := scanner.Err(); err != nil {
		return Profile{}, fmt.Errorf("read coverage profile: %w", err)
	}
	if !modeSeen {
		return Profile{}, fmt.Errorf("coverage profile is empty")
	}
	if profile.Total.Total == 0 {
		return Profile{}, fmt.Errorf("coverage profile contains no statements")
	}
	return profile, nil
}

func (p Profile) Package(selector string) (Stats, error) {
	selector = strings.Trim(strings.ReplaceAll(strings.TrimSpace(selector), "\\", "/"), "/")
	selector = strings.TrimPrefix(selector, "./")
	if selector == "" {
		return Stats{}, fmt.Errorf("coverage package selector is empty")
	}

	var (
		match      Stats
		matchedKey string
	)
	for packagePath, stats := range p.Packages {
		normalized := strings.Trim(packagePath, "/")
		if normalized != selector && !strings.HasSuffix(normalized, "/"+selector) {
			continue
		}
		if matchedKey != "" {
			return Stats{}, fmt.Errorf("coverage package selector %q is ambiguous between %q and %q", selector, matchedKey, packagePath)
		}
		matchedKey = packagePath
		match = stats
	}
	if matchedKey == "" {
		return Stats{}, fmt.Errorf("coverage package %q is missing from profile", selector)
	}
	return match, nil
}
