// Package logging configures the application's slog logger, mirroring the
// behaviour of the Python api/logging_setup.py (stderr + append-to-file,
// default INFO level).
package logging

import (
	"io"
	"log/slog"
	"os"
)

// Setup builds a slog.Logger writing to both stderr and the given log file
// (opened in append mode). It installs the logger as the slog default and
// returns it. If the log file cannot be opened, logging falls back to stderr
// only. The default level is INFO.
func Setup(logFile string) *slog.Logger {
	var w io.Writer = os.Stderr

	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			w = io.MultiWriter(os.Stderr, f)
		}
	}

	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
