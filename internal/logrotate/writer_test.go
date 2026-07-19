package logrotate

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteAppendsToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paxd.log")
	w, err := New(Options{Path: path, MaxBytes: 1024, MaxBackups: 2})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := w.Write([]byte("world\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "hello\nworld\n" {
		t.Fatalf("log content = %q, want %q", data, "hello\nworld\n")
	}
}

func TestRotateShiftsBackupsAndDropsOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paxd.log")
	// MaxBytes 10: 每条 7 字节的记录第二次写入就触发轮转。
	w, err := New(Options{Path: path, MaxBytes: 10, MaxBackups: 2})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer w.Close()
	for _, line := range []string{"line-A\n", "line-B\n", "line-C\n", "line-D\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write(%q) error = %v", line, err)
		}
	}
	// 期望：paxd.log = line-D, paxd.log.1 = line-C, paxd.log.2 = line-B, line-A 被丢弃。
	assertFile := func(p string, want string) {
		t.Helper()
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", p, err)
		}
		if string(data) != want {
			t.Fatalf("%s content = %q, want %q", p, data, want)
		}
	}
	assertFile(path, "line-D\n")
	assertFile(path+".1", "line-C\n")
	assertFile(path+".2", "line-B\n")
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("paxd.log.3 should not exist, stat err = %v", err)
	}
}

func TestNewResumesExistingFileSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paxd.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 8), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := New(Options{Path: path, MaxBytes: 10, MaxBackups: 1})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer w.Close()
	// 已有 8 字节，再写 6 字节应先轮转再写。
	if _, err := w.Write([]byte("fresh\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fresh\n" {
		t.Fatalf("log content = %q, want %q", data, "fresh\n")
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("backup should exist: %v", err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paxd.log")
	w, err := New(Options{Path: path, MaxBytes: 512, MaxBackups: 3})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer w.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if _, err := w.Write([]byte("concurrent line\n")); err != nil {
					t.Errorf("Write() error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	w, err := New(Options{Path: filepath.Join(dir, "paxd.log")})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer w.Close()
	if w.opts.MaxBytes != 20<<20 {
		t.Fatalf("default MaxBytes = %d, want %d", w.opts.MaxBytes, 20<<20)
	}
	if w.opts.MaxBackups != 3 {
		t.Fatalf("default MaxBackups = %d, want 3", w.opts.MaxBackups)
	}
}
