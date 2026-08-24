package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestDecryptRejectsOversizedChunkBeforeAllocation(t *testing.T) {
	var encrypted bytes.Buffer
	encrypted.WriteString(backupEncryptionMagic)
	encrypted.Write(make([]byte, backupEncryptionSaltSize+backupEncryptionPrefixSize))
	if err := binary.Write(&encrypted, binary.BigEndian, uint32(backupEncryptionChunkSize+16+1)); err != nil {
		t.Fatal(err)
	}
	var plaintext bytes.Buffer
	err := decryptBackupArchiveStream(context.Background(), &encrypted, &plaintext, []byte("secret"))
	if err == nil || !strings.Contains(err.Error(), "chunk length") {
		t.Fatalf("decryptBackupArchiveStream() error = %v, want chunk length rejection", err)
	}
	if plaintext.Len() != 0 {
		t.Fatalf("plaintext length = %d, want no allocation-backed output", plaintext.Len())
	}
}

func TestExtractBackupArchiveEnforcesHeaderBudgets(t *testing.T) {
	t.Run("single file size", func(t *testing.T) {
		archive := writeHeaderOnlyBackupArchive(t, &tar.Header{
			Name:     "oversized.bin",
			Mode:     0600,
			Typeflag: tar.TypeReg,
			Size:     maxBackupArchiveFileBytes + 1,
		})
		err := extractBackupArchive(archive, filepath.Join(t.TempDir(), "extract"))
		if err == nil || !strings.Contains(err.Error(), "file") || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("extractBackupArchive() error = %v, want file-size limit", err)
		}
	})

	t.Run("path length", func(t *testing.T) {
		archive := writeHeaderOnlyBackupArchive(t, &tar.Header{
			Name:     strings.Repeat("a", maxBackupArchivePathBytes+1),
			Mode:     0600,
			Typeflag: tar.TypeReg,
			Size:     0,
		})
		err := extractBackupArchive(archive, filepath.Join(t.TempDir(), "extract"))
		if err == nil || !strings.Contains(err.Error(), "path") || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("extractBackupArchive() error = %v, want path limit", err)
		}
	})

	t.Run("unsupported link", func(t *testing.T) {
		archive := writeHeaderOnlyBackupArchive(t, &tar.Header{
			Name:     "linked",
			Linkname: "target",
			Mode:     0777,
			Typeflag: tar.TypeSymlink,
		})
		err := extractBackupArchive(archive, filepath.Join(t.TempDir(), "extract"))
		if err == nil || !strings.Contains(err.Error(), "unsupported entry type") {
			t.Fatalf("extractBackupArchive() error = %v, want link rejection", err)
		}
	})
}

