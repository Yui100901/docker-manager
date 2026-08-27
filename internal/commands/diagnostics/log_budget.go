package diagnostics

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	defaultMaxLogBytes      int64 = 16 << 20
	defaultMaxTotalLogBytes int64 = 256 << 20
	maxAllowedLogBytes      int64 = 256 << 20
	maxAllowedTotalLogBytes int64 = 4 << 30
)

var errLogReadBudgetExceeded = errors.New("log read byte budget exceeded")

// logReadBudget is shared by all log readers in one command execution. used
// only tracks bytes returned by the underlying readers; reserved prevents
// concurrent reads from exceeding total while those reads are in flight.
type logReadBudget struct {
	perContainer int64
	total        int64
	used         atomic.Int64

	mu       sync.Mutex
	reserved int64
	changed  chan struct{}
}

func newLogReadBudget(perContainer, total int64) (*logReadBudget, error) {
	if perContainer == 0 {
		perContainer = defaultMaxLogBytes
	}
	if total == 0 {
		total = defaultMaxTotalLogBytes
	}
	if perContainer < 1 || perContainer > maxAllowedLogBytes {
		return nil, fmt.Errorf("max log bytes must be between 1 and %d", maxAllowedLogBytes)
	}
	if total < 1 || total > maxAllowedTotalLogBytes {
		return nil, fmt.Errorf("max total log bytes must be between 1 and %d", maxAllowedTotalLogBytes)
	}
	return &logReadBudget{
		perContainer: perContainer,
		total:        total,
		changed:      make(chan struct{}),
	}, nil
}

func prepareLogReadBudget(perContainer, total int64, existing *logReadBudget) (*logReadBudget, error) {
	budget, err := newLogReadBudget(perContainer, total)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	return budget, nil
}

func (budget *logReadBudget) reserveUpTo(ctx context.Context, count int64) (int64, error) {
	if budget == nil || count <= 0 {
		return count, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		budget.mu.Lock()
		used := budget.used.Load()
		available := budget.total - used - budget.reserved
		if available > 0 {
			reserved := min(count, available)
			budget.reserved += reserved
			budget.mu.Unlock()
			return reserved, nil
		}
		if used >= budget.total {
			budget.mu.Unlock()
			return 0, fmt.Errorf("%w: total limit=%d used=%d requested=%d", errLogReadBudgetExceeded, budget.total, used, count)
		}
		changed := budget.changed
		budget.mu.Unlock()

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-changed:
		}
	}
}

func (budget *logReadBudget) finishReservation(reserved, read int64) {
	if budget == nil || reserved <= 0 {
		return
	}
	budget.mu.Lock()
	budget.reserved -= reserved
	budget.used.Add(read)
	close(budget.changed)
	budget.changed = make(chan struct{})
	budget.mu.Unlock()
}

func applyLogBudgetValues(perContainer, total *int64, perContainerText, totalText string) error {
	parsedPerContainer, err := parseLogByteSize("--max-log-bytes", perContainerText, maxAllowedLogBytes)
	if err != nil {
		return err
	}
	parsedTotal, err := parseLogByteSize("--max-total-log-bytes", totalText, maxAllowedTotalLogBytes)
	if err != nil {
		return err
	}
	*perContainer = parsedPerContainer
	*total = parsedTotal
	return nil
}

func parseLogByteSize(name, value string, maximum int64) (int64, error) {
	original := value
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("%s must be a positive byte size", name)
	}
	multiplier := int64(1)
	switch suffix := strings.ToUpper(value[len(value)-1:]); suffix {
	case "K":
		multiplier = 1 << 10
		value = value[:len(value)-1]
	case "M":
		multiplier = 1 << 20
		value = value[:len(value)-1]
	case "G":
		multiplier = 1 << 30
		value = value[:len(value)-1]
	case "T":
		multiplier = 1 << 40
		value = value[:len(value)-1]
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 || parsed > maximum/multiplier {
		return 0, fmt.Errorf("%s must be a positive byte size not exceeding %d bytes (K/M/G/T suffixes are supported), got %q", name, maximum, original)
	}
	return parsed * multiplier, nil
}

