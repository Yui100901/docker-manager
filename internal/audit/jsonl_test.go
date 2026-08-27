package audit

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEvent(sequence uint64, message string) Event {
	return Event{
		Schema:    SchemaVersion,
		Type:      EventCommandFinish,
		Time:      time.Date(2026, time.August, 27, 1, 2, 3, 0, time.UTC).Format(time.RFC3339Nano),
		RunID:     "run-id",
		Sequence:  sequence,
		Operation: "health",
		Endpoint:  Endpoint{Scheme: "unix", ID: "hmac-sha256:1234"},
		Error:     &ErrorInfo{Class: "error", ID: "hmac-sha256:5678", Message: message},
	}
}

func TestOpenFileSinkPersistsKeyAndAppendsJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "events.jsonl")
	sink, err := OpenFileSink(FileOptions{Path: path, MaxBytes: 4096, MaxFiles: 2, MaxEventBytes: 1024})
	if err != nil {
		t.Fatalf("OpenFileSink() error = %v", err)
	}
	key := sink.IdentifierKey()
	if len(key) != identifierKeyBytes {
		t.Fatalf("key length = %d, want %d", len(key), identifierKeyBytes)
	}
	if err := sink.Append(context.Background(), testEvent(1, "ok")); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	keyData, err := os.ReadFile(path + ".key")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := hex.DecodeString(strings.TrimSpace(string(keyData))); err != nil || string(got) != string(key) {
		t.Fatalf("persisted key = %q, want %x (err=%v)", keyData, key, err)
	}
	for _, candidate := range []string{path, path + ".key", path + ".lock"} {
		info, statErr := os.Stat(candidate)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Errorf("%s mode = %o, want 600", candidate, info.Mode().Perm())
		}
	}

	reopened, err := OpenFileSink(FileOptions{Path: path, MaxBytes: 4096, MaxFiles: 2, MaxEventBytes: 1024})
	if err != nil {
		t.Fatalf("reopen sink error = %v", err)
	}
	defer reopened.Close()
	if string(reopened.IdentifierKey()) != string(key) {
		t.Fatal("reopened sink generated a different key")
	}
	if err := reopened.Append(context.Background(), testEvent(2, "second")); err != nil {
		t.Fatal(err)
	}
	assertJSONLSequences(t, path, []uint64{1, 2})
}

func TestFileSinkConcurrentAppendsRemainWholeJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink, err := OpenFileSink(FileOptions{Path: path, MaxBytes: 1 << 20, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	const writers = 12
	const perWriter = 20
	var group sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		writer := writer
		group.Add(1)
		go func() {
			defer group.Done()
			for index := 0; index < perWriter; index++ {
				sequence := uint64(writer*perWriter + index + 1)
				if err := sink.Append(context.Background(), testEvent(sequence, strings.Repeat("x", 200))); err != nil {
					t.Errorf("Append(%d) error = %v", sequence, err)
				}
			}
		}()
	}
	group.Wait()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("line %d is invalid JSON: %v", count+1, err)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if want := writers * perWriter; count != want {
		t.Fatalf("line count = %d, want %d", count, want)
	}
}

func TestFileSinkRotatesWholeEventsAndRejectsRotationLinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink, err := OpenFileSink(FileOptions{Path: path, MaxBytes: 1200, MaxFiles: 8, MaxEventBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 8; index++ {
		if err := sink.Append(context.Background(), testEvent(uint64(index), strings.Repeat("x", 200))); err != nil {
			t.Fatalf("Append(%d) error = %v", index, err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	files := []string{path}
	for index := 1; index <= 8; index++ {
		files = append(files, rotatedAuditPath(path, index))
	}
	seen := 0
	for _, filePath := range files {
		data, readErr := os.ReadFile(filePath)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, line := range auditJSONLines(data) {
			var event Event
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("rotation line invalid JSON: %v", err)
			}
			seen++
		}
	}
	if seen != 8 {
		t.Fatalf("rotated event count = %d, want 8", seen)
	}

	// A hostile rotation path must fail closed instead of unlinking the target.
	// Use a fresh file so the hostile .1 entry is guaranteed to be consulted by
	// the first rotation on every platform.
	hostileDir := filepath.Join(t.TempDir(), "hostile")
	if err := os.MkdirAll(hostileDir, 0700); err != nil {
		t.Fatal(err)
	}
	hostilePath := filepath.Join(hostileDir, "events.jsonl")
	linkTarget := filepath.Join(hostileDir, "outside")
	if err := os.WriteFile(linkTarget, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	hostileSink, err := OpenFileSink(FileOptions{Path: hostilePath, MaxBytes: 1200, MaxFiles: 8, MaxEventBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer hostileSink.Close()
	if err := hostileSink.Append(context.Background(), testEvent(90, strings.Repeat("x", 200))); err != nil {
		t.Fatal(err)
	}
	rotationLink := hostilePath + ".1"
	if err := os.Symlink(linkTarget, rotationLink); err != nil {
		t.Skipf("symlink unavailable on %s: %v", runtime.GOOS, err)
	}
	var appendErr error
	for sequence := uint64(91); sequence < 100; sequence++ {
		appendErr = hostileSink.Append(context.Background(), testEvent(sequence, strings.Repeat("y", 200)))
		if appendErr != nil {
			break
		}
	}
	if appendErr == nil {
		t.Fatal("Append() followed a rotation symlink")
	}
	if data, err := os.ReadFile(linkTarget); err != nil || string(data) != "keep" {
		t.Fatalf("symlink target changed: data=%q err=%v", data, err)
	}
}

func TestOpenFileSinkRejectsUnsafePathsAndInvalidKeys(t *testing.T) {
	if _, err := OpenFileSink(FileOptions{Path: "-"}); err == nil {
		t.Fatal("OpenFileSink() accepted stdout path")
	}
	dir := t.TempDir()
	if _, err := OpenFileSink(FileOptions{Path: dir}); err == nil {
		t.Fatal("OpenFileSink() accepted directory path")
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".key", []byte("not-hex\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileSink(FileOptions{Path: path}); err == nil {
		t.Fatal("OpenFileSink() accepted invalid key")
	}
	if err := os.WriteFile(path+".key", []byte(strings.Repeat("00", identifierKeyBytes)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sink, err := OpenFileSink(FileOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Append(context.Background(), testEvent(1, "ok")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileSinkHonorsCanceledContextAndEventLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink, err := OpenFileSink(FileOptions{Path: path, MaxBytes: 2048, MaxEventBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Append(ctx, testEvent(1, "ok")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Append(canceled) error = %v, want context.Canceled", err)
	}
	if err := sink.Append(context.Background(), testEvent(2, strings.Repeat("x", maxErrorText))); err == nil {
		t.Fatal("Append() accepted event above MaxEventBytes")
	}
}

func assertJSONLSequences(t *testing.T, path string, want []uint64) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var got []uint64
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		got = append(got, event.Sequence)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("sequences = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("sequences = %#v, want %#v", got, want)
		}
	}
}
