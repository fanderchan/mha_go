package obs

import (
	"log/slog"
	"os"
	"strings"
)

type Logger = slog.Logger

// NewLogger creates a logger that writes text (logfmt) to stderr.
func NewLogger(level string) *Logger {
	return NewLoggerWithFormat(level, "text")
}

// NewLoggerWithFormat creates a logger writing to stderr.
// format is "json" or "text" (default); any other value is treated as "text".
func NewLoggerWithFormat(level, format string) *Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var handler slog.Handler
	if strings.ToLower(strings.TrimSpace(format)) == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

func parseLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