func readDockerLogsWithBudget(ctx context.Context, reader io.Reader, tty bool, budget *logReadBudget) (string, error) {
	stream, err := newLogBudgetReader(ctx, reader, budget)
	if err != nil {
		return "", err
	}
	if tty {
		return readAllLogString(stream)
	}
	return readMultiplexedDockerLogs(stream)
}

func readAllWithContextAndBudget(ctx context.Context, reader io.Reader, budget *logReadBudget) ([]byte, error) {
	stream, err := newLogBudgetReader(ctx, reader, budget)
	if err != nil {
		return nil, err
	}
	return readAllLogBytes(stream)
}

type logBudgetReader struct {
	ctx           context.Context
	reader        io.Reader
	budget        *logReadBudget
	containerUsed int64
}

func newLogBudgetReader(ctx context.Context, reader io.Reader, budget *logReadBudget) (*logBudgetReader, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if reader == nil {
		return nil, errors.New("log reader is nil")
	}
	return &logBudgetReader{ctx: ctx, reader: reader, budget: budget}, nil
}

func (reader *logBudgetReader) Read(p []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}

	request := int64(len(p))
	if reader.budget != nil {
		remaining := reader.budget.perContainer - reader.containerUsed
		if remaining <= 0 {
			return 0, fmt.Errorf("%w: per-container limit=%d used=%d requested=%d", errLogReadBudgetExceeded, reader.budget.perContainer, reader.containerUsed, request)
		}
		request = min(request, remaining)
	}

	reserved, err := reader.budget.reserveUpTo(reader.ctx, request)
	if err != nil {
		return 0, err
	}
	if err := reader.ctx.Err(); err != nil {
		reader.budget.finishReservation(reserved, 0)
		return 0, err
	}
	n, readErr := reader.reader.Read(p[:int(reserved)])
	if n < 0 || int64(n) > reserved {
		reader.budget.finishReservation(reserved, 0)
		return 0, fmt.Errorf("invalid log reader result: read %d bytes into a %d-byte buffer", n, reserved)
	}
	reader.budget.finishReservation(reserved, int64(n))
	reader.containerUsed += int64(n)
	if n > 0 && reader.budget != nil {
		var budgetErr error
		switch {
		case reader.containerUsed >= reader.budget.perContainer:
			budgetErr = fmt.Errorf("%w: per-container limit=%d used=%d requested=%d", errLogReadBudgetExceeded, reader.budget.perContainer, reader.containerUsed, request)
		case reader.budget.used.Load() >= reader.budget.total:
			budgetErr = fmt.Errorf("%w: total limit=%d used=%d requested=%d", errLogReadBudgetExceeded, reader.budget.total, reader.budget.used.Load(), request)
		}
		if budgetErr != nil {
			return n, errors.Join(readErr, budgetErr)
		}
	}
	return n, readErr
}

func readAllLogString(reader io.Reader) (string, error) {
	var result strings.Builder
	chunk := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(chunk)
		if n > 0 {
			if _, err := result.Write(chunk[:n]); err != nil {
				return "", err
			}
		}
		if readErr != nil {
			if isCleanLogStreamEnd(readErr) {
				return result.String(), nil
			}
			return "", readErr
		}
	}
}

func readAllLogBytes(reader io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(chunk)
		if n > 0 {
			if _, err := buf.Write(chunk[:n]); err != nil {
				return nil, err
			}
		}
		if readErr != nil {
			if isCleanLogStreamEnd(readErr) {
				return buf.Bytes(), nil
			}
			return nil, readErr
		}
	}
}

func isCleanLogStreamEnd(err error) bool {
	return !errors.Is(err, errLogReadBudgetExceeded) &&
		(errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))
}

const dockerLogHeaderSize = 8

type dockerLogStream struct {
	data     strings.Builder
	complete int
}

type dockerLogOutput struct {
	stdout dockerLogStream
	stderr dockerLogStream
	system dockerLogStream
	// Headers are enough to reconstruct the raw stream on a format error;
	// payload bytes remain single-copy in their destination buffers.
	headers []byte
}

