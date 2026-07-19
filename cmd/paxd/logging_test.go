package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pax-beehive/paxd/internal/config"
)

func TestSetupLoggingWritesToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paxd.log")
	closeLog, err := setupLogging(config.DaemonConfig{LogFile: path, LogMaxSizeMB: 1, LogMaxBackups: 1})
	if err != nil {
		t.Fatalf("setupLogging() error = %v", err)
	}
	defer func() {
		log.SetOutput(os.Stderr)
		closeLog()
	}()
	log.Printf("[paxd] logging smoke test")
	closeLog()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "logging smoke test") {
		t.Fatalf("log file missing entry, content = %q", data)
	}
}

func TestSetupLoggingEmptyPathIsNoop(t *testing.T) {
	closeLog, err := setupLogging(config.DaemonConfig{})
	if err != nil {
		t.Fatalf("setupLogging() error = %v", err)
	}
	closeLog()
}
