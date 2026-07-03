package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"docker-manager/internal/appconfig"
)

const defaultConfigPath = appconfig.DefaultPath
const configEnvName = appconfig.EnvName

type appConfig = appconfig.Config

type outputOptions struct {
	Verbose bool
	Quiet   bool
	JSON    bool
}

type jsonLogWriter struct {
	out io.Writer
	mu  sync.Mutex
}

func loadAppConfig(path string) (appConfig, error) {
	return appconfig.Load(path)
}

func resolveConfigPath(path string, flagChanged bool) string {
	return appconfig.ResolvePath(path, flagChanged)
}

func configureLogging(opts outputOptions) {
	switch {
	case opts.Quiet:
		log.SetOutput(io.Discard)
		log.SetFlags(0)
	case opts.JSON:
		log.SetOutput(&jsonLogWriter{out: os.Stderr})
		log.SetFlags(0)
	case opts.Verbose:
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	default:
		log.SetOutput(os.Stderr)
		log.SetFlags(0)
	}
}

func (w *jsonLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		data, err := json.Marshal(map[string]string{
			"level":   "info",
			"time":    time.Now().Format(time.RFC3339),
			"message": line,
		})
		if err != nil {
			return 0, err
		}
		if _, err := fmt.Fprintln(w.out, string(data)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func writeCommandError(out io.Writer, err error, opts outputOptions) {
	if err == nil {
		return
	}
	err = displayCommandError(err)
	if opts.JSON {
		data, marshalErr := json.Marshal(map[string]string{
			"level": "error",
			"error": err.Error(),
		})
		if marshalErr == nil {
			_, _ = fmt.Fprintln(out, string(data))
			return
		}
	}
	_, _ = fmt.Fprintf(out, "Error: %v\n", err)
}

func isCommandCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

func displayCommandError(err error) error {
	if isCommandCanceled(err) {
		return errors.New("操作已取消")
	}
	return err
}
