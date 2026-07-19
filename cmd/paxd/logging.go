package main

import (
	"io"
	"log"
	"os"

	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/logrotate"
)

// setupLogging routes the standard logger to a size-capped rolling file.
// When stderr is a terminal the log is mirrored there for foreground runs.
func setupLogging(cfg config.DaemonConfig) (func(), error) {
	if cfg.LogFile == "" {
		return func() {}, nil
	}
	writer, err := logrotate.New(logrotate.Options{
		Path:       cfg.LogFile,
		MaxBytes:   int64(cfg.LogMaxSizeMB) << 20,
		MaxBackups: cfg.LogMaxBackups,
	})
	if err != nil {
		return func() {}, err
	}
	if stderrIsTerminal() {
		log.SetOutput(io.MultiWriter(os.Stderr, writer))
	} else {
		log.SetOutput(writer)
	}
	return func() { _ = writer.Close() }, nil
}

func stderrIsTerminal() bool {
	info, err := os.Stderr.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