func TestExtractBackupArchiveAcceptsLegacyNULRegularType(t *testing.T) {
	const payload = "legacy regular file"
	var rawArchive bytes.Buffer
	tarWriter := tar.NewWriter(&rawArchive)
	if err := tarWriter.WriteHeader(&tar.Header{
		Name:     "legacy.txt",
		Mode:     0600,
		Size:     int64(len(payload)),
		Typeflag: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}

	// Older archives used a NUL typeflag for a regular file. archive/tar
	// normalizes it before returning the header.
	archivePath := filepath.Join(t.TempDir(), "legacy.tar.gz")
	archiveFile, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(archiveFile)
	if _, err := gzipWriter.Write(rawArchive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "extract")
	if err := extractBackupArchive(archivePath, destination); err != nil {
		t.Fatalf("extract legacy archive: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "legacy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("legacy payload = %q, want %q", got, payload)
	}
}

func TestBackupTarBudgetBoundsEntriesPathsAndExpandedBytes(t *testing.T) {
	entryBudget := backupTarBudget{entries: maxBackupArchiveEntries - 1}
	if err := entryBudget.Add("last", 0, false); err != nil {
		t.Fatal(err)
	}
	if err := entryBudget.Add("too-many", 0, false); err == nil || !strings.Contains(err.Error(), "entry limit") {
		t.Fatalf("entry budget error = %v, want entry limit", err)
	}

	var pathBudget backupTarBudget
	if err := pathBudget.Add(strings.Repeat("p", maxBackupArchivePathBytes+1), 0, false); err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("path budget error = %v, want path limit", err)
	}

	expandedBudget := backupTarBudget{bytes: maxBackupArchiveExpandedBytes - 10}
	if err := expandedBudget.Add("bomb", 11, true); err == nil || !strings.Contains(err.Error(), "expanded-size") {
		t.Fatalf("expanded budget error = %v, want gzip expansion limit", err)
	}
}

func TestSplitArchiveCommitManifestAndDigestValidation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("split-archive-payload"), 200)
	if err := os.WriteFile(filepath.Join(source, "payload.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "bundle.tar.gz")
	if err := createBackupArchiveWithOptions(context.Background(), source, base, backupArchiveOptions{SplitSize: 128}); err != nil {
		t.Fatal(err)
	}
	manifestData, err := os.ReadFile(backupPartsManifestPath(base))
	if err != nil {
		t.Fatal(err)
	}
	var manifest backupArchivePartManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Commit != "complete" || len(manifest.Parts) < 2 || !validBackupSHA256(manifest.ArchiveSHA256) {
		t.Fatalf("parts manifest = %#v, want committed ordered digests", manifest)
	}
	for i, part := range manifest.Parts {
		if part.Index != i+1 || part.Name != filepath.Base(splitPartPath(base, i+1)) || part.Size <= 0 || !validBackupSHA256(part.SHA256) {
			t.Fatalf("part[%d] = %#v, want canonical index/name/size/digest", i, part)
		}
	}
	joined := filepath.Join(root, "joined.tar.gz")
	if err := joinBackupArchivePartsWithContext(context.Background(), splitPartPath(base, 1), joined); err != nil {
		t.Fatalf("joinBackupArchivePartsWithContext() error = %v", err)
	}

	partPath := splitPartPath(base, 1)
	partData, err := os.ReadFile(partPath)
	if err != nil {
		t.Fatal(err)
	}
	partData[0] ^= 0xff
	if err := os.WriteFile(partPath, partData, 0600); err != nil {
		t.Fatal(err)
	}
	tamperedOutput := filepath.Join(root, "tampered.tar.gz")
	err = joinBackupArchivePartsWithContext(context.Background(), splitPartPath(base, 1), tamperedOutput)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("join tampered parts error = %v, want digest mismatch", err)
	}
	if _, statErr := os.Stat(tamperedOutput); !os.IsNotExist(statErr) {
		t.Fatalf("tampered join output remains after failure: %v", statErr)
	}
}

func TestSplitArchiveRequiresCommittedContinuousManifest(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "bundle.tar.gz")
	if err := os.WriteFile(splitPartPath(base, 1), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	err := joinBackupArchivePartsWithContext(context.Background(), splitPartPath(base, 1), filepath.Join(root, "joined"))
	if err == nil || !strings.Contains(err.Error(), "committed backup parts manifest") {
		t.Fatalf("join without manifest error = %v, want commit rejection", err)
	}
}

func TestSplitArchiveRejectsMissingAndUnexpectedParts(t *testing.T) {
	t.Run("missing middle part", func(t *testing.T) {
		base, manifest := createTestSplitArchive(t, 256, 8<<10)
		if len(manifest.Parts) < 3 {
			t.Fatalf("parts = %d, want at least 3", len(manifest.Parts))
		}
		if err := os.Remove(splitPartPath(base, 2)); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(t.TempDir(), "joined.tar.gz")
		err := joinBackupArchivePartsWithContext(context.Background(), splitPartPath(base, 1), output)
		if err == nil || !strings.Contains(err.Error(), "part 2") {
			t.Fatalf("join missing part error = %v, want contiguous part rejection", err)
		}
		if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
			t.Fatalf("join output remains after missing part: %v", statErr)
		}
	})

	t.Run("unexpected numeric part", func(t *testing.T) {
		base, manifest := createTestSplitArchive(t, 256, 8<<10)
		extra := splitPartPath(base, len(manifest.Parts)+1)
		if err := os.WriteFile(extra, []byte("uncommitted"), 0600); err != nil {
			t.Fatal(err)
		}
		err := joinBackupArchivePartsWithContext(context.Background(), splitPartPath(base, 1), filepath.Join(t.TempDir(), "joined.tar.gz"))
		if err == nil || !strings.Contains(err.Error(), "unexpected backup archive part") {
			t.Fatalf("join extra part error = %v, want unexpected part rejection", err)
		}
	})
}

