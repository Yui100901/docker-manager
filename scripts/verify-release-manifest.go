package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type releaseManifest struct {
	Version   string            `json:"version"`
	Commit    string            `json:"commit"`
	BuildDate string            `json:"build_date"`
	Artifacts []releaseArtifact `json:"artifacts"`
}

type releaseArtifact struct {
	Platform string `json:"platform"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Format   string `json:"format"`
	Binary   string `json:"binary"`
	Archive  string `json:"archive"`
	SHA256   string `json:"sha256"`
}

func main() {
	dir := flag.String("dir", "", "release directory")
	version := flag.String("version", "", "expected version")
	commit := flag.String("commit", "", "expected commit")
	count := flag.Int("count", -1, "expected artifact count")
	flag.Parse()
	if err := verifyRelease(*dir, *version, *commit, *count); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func verifyRelease(dir, version, commit string, count int) error {
	if dir == "" || version == "" || commit == "" || count < 0 {
		return fmt.Errorf("dir, version, commit and non-negative count are required")
	}
	manifestFile, err := os.Open(filepath.Join(dir, "release-manifest.json"))
	if err != nil {
		return err
	}
	defer manifestFile.Close()
	decoder := json.NewDecoder(io.LimitReader(manifestFile, 4<<20))
	decoder.DisallowUnknownFields()
	var manifest releaseManifest
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("decode release manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	if manifest.Version != version || manifest.Commit != commit {
		return fmt.Errorf("release manifest identity mismatch")
	}
	if len(manifest.Artifacts) != count {
		return fmt.Errorf("release manifest has %d artifacts, want %d", len(manifest.Artifacts), count)
	}
	checksums, err := readChecksums(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return err
	}
	if len(checksums) != count {
		return fmt.Errorf("checksums.txt has %d entries, want %d", len(checksums), count)
	}
	platforms := map[string]bool{}
	archives := map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		if artifact.Platform == "" || platforms[artifact.Platform] {
			return fmt.Errorf("empty or duplicate platform %q", artifact.Platform)
		}
		platforms[artifact.Platform] = true
		if artifact.Archive == "" || filepath.Base(artifact.Archive) != artifact.Archive || archives[artifact.Archive] {
			return fmt.Errorf("invalid or duplicate archive %q", artifact.Archive)
		}
		archives[artifact.Archive] = true
		archivePath := filepath.Join(dir, artifact.Archive)
		info, err := os.Lstat(archivePath)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("release archive is not a regular file: %s", artifact.Archive)
		}
		digest, err := fileSHA256(archivePath)
		if err != nil {
			return err
		}
		if digest != strings.ToLower(artifact.SHA256) {
			return fmt.Errorf("release archive digest mismatch: %s", artifact.Archive)
		}
		if checksums[artifact.Archive] != digest {
			return fmt.Errorf("checksums.txt mismatch: %s", artifact.Archive)
		}
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("release manifest contains trailing JSON")
		}
		return fmt.Errorf("read release manifest trailer: %w", err)
	}
	return nil
}

func readChecksums(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	result := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(file, 4<<20))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid checksum line %q", scanner.Text())
		}
		name := strings.TrimPrefix(fields[1], "*")
		if filepath.Base(name) != name || result[name] != "" {
			return nil, fmt.Errorf("invalid or duplicate checksum archive %q", name)
		}
		if len(fields[0]) != sha256.Size*2 {
			return nil, fmt.Errorf("invalid SHA-256 for %s", name)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return nil, fmt.Errorf("invalid SHA-256 for %s: %w", name, err)
		}
		result[name] = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