func readMultiplexedDockerLogs(reader io.Reader) (string, error) {
	var output dockerLogOutput
	chunk := make([]byte, 32*1024)
	for {
		var header [dockerLogHeaderSize]byte
		n, err := io.ReadFull(reader, header[:])
		if err != nil {
			if isCleanLogStreamEnd(err) {
				return output.combined(), nil
			}
			return "", err
		}

		stream := header[0]
		if stream > 3 {
			return output.rawFallback(reader, header[:n])
		}
		// Stream the declared frame size through a fixed buffer. A forged large
		// size therefore cannot trigger a frame-sized allocation.
		frameSize := int64(binary.BigEndian.Uint32(header[4:]))
		target := output.target(stream)
		if err := copyDockerLogFrame(&target.data, reader, frameSize, chunk); err != nil {
			if isCleanLogStreamEnd(err) {
				return output.combined(), nil
			}
			return "", err
		}
		target.complete = target.data.Len()
		output.headers = append(output.headers, header[:]...)
		if stream == 3 {
			return output.rawFallback(reader, nil)
		}
	}
}

func copyDockerLogFrame(dst *strings.Builder, reader io.Reader, size int64, chunk []byte) error {
	for size > 0 {
		readSize := min(size, int64(len(chunk)))
		n, err := io.ReadFull(reader, chunk[:int(readSize)])
		if n > 0 {
			if _, writeErr := dst.Write(chunk[:n]); writeErr != nil {
				return writeErr
			}
			size -= int64(n)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (output *dockerLogOutput) target(stream byte) *dockerLogStream {
	switch stream {
	case 0, 1:
		return &output.stdout
	case 2:
		return &output.stderr
	default:
		return &output.system
	}
}

func (output *dockerLogOutput) combined() string {
	stdoutLen := output.stdout.complete
	stderrLen := output.stderr.complete
	if stdoutLen == 0 {
		return output.stderr.data.String()[:stderrLen]
	}
	if stderrLen == 0 {
		return output.stdout.data.String()[:stdoutLen]
	}

	stderr := output.stderr.data.String()[:stderrLen]
	if output.stdout.data.Len() == stdoutLen && output.stdout.data.Cap()-stdoutLen >= stderrLen {
		_, _ = output.stdout.data.WriteString(stderr)
		return output.stdout.data.String()
	}

	// Allocate the exact final capacity when the stdout builder cannot append
	// in place. This also excludes any truncated stdout frame without copying
	// through an intermediate []byte.
	var result strings.Builder
	result.Grow(stdoutLen + stderrLen)
	_, _ = result.WriteString(output.stdout.data.String()[:stdoutLen])
	_, _ = result.WriteString(stderr)
	return result.String()
}

func (output *dockerLogOutput) rawFallback(reader io.Reader, currentHeader []byte) (string, error) {
	var raw strings.Builder
	var stdoutOffset, stderrOffset, systemOffset int
	for offset := 0; offset < len(output.headers); offset += dockerLogHeaderSize {
		header := output.headers[offset : offset+dockerLogHeaderSize]
		stream := header[0]
		size := int(binary.BigEndian.Uint32(header[4:]))
		start := stdoutOffset
		switch stream {
		case 0, 1:
			stdoutOffset += size
		case 2:
			start = stderrOffset
			stderrOffset += size
		default:
			start = systemOffset
			systemOffset += size
		}
		_, _ = raw.Write(header)
		payload := output.target(stream).data.String()[start : start+size]
		_, _ = raw.WriteString(payload)
	}
	_, _ = raw.Write(currentHeader)

	output.stdout = dockerLogStream{}
	output.stderr = dockerLogStream{}
	output.system = dockerLogStream{}
	output.headers = nil

	chunk := make([]byte, 32*1024)
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			_, _ = raw.Write(chunk[:n])
		}
		if err != nil {
			if isCleanLogStreamEnd(err) {
				return raw.String(), nil
			}
			return "", err
		}
	}
}
