package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitArchiveRollbackPreservesReplacedPublishedArtifact(t *testing.T) {
	root := t.TempDir()
	stagingDir := filepath.Join(root, ".dm-backup-staging-test")
	if err := os.Mkdir(stagingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stagedPath := filepath.Join(stagingDir, "bundle.tar.gz.part-001")
	finalPath := filepath.Join(root, "bundle.tar.gz.part-001")
	if err := os.WriteFile(stagedPath, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishBackupStagedFile(stagedPath, finalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}

	writer := &splitFileWriter{
		basePath:   filepath.Join(root, "bundle.tar.gz"),
		published:  []backupPublishedArtifact{{stagedPath: stagedPath, finalPath: finalPath}},
		stagingDir: stagingDir,
	}
	err := writer.rollbackPublished()
	if err == nil || !strings.Contains(err.Error(), "refusing to remove replaced backup artifact") {
		t.Fatalf("rollbackPublished() error = %v, want replacement rejection", err)
	}
	data, readErr := os.ReadFile(finalPath)
	if readErr != nil || string(data) != "foreign" {
		t.Fatalf("foreign artifact changed: data=%q error=%v", data, readErr)
	}
}
