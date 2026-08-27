package diagnostics

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

type logBudgetPropagationService struct {
	containers []container.Summary
	reader     func(context.Context, string) io.ReadCloser
}

func (service *logBudgetPropagationService) ListContainers(context.Context, bool) ([]container.Summary, error) {
	if service.containers != nil {
		return append([]container.Summary(nil), service.containers...), nil
	}
	return []container.Summary{{ID: "demo", Names: []string{"/demo"}, Image: "demo:latest", State: "running"}}, nil
}

func (service *logBudgetPropagationService) InspectContainer(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{
		ID:         "demo",
		Name:       "/demo",
		State:      &container.State{Status: container.StateRunning},
		Config:     &container.Config{Image: "demo:latest", Tty: true},
		HostConfig: &container.HostConfig{LogConfig: container.LogConfig{Type: "json-file"}},
	}, nil
}

func (service *logBudgetPropagationService) ContainerLogs(ctx context.Context, id string, _ mobyclient.ContainerLogsOptions) (io.ReadCloser, error) {
	return service.reader(ctx, id), nil
}

type terminalLogErrorReader struct {
	err error
}

func (reader *terminalLogErrorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

type waitThenLogReader struct {
	wait   <-chan struct{}
	reader io.Reader
}

func (reader *waitThenLogReader) Read(p []byte) (int, error) {
	<-reader.wait
	return reader.reader.Read(p)
}

type contextOrEOFLogReader struct {
	ctx     context.Context
	entered chan struct{}
	once    sync.Once
}

func (reader *contextOrEOFLogReader) Read([]byte) (int, error) {
	reader.once.Do(func() { close(reader.entered) })
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-reader.ctx.Done():
		return 0, reader.ctx.Err()
	case <-timer.C:
		return 0, io.EOF
	}
}

func replaceLogBudgetPropagationFactories(t *testing.T, service *logBudgetPropagationService) {
	t.Helper()
	previousHealth := newHealthDockerService
	previousLogs := newLogsScanDockerService
	newHealthDockerService = func() (healthDockerService, error) { return service, nil }
	newLogsScanDockerService = func() (logsScanDockerService, error) { return service, nil }
	t.Cleanup(func() {
		newHealthDockerService = previousHealth
		newLogsScanDockerService = previousLogs
	})
}

func budgetPropagationOptions() (HealthOptions, LogsScanOptions, ReportAllOptions) {
	return HealthOptions{
		LogTail:          10,
		Keywords:         []string{"error"},
		MaxLogBytes:      4,
		MaxTotalLogBytes: 100,
	}, LogsScanOptions{
		Tail:             10,
		Keywords:         []string{"error"},
		MaxLogBytes:      4,
		MaxTotalLogBytes: 100,
	}, ReportAllOptions{
		Include:          []string{reportAllKindLogs},
		LogTail:          10,
		LogKeywords:      []string{"error"},
		MaxLogBytes:      4,
		MaxTotalLogBytes: 100,
	}
}

func TestLogBudgetExceededPropagatesFromReportCalls(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, HealthOptions, LogsScanOptions, ReportAllOptions) error
	}{
		{
			name: "health",
			run: func(ctx context.Context, health HealthOptions, _ LogsScanOptions, _ ReportAllOptions) error {
				_, err := runHealthReport(ctx, health)
				return err
			},
		},
		{
			name: "logs",
			run: func(ctx context.Context, _ HealthOptions, logs LogsScanOptions, _ ReportAllOptions) error {
				_, err := runLogsScan(ctx, logs)
				return err
			},
		},
		{
			name: "report all",
			run: func(ctx context.Context, _ HealthOptions, _ LogsScanOptions, all ReportAllOptions) error {
				_, err := runReportAll(ctx, all)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &logBudgetPropagationService{reader: func(context.Context, string) io.ReadCloser {
				return io.NopCloser(strings.NewReader("error\n"))
			}}
			replaceLogBudgetPropagationFactories(t, service)
			health, logs, all := budgetPropagationOptions()
			err := test.run(context.Background(), health, logs, all)
			if !errors.Is(err, errLogReadBudgetExceeded) {
				t.Fatalf("report error = %v, want log budget failure", err)
			}
		})
	}
}