func TestSplitArchivePartCountLimitLeavesNoPublishedFiles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 8<<10)
	if _, err := cryptorand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "random.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "limited.tar.gz")
	err := createBackupArchiveWithOptions(context.Background(), source, base, backupArchiveOptions{SplitSize: 1})
	if err == nil || !strings.Contains(err.Error(), "part limit") {
		t.Fatalf("create split archive error = %v, want part-count limit", err)
	}
	paths, globErr := filepath.Glob(base + ".part-*")
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(paths) != 0 {
		t.Fatalf("partial split archive was published: %#v", paths)
	}
	if _, statErr := os.Stat(backupPartsManifestPath(base)); !os.IsNotExist(statErr) {
		t.Fatalf("parts commit manifest exists after failure: %v", statErr)
	}
}

func TestSplitArchiveManifestCollisionRollsBackPublishedParts(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), bytes.Repeat([]byte("payload"), 512), 0600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "bundle.tar.gz")
	manifestPath := backupPartsManifestPath(base)
	if err := os.WriteFile(manifestPath, []byte("keep-existing-marker"), 0600); err != nil {
		t.Fatal(err)
	}

	err := createBackupArchiveWithOptions(context.Background(), source, base, backupArchiveOptions{SplitSize: 128})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create split archive error = %v, want existing manifest rejection", err)
	}
	parts, globErr := filepath.Glob(base + ".part-*")
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(parts) != 0 {
		t.Fatalf("parts remain after failed commit marker publication: %#v", parts)
	}
	marker, readErr := os.ReadFile(manifestPath)
	if readErr != nil || string(marker) != "keep-existing-marker" {
		t.Fatalf("preexisting commit marker changed: content=%q error=%v", marker, readErr)
	}
	staging, globErr := filepath.Glob(filepath.Join(root, ".dm-backup-staging-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(staging) != 0 {
		t.Fatalf("staging remains after failed commit marker publication: %#v", staging)
	}
}

func TestSplitArchiveRecoversInterruptedPublishedPartsAndRetries(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "bundle.tar.gz")
	interruptedPayload := bytes.Repeat([]byte("interrupted"), 80)
	simulateInterruptedSplitPublish(t, base, 64, interruptedPayload, false)

	parts, err := filepath.Glob(base + ".part-*")
	if err != nil || len(parts) < 2 {
		t.Fatalf("interrupted parts = %#v, error=%v", parts, err)
	}
	if _, err := os.Stat(backupSplitTransactionPath(base)); err != nil {
		t.Fatalf("pending marker missing after simulated crash: %v", err)
	}

	retryPayload := bytes.Repeat([]byte("retry"), 100)
	retry, err := newSplitFileWriter(context.Background(), base, 64)
	if err != nil {
		t.Fatalf("newSplitFileWriter() retry error = %v", err)
	}
	if _, err := retry.Write(retryPayload); err != nil {
		t.Fatal(err)
	}
	if err := retry.Close(); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}

	joined := filepath.Join(root, "joined")
	if err := joinBackupArchivePartsWithContext(context.Background(), splitPartPath(base, 1), joined); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(joined)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, retryPayload) {
		t.Fatalf("joined retry payload length=%d, want %d", len(data), len(retryPayload))
	}
	if _, err := os.Stat(backupSplitTransactionPath(base)); !os.IsNotExist(err) {
		t.Fatalf("pending marker remains after retry: %v", err)
	}
	staging, err := filepath.Glob(filepath.Join(root, ".dm-backup-staging-*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("staging remains after retry: paths=%#v error=%v", staging, err)
	}
}

