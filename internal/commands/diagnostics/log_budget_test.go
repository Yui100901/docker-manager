package diagnostics

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type trackingLogReader struct {
	data         []byte
	offset       int
	maxRequested int
	reads        int
}

func (reader *trackingLogReader) Read(p []byte) (int, error) {
	reader.reads++
	reader.maxRequested = max(reader.maxRequested, len(p))
	if reader.offset == len(reader.data) {
		return 0, io.EOF
	}
	n := copy(p, reader.data[reader.offset:])
	reader.offset += n
	return n, nil
}

type eofWithDataReader struct {
	data []byte
}

func (reader *eofWithDataReader) Read(p []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, reader.data)
	reader.data = reader.data[n:]
	if len(reader.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

type blockingLogReader struct {
	entered chan struct{}
	release chan struct{}
	data    []byte
	err     error
	once    sync.Once
}

func (reader *blockingLogReader) Read(p []byte) (int, error) {
	reader.once.Do(func() { close(reader.entered) })
	<-reader.release
	n := copy(p, reader.data)
	reader.data = reader.data[n:]
	if len(reader.data) == 0 {
		return n, reader.err
	}
	return n, nil
}

func dockerLogTestFrame(stream byte, payload []byte) []byte {
	header := make([]byte, dockerLogHeaderSize)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	return append(header, payload...)
}

func TestParseLogByteSize(t *testing.T) {
	tests := []struct {
		value string
		want  int64
	}{
		{value: "1", want: 1},
		{value: "2K", want: 2 << 10},
		{value: "3m", want: 3 << 20},
		{value: "4G", want: 4 << 30},
	}
	for _, tt := range tests {
		got, err := parseLogByteSize("limit", tt.value, maxAllowedTotalLogBytes)
		if err != nil {
			t.Fatalf("parseLogByteSize(%q) error = %v", tt.value, err)
		}
		if got != tt.want {
			t.Fatalf("parseLogByteSize(%q) = %d, want %d", tt.value, got, tt.want)
		}
	}
	for _, value := range []string{"", "0", "-1", "1.5M", "5G"} {
		if _, err := parseLogByteSize("limit", value, 4<<30); err == nil {
			t.Fatalf("parseLogByteSize(%q) error = nil, want rejection", value)
		}
	}
}

func TestReadAllWithBudgetRejectsPerContainerOverflow(t *testing.T) {
	budget, err := newLogReadBudget(4, 100)
	if err != nil {
		t.Fatal(err)
	}
	reader := &trackingLogReader{data: []byte("12345")}
	_, err = readAllWithContextAndBudget(context.Background(), reader, budget)
	if !errors.Is(err, errLogReadBudgetExceeded) || !strings.Contains(err.Error(), "per-container") {
		t.Fatalf("read error = %v, want per-container budget failure", err)
	}
	if reader.offset != 4 || reader.maxRequested != 4 {
		t.Fatalf("reader consumed=%d max-request=%d, want exactly 4/4", reader.offset, reader.maxRequested)
	}
	if got := budget.used.Load(); got != 4 {
		t.Fatalf("total used = %d, want actual read count 4", got)
	}
}

func TestReadAllWithBudgetSharesTotalAcrossReaders(t *testing.T) {
	budget, err := newLogReadBudget(10, 5)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readAllWithContextAndBudget(context.Background(), strings.NewReader("123"), budget)
	if err != nil || string(data) != "123" {
		t.Fatalf("first read = %q, %v", data, err)
	}
	second := &trackingLogReader{data: []byte("456")}
	_, err = readAllWithContextAndBudget(context.Background(), second, budget)
	if !errors.Is(err, errLogReadBudgetExceeded) || !strings.Contains(err.Error(), "total") {
		t.Fatalf("second read error = %v, want total budget failure", err)
	}
	if second.offset != 2 {
		t.Fatalf("second reader consumed = %d, want remaining total budget 2", second.offset)
	}
	if got := budget.used.Load(); got != 5 {
		t.Fatalf("total used = %d, want actual read count 5", got)
	}
}

func TestReadAllWithBudgetRejectsExactLimits(t *testing.T) {
	tests := []struct {
		name         string
		perContainer int64
		total        int64
		wantDetail   string
	}{
		{name: "per-container", perContainer: 4, total: 10, wantDetail: "per-container"},
		{name: "total", perContainer: 10, total: 4, wantDetail: "total"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			budget, err := newLogReadBudget(test.perContainer, test.total)
			if err != nil {
				t.Fatal(err)
			}
			data, err := readAllWithContextAndBudget(context.Background(), &eofWithDataReader{data: []byte("1234")}, budget)
			if !errors.Is(err, errLogReadBudgetExceeded) || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("read = %q, %v; want exact-limit %s failure", data, err, test.wantDetail)
			}
			if got := budget.used.Load(); got != 4 {
				t.Fatalf("total used = %d, want 4", got)
			}
		})
	}

	t.Run("total across readers", func(t *testing.T) {
		budget, err := newLogReadBudget(4, 4)
		if err != nil {
			t.Fatal(err)
		}
		first, err := readAllWithContextAndBudget(context.Background(), &eofWithDataReader{data: []byte("12")}, budget)
		if err != nil || string(first) != "12" {
			t.Fatalf("first read = %q, %v; want success", first, err)
		}
		second, err := readAllWithContextAndBudget(context.Background(), &eofWithDataReader{data: []byte("34")}, budget)
		if !errors.Is(err, errLogReadBudgetExceeded) || !strings.Contains(err.Error(), "total") {
			t.Fatalf("second read = %q, %v; want exact total failure", second, err)
		}
		if got := budget.used.Load(); got != 4 {
			t.Fatalf("total used = %d, want 4", got)
		}
	})
}

