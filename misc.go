package gated

import (
	"strings"

	"github.com/hexian000/gated/slog"
)

type logWrapper struct {
	*slog.Logger
}

func (w *logWrapper) Write(p []byte) (n int, err error) {
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
