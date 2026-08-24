//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

var textExtensions = map[string]bool{
	".example": true,
	".go":      true,
	".json":    true,
	".md":      true,
	".mod":     true,
	".ps1":     true,
	".sh":      true,
	".sum":     true,
	".txt":     true,
	".yaml":    true,
	".yml":     true,
}

var textBaseNames = map[string]bool{
	".editorconfig":  true,
	".gitattributes": true,
	".gitignore":     true,
}

func main() {
	files, err := repositoryTextFiles()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	failures := 0
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: read: %v\n", path, err)
			failures++
			continue
		}
		for _, problem := range textProblems(data) {
			fmt.Fprintf(os.Stderr, "%s: %s\n", filepath.ToSlash(path), problem)
			failures++
		}
	}

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "text check failed with %d problem(s)\n", failures)
		os.Exit(1)
	}
	fmt.Printf("Text check passed for %d file(s).\n", len(files))
}

func repositoryTextFiles() ([]string, error) {
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list repository files: %w", err)
	}

	var files []string
	for _, item := range bytes.Split(output, []byte{0}) {
		if len(item) == 0 {
			continue
		}
		path := string(item)
		if isTextFile(path) {
			files = append(files, path)
		}
	}
	sort.Strings(files)
	return files, nil
}

func isTextFile(path string) bool {
	base := filepath.Base(path)
	if textBaseNames[base] {
		return true
	}
	return textExtensions[strings.ToLower(filepath.Ext(base))]
}

func textProblems(data []byte) []string {
	var problems []string
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		problems = append(problems, "UTF-8 BOM is not allowed")
	}
	if !utf8.Valid(data) {
		problems = append(problems, "content is not valid UTF-8")
	}
	if bytes.ContainsRune(data, '\uFFFD') {
		problems = append(problems, "contains Unicode replacement character U+FFFD")
	}
	if bytes.ContainsRune(data, '\r') {
		problems = append(problems, "line endings must be LF")
	}
	if bytes.ContainsRune(data, 0) {
		problems = append(problems, "contains NUL byte")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		problems = append(problems, "file must end with a newline")
	}
	return problems
}
