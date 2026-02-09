package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"gopkg.in/natefinch/lumberjack.v2"
)

func SetupLogger(logFile, level string, isTUI bool) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	// Always write to file with rotation
	logDir := filepath.Dir(logFile)
	if logDir != "" && logDir != "." {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, fmt.Errorf("create log dir: %w", err)
		}
	}

	fileWriter = &lumberjack.Logger{
		Filename:   logFile,
		MaxSize:    100, // MB
		MaxBackups: 5,
		MaxAge:     28, // days
		Compress:   false,
	}

	fileHandler := tint.NewHandler(fileWriter, &tint.Options{
		Level:      lvl,
		TimeFormat: time.RFC3339,
		NoColor:    true,
	})

	// If TUI mode, only write to file; otherwise also write to stderr
	if isTUI {
		return slog.New(fileHandler), nil
	}

	noColor := !isatty.IsTerminal(os.Stderr.Fd()) || os.Getenv("NO_COLOR") != ""
	stderrHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:      lvl,
		TimeFormat: time.TimeOnly,
		NoColor:    noColor,
	})

	multiHandler := &MultiHandler{
		handlers: []slog.Handler{fileHandler, stderrHandler},
	}

	return slog.New(multiHandler), nil
}

type MultiHandler struct {
	handlers []slog.Handler
}

func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *MultiHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, h := range m.handlers {
		if err := h.Handle(ctx, record); err != nil {
			return err
		}
	}
	return nil
}

func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: newHandlers}
}

func (m *MultiHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: newHandlers}
}

var fileWriter *lumberjack.Logger

// CloseFile closes the log file writer if it's a lumberjack logger
func CloseFile() error {
	if fileWriter != nil {
		return fileWriter.Close()
	}
	return nil
}