func TestLogReadBudgetWaitsForBlockingReservationAndHonorsContext(t *testing.T) {
	budget, err := newLogReadBudget(8, 8)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockingLogReader{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		err:     io.EOF,
	}
	firstResult := make(chan error, 1)
	go func() {
		_, readErr := readAllWithContextAndBudget(context.Background(), blocked, budget)
		firstResult <- readErr
	}()
	<-blocked.entered

	if got := budget.used.Load(); got != 0 {
		t.Fatalf("used while zero-byte EOF read is blocked = %d, want 0 actual bytes", got)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = budget.reserveUpTo(waitCtx, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent reservation error = %v, want context deadline while waiting", err)
	}

	close(blocked.release)
	if err := <-firstResult; err != nil {
		t.Fatalf("blocking EOF read error = %v", err)
	}
	data, err := readAllWithContextAndBudget(context.Background(), &eofWithDataReader{data: []byte("ok")}, budget)
	if err != nil || string(data) != "ok" {
		t.Fatalf("read after released reservation = %q, %v; want ok", data, err)
	}
}

func TestLogReadBudgetReleasesUnusedShortReadReservation(t *testing.T) {
	budget, err := newLogReadBudget(8, 9)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockingLogReader{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		data:    []byte("ab"),
		err:     io.EOF,
	}
	firstResult := make(chan error, 1)
	go func() {
		_, readErr := readAllWithContextAndBudget(context.Background(), blocked, budget)
		firstResult <- readErr
	}()
	<-blocked.entered
	if got := budget.used.Load(); got != 0 {
		t.Fatalf("used while short read is blocked = %d, want 0 actual bytes", got)
	}
	close(blocked.release)
	if err := <-firstResult; err != nil {
		t.Fatalf("short read error = %v", err)
	}

	data, err := readAllWithContextAndBudget(context.Background(), &eofWithDataReader{data: []byte("cdefgh")}, budget)
	if err != nil || string(data) != "cdefgh" {
		t.Fatalf("read from released budget = %q, %v; want cdefgh", data, err)
	}
	if got := budget.used.Load(); got != 8 {
		t.Fatalf("total used = %d, want 8 actual bytes", got)
	}
}

func TestLogReadBudgetConcurrentReservationsStayWithinLimit(t *testing.T) {
	budget, err := newLogReadBudget(2, 8)
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := readAllWithContextAndBudget(context.Background(), &eofWithDataReader{data: []byte("x")}, budget); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, errLogReadBudgetExceeded) {
				t.Errorf("read error = %v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 7 || budget.used.Load() != 8 {
		t.Fatalf("successes=%d used=%d, want 7 successful reads and one exact-limit failure consuming 8 bytes", successes.Load(), budget.used.Load())
	}
}

func TestReadDockerLogsStreamsMultiplexedFramesInBoundedChunks(t *testing.T) {
	stdout := bytes.Repeat([]byte("o"), 96*1024)
	stderr := bytes.Repeat([]byte("e"), 48*1024)
	data := append(dockerLogTestFrame(2, stderr), dockerLogTestFrame(1, stdout)...)
	reader := &trackingLogReader{data: data}
	budget, err := newLogReadBudget(int64(len(data)+1), int64(len(data)+1))
	if err != nil {
		t.Fatal(err)
	}

	got, err := readDockerLogsWithBudget(context.Background(), reader, false, budget)
	if err != nil {
		t.Fatalf("readDockerLogsWithBudget() error = %v", err)
	}
	want := string(stdout) + string(stderr)
	if got != want {
		t.Fatalf("decoded output length=%d, want stdout then stderr length=%d", len(got), len(want))
	}
	if reader.maxRequested > 32*1024 {
		t.Fatalf("largest underlying read = %d, want at most 32 KiB", reader.maxRequested)
	}
	if gotUsed := budget.used.Load(); gotUsed != int64(len(data)) {
		t.Fatalf("total used = %d, want actual multiplexed bytes %d", gotUsed, len(data))
	}
}

func TestReadDockerLogsHugeDeclaredFrameStopsAtBudgetWithoutOverread(t *testing.T) {
	header := make([]byte, dockerLogHeaderSize)
	header[0] = 1
	binary.BigEndian.PutUint32(header[4:], 1<<30)
	data := append(header, bytes.Repeat([]byte("x"), 128)...)
	reader := &trackingLogReader{data: data}
	budget, err := newLogReadBudget(64, 64)
	if err != nil {
		t.Fatal(err)
	}

	_, err = readDockerLogsWithBudget(context.Background(), reader, false, budget)
	if !errors.Is(err, errLogReadBudgetExceeded) {
		t.Fatalf("readDockerLogsWithBudget() error = %v, want byte budget failure", err)
	}
	if reader.offset != 64 || reader.maxRequested > 56 {
		t.Fatalf("reader consumed=%d max-request=%d, want exactly 64 bytes and no payload read above 56", reader.offset, reader.maxRequested)
	}
	if got := budget.used.Load(); got != 64 {
		t.Fatalf("total used = %d, want 64 actual bytes", got)
	}
}

func TestReadDockerLogsInvalidFormatFallsBackToRawStream(t *testing.T) {
	valid := dockerLogTestFrame(1, []byte("before\n"))
	invalid := append([]byte{9, 0, 0, 0, 0, 0, 0, 3}, []byte("raw")...)
	data := append(valid, invalid...)
	budget, err := newLogReadBudget(int64(len(data)+1), int64(len(data)+1))
	if err != nil {
		t.Fatal(err)
	}

	got, err := readDockerLogsWithBudget(context.Background(), bytes.NewReader(data), false, budget)
	if err != nil {
		t.Fatalf("readDockerLogsWithBudget() error = %v", err)
	}
	if got != string(data) {
		t.Fatalf("invalid multiplexed output = %q, want original raw stream %q", got, data)
	}
}

func TestReadDockerLogsInvalidFormatNearBudgetPreservesRawStream(t *testing.T) {
	stdout := bytes.Repeat([]byte("o"), 512*1024)
	stderr := bytes.Repeat([]byte("e"), 256*1024)
	invalid := append([]byte{9, 0, 0, 0, 0, 0, 0, 4}, []byte("tail")...)
	data := append(dockerLogTestFrame(2, stderr), dockerLogTestFrame(1, stdout)...)
	data = append(data, invalid...)
	reader := &eofWithDataReader{data: data}
	budget, err := newLogReadBudget(int64(len(data)+1), int64(len(data)+1))
	if err != nil {
		t.Fatal(err)
	}

	got, err := readDockerLogsWithBudget(context.Background(), reader, false, budget)
	if err != nil {
		t.Fatalf("readDockerLogsWithBudget() error = %v", err)
	}
	if got != string(data) {
		t.Fatalf("raw fallback length=%d, want original length=%d", len(got), len(data))
	}
	if used := budget.used.Load(); used != int64(len(data)) {
		t.Fatalf("total used = %d, want exact stream size %d", used, len(data))
	}
}

func TestReadDockerLogsTruncatedFrameDropsPartialPayload(t *testing.T) {
	complete := dockerLogTestFrame(1, []byte("complete\n"))
	header := make([]byte, dockerLogHeaderSize)
	header[0] = 2
	binary.BigEndian.PutUint32(header[4:], 10)
	data := append(append(complete, header...), []byte("part")...)

	got, err := readDockerLogsWithBudget(context.Background(), bytes.NewReader(data), false, nil)
	if err != nil {
		t.Fatalf("readDockerLogsWithBudget() error = %v", err)
	}
	if got != "complete\n" {
		t.Fatalf("truncated multiplexed output = %q, want only complete frames", got)
	}
}

func TestReadDockerLogsStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelingLogReader{data: []byte("some log data"), cancel: cancel}
	budget, err := newLogReadBudget(100, 100)
	if err != nil {
		t.Fatal(err)
	}

	_, err = readDockerLogsWithBudget(ctx, reader, true, budget)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readDockerLogsWithBudget() error = %v, want context.Canceled", err)
	}
	if reader.offset == 0 || int64(reader.offset) != budget.used.Load() {
		t.Fatalf("reader consumed=%d total used=%d, want matching non-zero actual bytes", reader.offset, budget.used.Load())
	}
}

type cancelingLogReader struct {
	data   []byte
	offset int
	cancel context.CancelFunc
}

func (reader *cancelingLogReader) Read(p []byte) (int, error) {
	if reader.offset > 0 {
		return 0, io.EOF
	}
	n := copy(p, reader.data)
	reader.offset += n
	reader.cancel()
	return n, nil
}

func TestLogCommandsExposeByteBudgetFlags(t *testing.T) {
	commands := map[string]struct {
		lookup func(string) bool
	}{
		"health": {lookup: func(name string) bool { return NewHealthCommand().Flags().Lookup(name) != nil }},
		"logs":   {lookup: func(name string) bool { return NewLogsScanCommand().Flags().Lookup(name) != nil }},
		"all":    {lookup: func(name string) bool { return NewReportAllCommand().Flags().Lookup(name) != nil }},
	}
	for command, check := range commands {
		for _, flag := range []string{"max-log-bytes", "max-total-log-bytes"} {
			if !check.lookup(flag) {
				t.Fatalf("%s missing --%s", command, flag)
			}
		}
	}
}