func TestSplitArchiveActiveWriterCannotBeRecoveredByConcurrentWriter(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "bundle.tar.gz")
	active, err := newSplitFileWriter(context.Background(), base, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := active.Write([]byte("active")); err != nil {
		t.Fatal(err)
	}

	_, err = newSplitFileWriter(context.Background(), base, 64)
	if err == nil || !errors.Is(err, errBackupTransactionLocked) {
		t.Fatalf("concurrent newSplitFileWriter() error = %v, want active transaction rejection", err)
	}
	if _, err := os.Stat(backupSplitTransactionPath(base)); err != nil {
		t.Fatalf("active pending marker was changed: %v", err)
	}
	if err := active.Abort(); err != nil {
		t.Fatalf("active Abort() error = %v", err)
	}
}

func TestSplitArchiveRecoveryDoesNotDeleteForeignPart(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "bundle.tar.gz")
	simulateInterruptedSplitPublish(t, base, 64, bytes.Repeat([]byte("payload"), 100), false)

	partPath := splitPartPath(base, 1)
	partData, err := os.ReadFile(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(partPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, partData, 0600); err != nil {
		t.Fatal(err)
	}

	_, err = newSplitFileWriter(context.Background(), base, 64)
	if err == nil || !strings.Contains(err.Error(), "unowned split backup part") {
		t.Fatalf("recovery error = %v, want unowned part rejection", err)
	}
	data, readErr := os.ReadFile(partPath)
	if readErr != nil || !bytes.Equal(data, partData) {
		t.Fatalf("foreign part changed: data=%q error=%v", data, readErr)
	}
	if _, err := os.Stat(backupSplitTransactionPath(base)); err != nil {
		t.Fatalf("pending marker removed after ownership conflict: %v", err)
	}
}

func TestSplitArchiveRecoveryPreservesCommittedParts(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "bundle.tar.gz")
	simulateInterruptedSplitPublish(t, base, 64, bytes.Repeat([]byte("committed"), 100), true)

	partPath := splitPartPath(base, 1)
	partData, err := os.ReadFile(partPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newSplitFileWriter(context.Background(), base, 64)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("new writer error = %v, want committed artifact rejection", err)
	}
	after, readErr := os.ReadFile(partPath)
	if readErr != nil || !bytes.Equal(after, partData) {
		t.Fatalf("committed part changed: data=%q error=%v", after, readErr)
	}
	if _, err := os.Stat(backupPartsManifestPath(base)); err != nil {
		t.Fatalf("committed manifest changed: %v", err)
	}
	if _, err := os.Stat(backupSplitTransactionPath(base)); !os.IsNotExist(err) {
		t.Fatalf("stale pending marker remains beside committed archive: %v", err)
	}
}

func TestArchiveFailureDoesNotPublishAndRejectsSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	linkedSource := filepath.Join(source, "linked")
	if err := os.Symlink(filepath.Join(source, "payload"), linkedSource); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	archive := filepath.Join(root, "bundle.tar.gz")
	err := createBackupArchive(source, archive)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("createBackupArchive() error = %v, want source symlink rejection", err)
	}
	if _, statErr := os.Lstat(archive); !os.IsNotExist(statErr) {
		t.Fatalf("archive was published after failure: %v", statErr)
	}
	staging, err := filepath.Glob(filepath.Join(root, ".dm-backup-staging-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staging) != 0 {
		t.Fatalf("staging directories remain after failure: %#v", staging)
	}
	if err := os.Remove(linkedSource); err != nil {
		t.Fatal(err)
	}

	preexisting := filepath.Join(root, "preexisting.tar.gz")
	if err := os.WriteFile(preexisting, []byte("keep-existing"), 0600); err != nil {
		t.Fatal(err)
	}
	err = createBackupArchive(source, preexisting)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("createBackupArchive(preexisting output) error = %v", err)
	}
	if content, readErr := os.ReadFile(preexisting); readErr != nil || string(content) != "keep-existing" {
		t.Fatalf("preexisting archive changed: content=%q error=%v", content, readErr)
	}

	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	linkedOutput := filepath.Join(root, "linked-output.tar.gz")
	if err := os.Symlink(target, linkedOutput); err != nil {
		t.Skipf("output symbolic links unavailable: %v", err)
	}
	err = createBackupArchive(source, linkedOutput)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("createBackupArchive(symlink output) error = %v", err)
	}
	content, readErr := os.ReadFile(target)
	if readErr != nil || string(content) != "keep" {
		t.Fatalf("symlink target changed: content=%q error=%v", content, readErr)
	}
}

