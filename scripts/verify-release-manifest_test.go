package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyReleaseDetectsArchiveTampering(t *testing.T) {
	dir := t.TempDir()
	archiveName := "dm_v1.0.0_linux_amd64.tar.gz"
	archivePath := filepath.Join(dir, archiveName)
	if err := os.WriteFile(archivePath, []byte("release archive"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := releaseManifest{
		Version:   "v1.0.0",
		Commit:    "abc123",
		BuildDate: "2026-08-18T00:00:00Z",
		Artifacts: []releaseArtifact{{
			Platform: "linux/amd64",
			OS:       "linux",
			Arch:     "amd64",
			Format:   "tar.gz",
			Binary:   "dm",
			Archive:  archiveName,
			SHA256:   digest,
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release-manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(digest+"  "+archiveName+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := verifyRelease(dir, "v1.0.0", "abc123", 1); err != nil {
		t.Fatalf("verifyRelease() error = %v", err)
	}
	if err := os.WriteFile(archivePath, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(dir, "v1.0.0", "abc123", 1); err == nil {
		t.Fatal("verifyRelease() accepted a tampered archive")
	}
}
