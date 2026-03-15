package logger

// core/logger/logger.go
//
// Structured logger for the Money Printer system.
// Uses Go's standard log/slog (available Go 1.21+) with a custom handler
// that writes JSON to a log file and human-readable text to stdout.
//
// Usage anywhere in the codebase:
//   log := logger.Log
//   log.Info("something happened", "key", value)
//   log.Warn("watch out", "err", err)
//   log.Error("fatal thing", "err", err)
//   log.Debug("verbose detail", "pair", pair)

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Log is the global logger — import and use directly.
var Log *slog.Logger

var (
	initOnce sync.Once
	logFile  *os.File
)

// Init initialises the global logger.
// filename: path to the log file (e.g. "logs/money_printer.log")
// If the file can't be opened, logs go to stdout only.
// Safe to call multiple times — only initialises once.
func Init(filename string) {
	initOnce.Do(func() {
		// Ensure log directory exists
		if dir := filepath.Dir(filename); dir != "" {
			_ = os.MkdirAll(dir, 0755)
		}

		// Open log file (append mode)
		var fileWriter io.Writer
		f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fileWriter = io.Discard
		} else {
			logFile = f
			fileWriter = f
		}

		// Multi-writer: JSON → file, text → stdout
		textHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level:     slog.LevelDebug,
			AddSource: false,
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				// Format timestamps as human-readable in stdout
				if a.Key == slog.TimeKey {
					a.Value = slog.StringValue(time.Now().UTC().Format("15:04:05.000"))
				}
				return a
			},
		})

		jsonHandler := slog.NewJSONHandler(fileWriter, &slog.HandlerOptions{
			Level:     slog.LevelDebug,
			AddSource: false,
		})

		Log = slog.New(&multiHandler{
			handlers: []slog.Handler{textHandler, jsonHandler},
		})

		// Set as default so any code using slog.Info() also routes here
		slog.SetDefault(Log)
	})
}

// Close flushes and closes the log file. Call on shutdown.
func Close() {
	if logFile != nil {
		_ = logFile.Sync()
		_ = logFile.Close()
	}
}

// ── multiHandler fans out to multiple slog.Handler instances ─────────────────

type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: handlers}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: handlers}
}