func TestEncryptedBackupRemovesPlaintextTreeOnSuccessFailureAndCancel(t *testing.T) {
	passphrase := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(passphrase, []byte("correct horse battery staple\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("success", func(t *testing.T) {
		fake := &fakeBackupDockerService{inspect: basicRestoreInspect("demo")}
		restoreFactory := replaceBackupServiceFactory(fake)
		defer restoreFactory()
		outputDir := filepath.Join(t.TempDir(), "plaintext")
		archive := filepath.Join(t.TempDir(), "bundle.tar.gz.enc")
		result, err := backupContainer(context.Background(), "demo", BackupOptions{
			OutputDir: outputDir, Bundle: true, Encrypt: true, PassphraseFile: passphrase, BundleOutput: archive,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result != archive {
			t.Fatalf("result = %q, want encrypted archive %q", result, archive)
		}
		if _, err := os.Stat(outputDir); !os.IsNotExist(err) {
			t.Fatalf("plaintext tree remains after success: %v", err)
		}
		if _, err := os.Stat(archive); err != nil {
			t.Fatalf("encrypted archive missing: %v", err)
		}
	})

	t.Run("failure", func(t *testing.T) {
		fake := &fakeBackupDockerService{inspect: basicRestoreInspect("demo")}
		restoreFactory := replaceBackupServiceFactory(fake)
		defer restoreFactory()
		outputDir := filepath.Join(t.TempDir(), "plaintext")
		archive := filepath.Join(t.TempDir(), "bundle.tar.gz.enc")
		invalidKey := filepath.Join(t.TempDir(), "invalid-key.pem")
		if err := os.WriteFile(invalidKey, []byte("not a key"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := backupContainer(context.Background(), "demo", BackupOptions{
			OutputDir: outputDir, Bundle: true, Encrypt: true, PassphraseFile: passphrase, BundleOutput: archive, SigningKey: invalidKey,
		})
		if err == nil {
			t.Fatal("backupContainer() error = nil, want signing failure")
		}
		if _, err := os.Stat(outputDir); !os.IsNotExist(err) {
			t.Fatalf("plaintext tree remains after failure: %v", err)
		}
		if _, err := os.Stat(archive); !os.IsNotExist(err) {
			t.Fatalf("encrypted archive published after failure: %v", err)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fake := &fakeBackupDockerService{inspect: basicRestoreInspect("demo"), cancelAfterSave: cancel}
		restoreFactory := replaceBackupServiceFactory(fake)
		defer restoreFactory()
		outputDir := filepath.Join(t.TempDir(), "plaintext")
		archive := filepath.Join(t.TempDir(), "bundle.tar.gz.enc")
		_, err := backupContainer(ctx, "demo", BackupOptions{
			OutputDir: outputDir, IncludeImage: true, Bundle: true, Encrypt: true, PassphraseFile: passphrase, BundleOutput: archive,
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("backupContainer() error = %v, want context.Canceled", err)
		}
		if _, err := os.Stat(outputDir); !os.IsNotExist(err) {
			t.Fatalf("plaintext tree remains after cancellation: %v", err)
		}
		if _, err := os.Stat(archive); !os.IsNotExist(err) {
			t.Fatalf("encrypted archive published after cancellation: %v", err)
		}
	})
}

func TestBackupOutputsUsePrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits consistently")
	}
	fake := &fakeBackupDockerService{inspect: container.InspectResponse{
		Name: "/demo", Config: &container.Config{Image: "busybox:latest"}, HostConfig: &container.HostConfig{},
	}}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	root := t.TempDir()
	outputDir := filepath.Join(root, "backup")
	archive := filepath.Join(root, "bundle.tar.gz")
	if _, err := backupContainer(context.Background(), "demo", BackupOptions{OutputDir: outputDir, Bundle: true, BundleOutput: archive}); err != nil {
		t.Fatal(err)
	}
	assertBackupMode(t, outputDir, 0700)
	for _, name := range []string{backupManifestName, backupInspectName, backupComposeName, backupReadmeName, backupChecksumName} {
		assertBackupMode(t, filepath.Join(outputDir, name), 0600)
	}
	assertBackupMode(t, filepath.Join(outputDir, backupRestoreName), 0700)
	assertBackupMode(t, archive, 0600)
}

func TestRestoreJSONBudgetsRejectOversizedItemAndDepth(t *testing.T) {
	t.Run("oversized inspect", func(t *testing.T) {
		root := t.TempDir()
		writeTestJSON(t, filepath.Join(root, backupManifestName), basicRestoreManifest("demo"))
		inspectPath := filepath.Join(root, backupInspectName)
		if err := os.WriteFile(inspectPath, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(inspectPath, maxBackupInspectJSONBytes+1); err != nil {
			t.Fatal(err)
		}
		manifest := normalizeBackupManifest(basicRestoreManifest("demo"))
		err := validateRestoreJSONBudget(root, manifest)
		if err == nil || !strings.Contains(err.Error(), "inspect") || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("validateRestoreJSONBudget() error = %v, want inspect size limit", err)
		}
	})

	t.Run("depth", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "deep.json")
		data := strings.Repeat("[", maxBackupJSONDepth+1) + "0" + strings.Repeat("]", maxBackupJSONDepth+1)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		var value interface{}
		err := readLimitedBackupJSON(path, maxBackupManifestJSONBytes, &value)
		if err == nil || !strings.Contains(err.Error(), "depth") {
			t.Fatalf("readLimitedBackupJSON() error = %v, want depth limit", err)
		}
	})

	t.Run("cumulative bytes", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("large sparse files are not portable on Windows")
		}
		root := t.TempDir()
		manifest := BackupManifest{Version: 1}
		perFile := maxBackupJSONTotalBytes/5 + 1
		for i := 0; i < 5; i++ {
			entryPath := filepath.ToSlash(filepath.Join("containers", string(rune('a'+i))))
			entryDir := filepath.Join(root, filepath.FromSlash(entryPath))
			if err := os.MkdirAll(entryDir, 0700); err != nil {
				t.Fatal(err)
			}
			inspectPath := filepath.Join(entryDir, backupInspectName)
			if err := os.WriteFile(inspectPath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(inspectPath, perFile); err != nil {
				t.Fatal(err)
			}
			manifest.Containers = append(manifest.Containers, BackupContainerManifest{
				ContainerName: string(rune('a' + i)), Path: entryPath, InspectFile: backupInspectName,
			})
		}
		writeTestJSON(t, filepath.Join(root, backupManifestName), manifest)
		err := validateRestoreJSONBudget(root, manifest)
		if err == nil || !strings.Contains(err.Error(), "restore JSON inputs") || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("validateRestoreJSONBudget() error = %v, want cumulative byte budget", err)
		}
	})
}