func TestLogBudgetExceededPropagatesFromCommands(t *testing.T) {
	tests := []struct {
		name string
		new  func() *cobra.Command
		args []string
	}{
		{name: "health", new: NewHealthCommand, args: []string{"--max-log-bytes", "4", "--max-total-log-bytes", "100"}},
		{name: "logs", new: NewLogsScanCommand, args: []string{"--max-log-bytes", "4", "--max-total-log-bytes", "100"}},
		{name: "report all", new: NewReportAllCommand, args: []string{"--include", "logs", "--max-log-bytes", "4", "--max-total-log-bytes", "100"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &logBudgetPropagationService{reader: func(context.Context, string) io.ReadCloser {
				return io.NopCloser(strings.NewReader("error\n"))
			}}
			replaceLogBudgetPropagationFactories(t, service)
			cmd := test.new()
			cmd.SetArgs(test.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.ExecuteContext(context.Background())
			if !errors.Is(err, errLogReadBudgetExceeded) {
				t.Fatalf("command error = %v, want log budget failure", err)
			}
		})
	}
}

func TestOrdinaryLogReadErrorsRemainReportFindings(t *testing.T) {
	readErr := errors.New("log transport failed")
	service := &logBudgetPropagationService{reader: func(context.Context, string) io.ReadCloser {
		return io.NopCloser(&terminalLogErrorReader{err: readErr})
	}}
	replaceLogBudgetPropagationFactories(t, service)

	healthOpts, logsOpts, allOpts := budgetPropagationOptions()
	healthOpts.MaxLogBytes = 100
	logsOpts.MaxLogBytes = 100
	allOpts.MaxLogBytes = 100

	health, err := runHealthReport(context.Background(), healthOpts)
	if err != nil {
		t.Fatalf("runHealthReport() error = %v, want report finding", err)
	}
	if !hasHealthIssue(health, "logs-unavailable") {
		t.Fatalf("health issues = %#v, want logs-unavailable", health.Issues)
	}

	logs, err := runLogsScan(context.Background(), logsOpts)
	if err != nil {
		t.Fatalf("runLogsScan() error = %v, want report error item", err)
	}
	if logs.Summary.Errors != 1 || len(logs.Containers) != 1 || logs.Containers[0].ErrorType != "read-failed" {
		t.Fatalf("logs report = %#v, want one read-failed item", logs)
	}

	all, err := runReportAll(context.Background(), allOpts)
	if err != nil {
		t.Fatalf("runReportAll() error = %v, want successful section with error item", err)
	}
	if len(all.Sections) != 1 || all.Sections[0].Status != "ok" || all.Logs == nil || all.Logs.Summary.Errors != 1 {
		t.Fatalf("report all = %#v, want successful logs section with one error item", all)
	}
}

func TestLogBudgetFailureDoesNotIncludeInternalSiblingCancellation(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, LogsScanOptions, ReportAllOptions) error
	}{
		{
			name: "logs",
			run: func(ctx context.Context, logs LogsScanOptions, _ ReportAllOptions) error {
				_, err := runLogsScan(ctx, logs)
				return err
			},
		},
		{
			name: "report all",
			run: func(ctx context.Context, _ LogsScanOptions, all ReportAllOptions) error {
				_, err := runReportAll(ctx, all)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			siblingEntered := make(chan struct{})
			service := &logBudgetPropagationService{
				containers: []container.Summary{
					{ID: "budget", Names: []string{"/budget"}, State: "running"},
					{ID: "sibling", Names: []string{"/sibling"}, State: "running"},
				},
				reader: func(ctx context.Context, id string) io.ReadCloser {
					if id == "budget" {
						return io.NopCloser(&waitThenLogReader{wait: siblingEntered, reader: strings.NewReader("error\n")})
					}
					return io.NopCloser(&contextOrEOFLogReader{ctx: ctx, entered: siblingEntered})
				},
			}
			replaceLogBudgetPropagationFactories(t, service)
			_, logs, all := budgetPropagationOptions()
			err := test.run(context.Background(), logs, all)
			if !errors.Is(err, errLogReadBudgetExceeded) {
				t.Fatalf("report error = %v, want log budget failure", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("report error = %v, contains internal sibling cancellation", err)
			}
		})
	}
}
