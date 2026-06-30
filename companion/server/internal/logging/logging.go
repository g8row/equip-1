// Package logging configures the application's slog logger.
package logging

import (
	"log/slog"
	"os"
)

// Setup builds a slog.Logger writing to stderr, installs it as the slog
// default, and returns it. The default level is INFO.
//
// Logging used to also append to a file under the capture directory, but
// that file had no rotation, grew unbounded, and both companion-api and
// companion-net wrote to the same path concurrently. systemd's
// StandardOutput=journal already captures stderr with proper retention —
// use `journalctl -u companion-api` / `-u companion-net` instead.
func Setup() *slog.Logger {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