func TestRestoreLimitsAreConfigurableDownward(t *testing.T) {
	cmd := NewRestoreCommand()
	for _, name := range []string{"max-archive-size", "max-expanded-size", "max-json-size", "max-parts"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("restore command missing --%s", name)
		}
	}

	if _, err := parseBackupByteSize("limit", "9223372036854775807T"); err == nil || !strings.Contains(err.Error(), "byte range") {
		t.Fatalf("overflowing byte limit error = %v", err)
	}
	if _, err := resolveRestoreLimits(RestoreOptions{MaxParts: -1}); err == nil || !strings.Contains(err.Error(), "part limit") {
		t.Fatalf("negative part limit error = %v", err)
	}

	t.Run("archive bytes", func(t *testing.T) {
		archive := writeHeaderOnlyBackupArchive(t, &tar.Header{Name: "payload", Mode: 0600, Size: 0, Typeflag: tar.TypeReg})
		_, cleanup, err := resolveRestoreBackupDirWithOptions(context.Background(), archive, RestoreOptions{MaxArchiveBytes: 1})
		if cleanup != nil {
			cleanup()
		}
		if err == nil || !strings.Contains(err.Error(), "archive exceeds") {
			t.Fatalf("archive limit error = %v", err)
		}
	})

	t.Run("expanded bytes", func(t *testing.T) {
		archive := writeHeaderOnlyBackupArchive(t, &tar.Header{Name: "payload", Mode: 0600, Size: 1024, Typeflag: tar.TypeReg})
		_, cleanup, err := resolveRestoreBackupDirWithOptions(context.Background(), archive, RestoreOptions{MaxExpandedBytes: 512})
		if cleanup != nil {
			cleanup()
		}
		if err == nil || !strings.Contains(err.Error(), "expanded-size limit") {
			t.Fatalf("expanded limit error = %v", err)
		}
	})

	t.Run("parts", func(t *testing.T) {
		base, manifest := createTestSplitArchive(t, 128, 8<<10)
		if len(manifest.Parts) < 2 {
			t.Fatalf("parts = %d, want at least 2", len(manifest.Parts))
		}
		_, cleanup, err := resolveRestoreBackupDirWithOptions(context.Background(), splitPartPath(base, 1), RestoreOptions{MaxParts: 1})
		if cleanup != nil {
			cleanup()
		}
		if err == nil || !strings.Contains(err.Error(), "part limit") {
			t.Fatalf("part limit error = %v", err)
		}
	})

	t.Run("JSON total", func(t *testing.T) {
		root := t.TempDir()
		manifest := normalizeBackupManifest(basicRestoreManifest("demo"))
		writeTestJSON(t, filepath.Join(root, backupManifestName), manifest)
		writeTestJSON(t, filepath.Join(root, backupInspectName), basicRestoreInspect("demo"))
		err := validateRestoreJSONBudgetWithLimit(root, manifest, 1)
		if err == nil || !strings.Contains(err.Error(), "restore JSON inputs") {
			t.Fatalf("JSON limit error = %v", err)
		}
	})
}

