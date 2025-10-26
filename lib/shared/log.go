package shared

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
)

var (
	Logger  *slog.Logger
	logOnce sync.Once
)

func InitLogger(filep string, level string) (err error) {
	logOnce.Do(func() {
		var l slog.Level
		l, err = toLevel(level)
		if err != nil {
			return
		}

		var writer io.Writer = os.Stderr
		if filep != "" {
			writer, err = os.OpenFile(filep, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return
			}
		}
		initialize(writer, l)
	})
	return
}

func initialize(writer io.Writer, level slog.Level) {
	Logger = slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: level}))
}

func InitTestLogger(tb testing.TB) {
	logOnce.Do(func() {
		writer := &testWriter{tb: tb}
		initialize(writer, slog.LevelDebug)
	})
}

type testWriter struct {
	tb testing.TB
}

func (w testWriter) Write(p []byte) (n int, err error) {
	w.tb.Log(string(p))
	return len(p), nil
}

func toLevel(rawValue string) (slog.Level, error) {
	value, found := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"error":   slog.LevelError,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
	}[strings.ToLower(rawValue)]
	if !found {
		return 0, fmt.Errorf("no such logging level: %s", rawValue)
	}
	return value, nil
}

func IsDebugEnabled() bool {
	return Logger.Enabled(context.TODO(), slog.LevelDebug)
}
