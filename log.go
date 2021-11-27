package gated

import (
	"log"
	"strings"

	"github.com/hexian000/gated/slog"
)

func init() {
	log.SetPrefix("")
	log.SetFlags(0)
	log.SetOutput(&logWrapper{slog.Default()})
}

type yamuxLogWrapper struct {
	*slog.Logger
}

func (w *yamuxLogWrapper) Write(p []byte) (n int, err error) {
	const calldepth = 4
	raw := string(p)
	if msg := strings.TrimPrefix(raw, "[ERR] "); len(msg) != len(raw) {
		w.Output(calldepth, slog.LevelError, msg)
	} else if msg := strings.TrimPrefix(raw, "[WARN] "); len(msg) != len(raw) {
		w.Output(calldepth, slog.LevelWarning, msg)
	} else {
		w.Output(calldepth, slog.LevelError, raw)
	}
	return len(p), nil
}

func newYamuxLogger() *log.Logger {
	return log.New(&yamuxLogWrapper{slog.Default()}, "", 0)
}

type logWrapper struct {
	*slog.Logger
}

func (w *logWrapper) Write(p []byte) (n int, err error) {
	const calldepth = 4
	w.Output(calldepth, slog.LevelError, string(p))
	return len(p), nil
}

func newHTTPLogger() *log.Logger {
	return log.New(&logWrapper{slog.Default()}, "", 0)
}