func TestConfirmedDirectoryRestoreSnapshotLimitFailsBeforeDocker(t *testing.T) {
	root := t.TempDir()
	writeTestJSON(t, filepath.Join(root, backupManifestName), basicRestoreManifest("demo"))
	writeTestJSON(t, filepath.Join(root, backupInspectName), basicRestoreInspect("demo"))
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), root, RestoreOptions{
		Confirm: true, NoStart: true, SkipChecksum: true, MaxExpandedBytes: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "expanded-size limit") {
		t.Fatalf("restoreBackup() error = %v, want snapshot expanded-size limit", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %#v, snapshot limit failure must happen before Docker access", fake.calls)
	}
}

func createTestSplitArchive(t *testing.T, splitSize int64, payloadSize int) (string, backupArchivePartManifest) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, payloadSize)
	if _, err := cryptorand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "bundle.tar.gz")
	if err := createBackupArchiveWithOptions(context.Background(), source, base, backupArchiveOptions{SplitSize: splitSize}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(backupPartsManifestPath(base))
	if err != nil {
		t.Fatal(err)
	}
	var manifest backupArchivePartManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return base, manifest
}

func simulateInterruptedSplitPublish(t *testing.T, base string, partSize int64, payload []byte, publishManifest bool) {
	t.Helper()
	writer, err := newSplitFileWriter(context.Background(), base, partSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.finalizeCurrentPart(); err != nil {
		t.Fatal(err)
	}
	manifestPath, err := writer.writePartsManifest()
	if err != nil {
		t.Fatal(err)
	}
	for i, stagedPath := range writer.stagedPaths {
		finalPath := writer.finalPaths[i]
		if err := publishBackupStagedFile(stagedPath, finalPath); err != nil {
			t.Fatal(err)
		}
		writer.published = append(writer.published, backupPublishedArtifact{stagedPath: stagedPath, finalPath: finalPath})
	}
	if publishManifest {
		finalManifestPath := backupPartsManifestPath(base)
		if err := publishBackupStagedFile(manifestPath, finalManifestPath); err != nil {
			t.Fatal(err)
		}
		writer.published = append(writer.published, backupPublishedArtifact{stagedPath: manifestPath, finalPath: finalManifestPath})
	}
	if err := unlockBackupTransactionFile(writer.transaction); err != nil {
		t.Fatal(err)
	}
	if err := writer.transaction.Close(); err != nil {
		t.Fatal(err)
	}
	writer.transaction = nil
}

func writeHeaderOnlyBackupArchive(t *testing.T, header *tar.Header) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "malicious.tar.gz")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertBackupMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
