// Package logrotate provides a size-capped rolling file writer for the paxd
// process log. Rotation renames paxd.log -> paxd.log.1 -> ... -> paxd.log.N
// and drops the oldest backup.
package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	defaultMaxBytes   = 20 << 20
	defaultMaxBackups = 3
)

type Options struct {
	Path       string
	MaxBytes   int64
	MaxBackups int
}

func (o Options) withDefaults() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultMaxBytes
	}
	if o.MaxBackups <= 0 {
		o.MaxBackups = defaultMaxBackups
	}
	return o
}

type Writer struct {
	mu   sync.Mutex
	opts Options
	file *os.File
	size int64
}

func New(opts Options) (*Writer, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("logrotate: path is required")
	}
	opts = opts.withDefaults()
	w := &Writer{opts: opts}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.opts.MaxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *Writer) open() error {
	if err := os.MkdirAll(filepath.Dir(w.opts.Path), 0o700); err != nil {
		return fmt.Errorf("logrotate: create log directory: %w", err)
	}
	file, err := os.OpenFile(w.opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logrotate: open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("logrotate: stat log file: %w", err)
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *Writer) rotate() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return fmt.Errorf("logrotate: close before rotate: %w", err)
		}
		w.file = nil
	}
	_ = os.Remove(w.backupPath(w.opts.MaxBackups))
	for i := w.opts.MaxBackups - 1; i >= 1; i-- {
		from := w.backupPath(i)
		if _, err := os.Stat(from); err == nil {
			if err := os.Rename(from, w.backupPath(i+1)); err != nil {
				return fmt.Errorf("logrotate: shift backup %d: %w", i, err)
			}
		}
	}
	if _, err := os.Stat(w.opts.Path); err == nil {
		if err := os.Rename(w.opts.Path, w.backupPath(1)); err != nil {
			return fmt.Errorf("logrotate: archive current log: %w", err)
		}
	}
	return w.open()
}

func (w *Writer) backupPath(n int) string {
	return fmt.Sprintf("%s.%d", w.opts.Path, n)
}
