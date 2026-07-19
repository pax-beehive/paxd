# 滚动日志 / stderr 捕获 / 诊断快照 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** paxd 自管滚动日志文件；harness stderr 不再丢弃（进主日志 + 失败时落 slot status）；local API 暴露 `/v1/diagnostics` 运行时诊断快照。

**Architecture:** 新包 `internal/logrotate` 提供 size-based 轮转 writer，`cmd/paxd` 启动时接管 `log` 输出。`internal/runtime` 新增 `stderrTail`（逐行写日志 + 环形缓冲），三处 `io.Copy(io.Discard, proc.Stderr())` 全部替换；slot 进程退出时尾部经 `Exit.Details["stderr_tail"]` 由 supervisor 落进 `ACPSlotStatus.LastErrorMessage`。诊断走既有 control query 模式：新 query type + `DiagnosticsProvider`（由 `daemon.runtimeSupervisors` 实现，聚合 producer stats / write-behind stats / supervisor snapshot），localapi 加一条 GET 路由。

**Tech Stack:** Go 1.22，stdlib only（无新依赖）。测试用 `go test` + testify（已有）。

## Global Constraints

- 不新增外部依赖（spec 决策：自写轮转器，不引入 lumberjack）。
- 日志默认值：`~/.paxd/logs/paxd.log`，20MB，3 个备份（最坏 80MB）。
- stderr 环形缓冲 8192 字节；写入 status 的尾部截断到 2048 字节。
- 所有 Go 文件遵循仓库现有风格：stdlib `log.Printf`、`[paxd]` 前缀、key=value 日志字段。
- 每个 task 结束跑 `go build ./... && go test ./<改动包>/...`，全部通过后 commit。
- commit message 末尾加 `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`。

---

### Task 1: `internal/logrotate` 轮转 writer

**Files:**
- Create: `internal/logrotate/writer.go`
- Test: `internal/logrotate/writer_test.go`

**Interfaces:**
- Consumes: 无（stdlib only）。
- Produces: `logrotate.New(opts Options) (*Writer, error)`；`Options{Path string; MaxBytes int64; MaxBackups int}`；`(*Writer).Write([]byte) (int, error)`（并发安全）；`(*Writer).Close() error`。Task 3 依赖这些签名。

- [ ] **Step 1: Write the failing test**

`internal/logrotate/writer_test.go`:

```go
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
	// MaxBytes 10: 每条 8 字节的记录第二次写入就触发轮转。
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
	// 写 4 条、容量 10 字节：每次轮转发生在当前文件已有 1 条再来 1 条时。
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/logrotate/ -v`
Expected: FAIL（package 不存在 / `New` undefined）

- [ ] **Step 3: Write minimal implementation**

`internal/logrotate/writer.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/logrotate/ -v -race`
Expected: PASS（全部 5 个测试）

- [ ] **Step 5: Commit**

```bash
git add internal/logrotate/
git commit -m "feat: add size-capped rolling log writer

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: 日志配置项

**Files:**
- Modify: `internal/config/config.go`（`DaemonConfig` 结构体 + `DefaultConfig`）
- Test: `internal/config/config_test.go`（追加）

**Interfaces:**
- Produces: `DaemonConfig.LogFile string`、`DaemonConfig.LogMaxSizeMB int`、`DaemonConfig.LogMaxBackups int`（yaml: `log_file` / `log_max_size_mb` / `log_max_backups`）。Task 3、7 依赖。

- [ ] **Step 1: Write the failing test**（追加到 `internal/config/config_test.go`）

```go
func TestDefaultConfigLogSettings(t *testing.T) {
	cfg := DefaultConfig()
	home, _ := os.UserHomeDir()
	if got, want := cfg.Daemon.LogFile, filepath.Join(home, ".paxd", "logs", "paxd.log"); got != want {
		t.Fatalf("Daemon.LogFile = %q, want %q", got, want)
	}
	if cfg.Daemon.LogMaxSizeMB != 20 {
		t.Fatalf("Daemon.LogMaxSizeMB = %d, want 20", cfg.Daemon.LogMaxSizeMB)
	}
	if cfg.Daemon.LogMaxBackups != 3 {
		t.Fatalf("Daemon.LogMaxBackups = %d, want 3", cfg.Daemon.LogMaxBackups)
	}
}
```

（若 config_test.go 未 import `os`/`filepath`，一并补上。）

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestDefaultConfigLogSettings -v`
Expected: FAIL（字段不存在，编译错误）

- [ ] **Step 3: Write minimal implementation**

`DaemonConfig` 增加字段（放在 `LogLevel` 之后）：

```go
	LogLevel          string        `yaml:"log_level"`          // debug, info, warn, error
	LogFile           string        `yaml:"log_file"`           // rolling log path (default ~/.paxd/logs/paxd.log, empty keeps stderr only)
	LogMaxSizeMB      int           `yaml:"log_max_size_mb"`    // rotate threshold in MB (default 20)
	LogMaxBackups     int           `yaml:"log_max_backups"`    // rotated files to keep (default 3)
```

`DefaultConfig()` 的 `Daemon:` 块增加：

```go
			LogFile:       filepath.Join(home, ".paxd", "logs", "paxd.log"),
			LogMaxSizeMB:  20,
			LogMaxBackups: 3,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS（含既有测试）

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat: add rolling log config to daemon section

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: `paxd run` 接管日志输出 + launchd plist 去掉 StandardOutPath

**Files:**
- Create: `cmd/paxd/logging.go`
- Modify: `cmd/paxd/main.go`（`cmdRun` 开头，`loadRuntimeConfig` 之后）
- Modify: `cmd/paxd/main.go` launchd plist 模板（~line 790-810，删除 `StandardOutPath` 键值对）
- Test: `cmd/paxd/logging_test.go`（新建）、`cmd/paxd/service_test.go`（修改断言）

**Interfaces:**
- Consumes: `logrotate.New`（Task 1）、`cfg.Daemon.LogFile/LogMaxSizeMB/LogMaxBackups`（Task 2）。
- Produces: `setupLogging(cfg config.DaemonConfig) (close func(), err error)`；`stderrIsTerminal() bool`。

- [ ] **Step 1: Write the failing test**

`cmd/paxd/logging_test.go`:

```go
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
```

`cmd/paxd/service_test.go`：将断言 launchd plist 含 `<string>/Users/dev/.paxd/logs/paxd.log</string>` 的行（~line 90）改为：

```go
	assert.NotContains(t, plist, "StandardOutPath")
	assert.Contains(t, plist, "<string>/Users/dev/.paxd/logs/paxd.error.log</string>")
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/paxd/ -run 'TestSetupLogging|TestGenerateLaunchd' -v`
Expected: FAIL（`setupLogging` undefined；plist 仍含 StandardOutPath）

- [ ] **Step 3: Write minimal implementation**

`cmd/paxd/logging.go`:

```go
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
```

`cmd/paxd/main.go` `cmdRun` 中，`loadRuntimeConfig` 成功之后、第一条 `log.Printf` 之前插入：

```go
	closeLog, err := setupLogging(cfg.Daemon)
	if err != nil {
		log.Printf("[paxd] rolling log unavailable, keeping stderr only: %v", err)
	}
	defer closeLog()
```

launchd plist 模板：删除

```
    <key>StandardOutPath</key>
    <string>%s</string>
```

两行，并从对应 `fmt.Sprintf` 参数列表中删掉 `xmlEscape(filepath.Join(logDir, "paxd.log"))`。`paxd.error.log` 的 `StandardErrorPath` 保留。检查 `cmd/paxd/main.go:624` 附近的 `tail -f` 提示路径仍指向 `~/.paxd/logs/paxd.log`（rotator 写同一路径，提示无需改）。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/paxd/ -v`
Expected: PASS（含 service_test 全部）

- [ ] **Step 5: Commit**

```bash
git add cmd/paxd/
git commit -m "feat: route paxd log through rolling file writer

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: `stderrTail` 组件

**Files:**
- Create: `internal/runtime/stderr_tail.go`
- Test: `internal/runtime/stderr_tail_test.go`

**Interfaces:**
- Produces: `newStderrTail(limit int) *stderrTail`（limit ≤ 0 时用 8192）；`(*stderrTail).Consume(r io.Reader, logPrefix string)`（阻塞直到 EOF/错误；每行 `log.Printf("%s %s", logPrefix, line)` 并入环）；`(*stderrTail).Tail(max int) string`（返回最后 ≤ max 字节，max ≤ 0 表示全部）。Task 5 依赖。
- 常量：`stderrTailLimit = 8192`、`stderrStatusTailLimit = 2048`。

- [ ] **Step 1: Write the failing test**

`internal/runtime/stderr_tail_test.go`:

```go
package runtime

import (
	"strings"
	"testing"
)

func TestStderrTailKeepsLastBytes(t *testing.T) {
	tail := newStderrTail(16)
	tail.Consume(strings.NewReader("first line\nsecond line\nthird\n"), "[test]")
	got := tail.Tail(0)
	if len(got) > 16 {
		t.Fatalf("tail length = %d, want <= 16", len(got))
	}
	if !strings.Contains(got, "third") {
		t.Fatalf("tail = %q, want to contain most recent line", got)
	}
	if strings.Contains(got, "first line") {
		t.Fatalf("tail = %q, oldest line should be evicted", got)
	}
}

func TestStderrTailTruncatesOversizedLine(t *testing.T) {
	tail := newStderrTail(8)
	tail.Consume(strings.NewReader(strings.Repeat("x", 100)+"END\n"), "[test]")
	got := tail.Tail(0)
	if len(got) > 8 {
		t.Fatalf("tail length = %d, want <= 8", len(got))
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "END") {
		t.Fatalf("tail = %q, want suffix of oversized line", got)
	}
}

func TestStderrTailMaxParameter(t *testing.T) {
	tail := newStderrTail(64)
	tail.Consume(strings.NewReader("abcdefghij\nklmnopqrst\n"), "[test]")
	got := tail.Tail(5)
	if len(got) > 5 {
		t.Fatalf("Tail(5) length = %d, want <= 5", len(got))
	}
}

func TestStderrTailEmptyReader(t *testing.T) {
	tail := newStderrTail(64)
	tail.Consume(strings.NewReader(""), "[test]")
	if got := tail.Tail(0); got != "" {
		t.Fatalf("Tail() = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestStderrTail -v`
Expected: FAIL（`newStderrTail` undefined）

- [ ] **Step 3: Write minimal implementation**

`internal/runtime/stderr_tail.go`:

```go
package runtime

import (
	"bufio"
	"io"
	"log"
	"sync"
)

const (
	stderrTailLimit       = 8192
	stderrStatusTailLimit = 2048
)

// stderrTail drains a child process stderr stream: every line is forwarded to
// the process log with a routing prefix, and a bounded ring of the most recent
// bytes is kept for failure reporting.
type stderrTail struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func newStderrTail(limit int) *stderrTail {
	if limit <= 0 {
		limit = stderrTailLimit
	}
	return &stderrTail{limit: limit}
}

func (t *stderrTail) Consume(r io.Reader, logPrefix string) {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := trimLineDelimiter(line); len(trimmed) > 0 {
			log.Printf("%s %s", logPrefix, trimmed)
			t.append(trimmed)
		}
		if err != nil {
			return
		}
	}
}

func (t *stderrTail) append(line []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(line) >= t.limit {
		t.buf = append(t.buf[:0], line[len(line)-t.limit:]...)
		return
	}
	t.buf = append(t.buf, line...)
	t.buf = append(t.buf, '\n')
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
}

func (t *stderrTail) Tail(max int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.buf
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return string(out)
}
```

注意：`trimLineDelimiter` 已存在于 `agent_tunnel_session.go`，同包直接复用。`ReadBytes` 超长行会一次性返回大 slice——`append` 已按 limit 截断，内存受 `bufio.Reader` 单行大小影响有限（行由子进程决定，接受）。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/runtime/ -run TestStderrTail -v -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/stderr_tail.go internal/runtime/stderr_tail_test.go
git commit -m "feat: add stderr tail ring buffer with log forwarding

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: 接线 stderr 捕获（三处替换 + status 落库）

**Files:**
- Modify: `internal/runtime/acp_slot.go`（构造器、`Start`、新增 `StderrTail`）
- Modify: `internal/runtime/acp_pool.go`（`ACPSlotSession.Run` 的 `process_ended` 退出）
- Modify: `internal/runtime/agent_tunnel_session.go:188`
- Modify: `internal/runtime/persistent_acp_process.go:235`
- Modify: `internal/supervisor/supervisor.go`（`acpSlotStatusWriter`）
- Test: `internal/runtime/acp_slot_test.go`、`internal/supervisor/supervisor_test.go`（各追加一个用例）

**Interfaces:**
- Consumes: `newStderrTail` / `Tail(max)`（Task 4）、`Exit.WithDetail`（已存在于 `internal/runtime/types.go:36`）。
- Produces: `(*ACPSlot).StderrTail(max int) string`；`Exit.Details["stderr_tail"]` 约定（supervisor 侧消费）。

- [ ] **Step 1: Write the failing tests**

`internal/runtime/acp_slot_test.go` 追加（该文件已有 fake process/runner 基建，沿用其命名；若既有 fake 名不同，以现存为准适配调用处，断言不变）：

```go
func TestACPSlotCapturesStderrTail(t *testing.T) {
	// 使用现有测试基建启动一个 slot，fake process 的 Stderr() 返回
	// strings.NewReader("boom: harness crashed\n")。
	// 进程退出后：
	//   slot.StderrTail(0) 应包含 "boom: harness crashed"
}
```

（具体样板照抄该文件中现有 `TestACPSlot...` 用例的 fake 构造方式，替换 stderr reader 与断言。）

`internal/supervisor/supervisor_test.go` 追加：

```go
func TestACPSlotStatusWriterAppendsStderrTail(t *testing.T) {
	store := &fakeACPSlotStore{} // 该文件已有 fake ACPSlotStore；若命名不同沿用现有
	write := statusWrite{
		Phase: PhaseStopped,
		Exit: runtimes.TransientExit("process_ended", "acp slot process ended").
			WithDetail("stderr_tail", "boom: harness crashed"),
		At: time.Now(),
	}
	err := acpSlotStatusWriter(store)(context.Background(), runtimes.ACPSlotSpec{
		SlotID: "slot-1", ConnectionID: "conn-1",
	}, write)
	if err != nil {
		t.Fatalf("writer error = %v", err)
	}
	got := store.lastUpdate.LastErrorMessage
	if !strings.Contains(got, "acp slot process ended") || !strings.Contains(got, "boom: harness crashed") {
		t.Fatalf("LastErrorMessage = %q, want exit message and stderr tail", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/runtime/ -run TestACPSlotCapturesStderrTail -v && go test ./internal/supervisor/ -run TestACPSlotStatusWriterAppendsStderrTail -v`
Expected: FAIL（`StderrTail` undefined；LastErrorMessage 不含 tail）

- [ ] **Step 3: Write minimal implementation**

`acp_slot.go`：

1. `ACPSlot` 结构体加字段 `stderr *stderrTail`；`NewACPSlot` 中初始化 `stderr: newStderrTail(stderrTailLimit),`。
2. `Start` 中把 `go io.Copy(io.Discard, proc.Stderr())` 替换为：

```go
	go s.stderr.Consume(proc.Stderr(), fmt.Sprintf(
		"[harness stderr] connection_id=%s slot_id=%s", s.spec.ConnectionID, s.spec.SlotID))
```

3. 新增方法：

```go
func (s *ACPSlot) StderrTail(max int) string {
	return s.stderr.Tail(max)
}
```

`acp_pool.go` `ACPSlotSession.Run` 中 `case <-slot.Done():` 改为：

```go
	case <-slot.Done():
		exit := TransientExit("process_ended", "acp slot process ended")
		if tail := slot.StderrTail(stderrStatusTailLimit); tail != "" {
			exit = exit.WithDetail("stderr_tail", tail)
		}
		return exit
```

`agent_tunnel_session.go:188` 替换为：

```go
	go newStderrTail(stderrTailLimit).Consume(proc.Stderr(), fmt.Sprintf(
		"[harness stderr] connection_id=%s", s.spec.ConnectionID))
```

`persistent_acp_process.go:235` 同样替换（该文件上下文里可用的标识字段以 spec 中的 ConnectionID 为准，照当前函数签名取值）。

`internal/supervisor/supervisor.go` `acpSlotStatusWriter` 中，把 `LastErrorMessage: safeExitMessage(write.Exit),` 改为 `LastErrorMessage: acpSlotErrorMessage(write.Exit),` 并新增：

```go
func acpSlotErrorMessage(exit runtimes.Exit) string {
	message := safeExitMessage(exit)
	tail := exit.Details["stderr_tail"]
	if tail == "" {
		return message
	}
	if message == "" {
		return "stderr tail:\n" + tail
	}
	return message + "\nstderr tail:\n" + tail
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/runtime/ ./internal/supervisor/ -race`
Expected: PASS（全部既有 + 新增）

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/ internal/supervisor/
git commit -m "feat: capture harness stderr into log and slot status

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: transport producer stats 埋点

**Files:**
- Create: `internal/runtime/transport_stats.go`
- Test: `internal/runtime/transport_stats_test.go`
- Modify: `internal/daemon/runtime_supervisors.go`（`Configure` 中包装 engineFactory）

**Interfaces:**
- Consumes: `ReliableEngineFactory`（`agent_tunnel_session.go:49`）、`reliablemq.ProducerStats`。
- Produces: `NewTransportStatsTracker(inner ReliableEngineFactory) *TransportStatsTracker`；`(*TransportStatsTracker).Stats() map[string]reliablemq.ProducerStats`；实现 `ReliableEngineFactory` 与 `Close(context.Context) error` 透传。Task 7 依赖 `Stats()`。

- [ ] **Step 1: Write the failing test**

`internal/runtime/transport_stats_test.go`:

```go
package runtime

import (
	"context"
	"testing"

	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/pax-beehive/paxkit/reliablemq/sqlstore"
	// 若 runtime 包既有测试已有内存 store helper，直接复用；
	// 否则用 sqlstore + sqlite in-memory（repo 测试已有此模式，照抄）。
)

func TestTransportStatsTrackerRecordsProducers(t *testing.T) {
	store := newTestDurableStore(t) // 复用/仿照 acp 相关测试中构造 reliablemq.DurableStore 的 helper
	inner := ReliableEngineFromStore(store)
	tracker := NewTransportStatsTracker(inner)

	if got := tracker.Stats(); len(got) != 0 {
		t.Fatalf("Stats() before Producer = %v, want empty", got)
	}
	_, err := tracker.Producer(context.Background(), "queue-1", reliablemq.StreamACP)
	if err != nil {
		t.Fatalf("Producer() error = %v", err)
	}
	stats := tracker.Stats()
	if _, ok := stats["queue-1"]; !ok {
		t.Fatalf("Stats() = %v, want entry for queue-1", stats)
	}
}
```

（`newTestDurableStore`：查 `internal/runtime` 或 `internal/integration` 既有测试中如何构造 `reliablemq.DurableStore`（`sqlstore.NewSQLite` + `:memory:`），提炼/复制为本文件的 helper。）

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestTransportStatsTracker -v`
Expected: FAIL（`NewTransportStatsTracker` undefined）

- [ ] **Step 3: Write minimal implementation**

`internal/runtime/transport_stats.go`:

```go
package runtime

import (
	"context"
	"sync"

	"github.com/pax-beehive/paxkit/reliablemq"
)

// TransportStatsTracker wraps a ReliableEngineFactory and remembers every
// producer it hands out, so diagnostics can report per-queue transport stats.
type TransportStatsTracker struct {
	inner ReliableEngineFactory

	mu        sync.Mutex
	producers map[string]*reliablemq.Producer
}

func NewTransportStatsTracker(inner ReliableEngineFactory) *TransportStatsTracker {
	return &TransportStatsTracker{
		inner:     inner,
		producers: make(map[string]*reliablemq.Producer),
	}
}

func (t *TransportStatsTracker) Producer(
	ctx context.Context,
	queueID string,
	stream reliablemq.Stream,
) (*reliablemq.Producer, error) {
	producer, err := t.inner.Producer(ctx, queueID, stream)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.producers[queueID] = producer
	t.mu.Unlock()
	return producer, nil
}

func (t *TransportStatsTracker) NewReliableEngine(
	producer *reliablemq.Producer,
	dispatcher reliablemq.Dispatcher,
) ReliableEngine {
	return t.inner.NewReliableEngine(producer, dispatcher)
}

func (t *TransportStatsTracker) Close(ctx context.Context) error {
	if closer, ok := t.inner.(interface{ Close(context.Context) error }); ok {
		return closer.Close(ctx)
	}
	return nil
}

func (t *TransportStatsTracker) Stats() map[string]reliablemq.ProducerStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]reliablemq.ProducerStats, len(t.producers))
	for queueID, producer := range t.producers {
		out[queueID] = producer.Stats()
	}
	return out
}
```

`internal/daemon/runtime_supervisors.go` `Configure` 中，在 `engineFactory := runtimes.ReliableEngineFromStoreWithProducerConfig(...)` 之后插入：

```go
	tracker := runtimes.NewTransportStatsTracker(engineFactory)
	s.transportStats = tracker
```

并将后续使用 `engineFactory` 的两处（`transportProducers` closer 检测、`agentFactory` deps）改用 `tracker`。`runtimeSupervisors` 结构体加字段：

```go
	transportStats *runtimes.TransportStatsTracker
```

（closer 检测 `if closer, ok := ...` 对 tracker 依然成立，因为 tracker 透传 Close。）

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/runtime/ ./internal/daemon/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/transport_stats.go internal/runtime/transport_stats_test.go internal/daemon/runtime_supervisors.go
git commit -m "feat: track per-queue transport producer stats

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: control `diagnostics.get` 查询 + provider

**Files:**
- Modify: `internal/control/types.go`（QueryType、Query、QueryResult、新类型）
- Modify: `internal/control/validation.go`（接受新 query type；照 `QueryStatusGet` 的既有校验分支样式加一条）
- Modify: `internal/control/service.go`（`ServiceOptions.Diagnostics`、`handleDiagnosticsQuery`）
- Modify: `internal/daemonstore/repository.go`（`ListACPSlotStatuses`）
- Modify: `internal/daemon/runtime_supervisors.go` + `internal/daemon/bootstrap.go`（实现并注入 provider）
- Test: `internal/control/service_test.go`、`internal/daemonstore/store_test.go`（各追加）

**Interfaces:**
- Consumes: `TransportStatsTracker.Stats()`（Task 6）、`ProducerWriteBehindStats`（`transportFlusher.Stats()` 已存在）、`config.DaemonConfig.LogFile`（Task 2）。
- Produces（control 包，含 json tag）:

```go
const QueryDiagnosticsGet QueryType = "diagnostics.get"

type GetDiagnosticsQuery struct{}

type DiagnosticsProvider interface {
	RuntimeDiagnostics(ctx context.Context) RuntimeDiagnostics
}

type RuntimeDiagnostics struct {
	PaxdVersion          string                       `json:"paxd_version,omitempty"`
	StartedAt            *time.Time                   `json:"started_at,omitempty"`
	LogFile              *LogFileInfo                 `json:"log_file,omitempty"`
	TransportQueues      []TransportQueueDiagnostics  `json:"transport_queues,omitempty"`
	TransportWriteBehind *TransportWriteBehindStats   `json:"transport_write_behind,omitempty"`
}

type LogFileInfo struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

type TransportQueueDiagnostics struct {
	QueueID      string `json:"queue_id"`
	Bound        bool   `json:"bound"`
	Tail         int64  `json:"tail"`
	AckedThrough int64  `json:"acked_through"`
	NextToSend   int64  `json:"next_to_send"`
	Unacked      int64  `json:"unacked"`
	LastError    string `json:"last_error,omitempty"`
}

type TransportWriteBehindStats struct {
	DirtyFrames              int    `json:"dirty_frames"`
	DirtyPatches             int    `json:"dirty_patches"`
	DirtyBytes               int64  `json:"dirty_bytes"`
	Degraded                 bool   `json:"degraded"`
	ConsecutiveFlushFailures int    `json:"consecutive_flush_failures"`
	LastFlushError           string `json:"last_flush_error,omitempty"`
}

type ACPSlotStatusInfo struct {
	SlotID           string `json:"slot_id"`
	ConnectionID     string `json:"connection_id"`
	Ordinal          int    `json:"ordinal"`
	Phase            string `json:"phase"`
	FailureClass     string `json:"failure_class,omitempty"`
	LastErrorCode    string `json:"last_error_code,omitempty"`
	LastErrorMessage string `json:"last_error_message,omitempty"`
	UpdatedAt        string `json:"updated_at,omitempty"`
}

type ACPSlotStatusSource interface {
	ListACPSlotStatuses(ctx context.Context) ([]ACPSlotStatusInfo, error)
}

type DiagnosticsView struct {
	RuntimeDiagnostics
	Remotes          []RemoteStatusView  `json:"remotes,omitempty"`
	AgentConnections []AgentStatusView   `json:"agent_connections,omitempty"`
	ACPSlots         []ACPSlotStatusInfo `json:"acp_slots,omitempty"`
}
```

- `Query` 加 `GetDiagnostics *GetDiagnosticsQuery \`json:"get_diagnostics,omitempty"\``；`QueryResult` 加 `Diagnostics *DiagnosticsView \`json:"diagnostics,omitempty"\``；`ServiceOptions` 加 `Diagnostics DiagnosticsProvider`。

- [ ] **Step 1: Write the failing tests**

`internal/control/service_test.go` 追加（fake store 命名沿用该文件现有的）：

```go
type fakeDiagnosticsProvider struct{}

func (fakeDiagnosticsProvider) RuntimeDiagnostics(ctx context.Context) RuntimeDiagnostics {
	return RuntimeDiagnostics{
		PaxdVersion: "test-version",
		TransportQueues: []TransportQueueDiagnostics{
			{QueueID: "queue-1", Tail: 10, AckedThrough: 7, Unacked: 3},
		},
	}
}

func TestHandleDiagnosticsQuery(t *testing.T) {
	// 用该文件现有方式构造带真实 daemonstore 的 service（参考 TestHandleStatusQuery 的做法），
	// ServiceOptions 额外传 Diagnostics: fakeDiagnosticsProvider{}。
	result, err := service.HandleQuery(ctx, Source{Kind: SourceLocal}, Query{
		Type:           QueryDiagnosticsGet,
		GetDiagnostics: &GetDiagnosticsQuery{},
	})
	if err != nil {
		t.Fatalf("HandleQuery() error = %v", err)
	}
	if result.Error != nil {
		t.Fatalf("result.Error = %v", result.Error)
	}
	if result.Diagnostics == nil {
		t.Fatal("result.Diagnostics is nil")
	}
	if result.Diagnostics.PaxdVersion != "test-version" {
		t.Fatalf("PaxdVersion = %q", result.Diagnostics.PaxdVersion)
	}
	if len(result.Diagnostics.TransportQueues) != 1 || result.Diagnostics.TransportQueues[0].Unacked != 3 {
		t.Fatalf("TransportQueues = %+v", result.Diagnostics.TransportQueues)
	}
}
```

`internal/daemonstore/store_test.go` 追加：

```go
func TestListACPSlotStatuses(t *testing.T) {
	// 用该文件现有方式 open 一个测试 store，UpsertACPSlotStatus 写入两条
	// （slot-1/conn-1/phase ready，slot-2/conn-1/phase failed + LastErrorMessage），
	// 然后:
	items, err := store.ListACPSlotStatuses(ctx)
	if err != nil {
		t.Fatalf("ListACPSlotStatuses() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	// 断言按 (connection_id, ordinal) 排序、字段透传正确。
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/control/ -run TestHandleDiagnosticsQuery -v; go test ./internal/daemonstore/ -run TestListACPSlotStatuses -v`
Expected: FAIL（类型/方法不存在，编译错误）

- [ ] **Step 3: Write minimal implementation**

1. `internal/control/types.go`：加入上面 Interfaces 块列出的全部类型与常量。
2. `internal/control/validation.go`：在 query 校验的 switch 中按 `QueryStatusGet` 分支的现有写法加 `QueryDiagnosticsGet`（要求 `GetDiagnostics != nil`）。
3. `internal/control/service.go`：
   - `ControlService` 加字段 `diagnostics DiagnosticsProvider`，`NewService` 赋值。
   - `HandleQuery` switch 加：

```go
	case QueryDiagnosticsGet:
		return s.handleDiagnosticsQuery(ctx)
```

   - 新增：

```go
func (s *ControlService) handleDiagnosticsQuery(ctx context.Context) (QueryResult, error) {
	view := DiagnosticsView{}
	if s.diagnostics != nil {
		view.RuntimeDiagnostics = s.diagnostics.RuntimeDiagnostics(ctx)
	}
	remotes, err := s.store.ListRemotes(ctx, ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		return QueryResult{Type: QueryDiagnosticsGet, Error: ptr(errorToControlError(err))}, nil
	}
	for _, remote := range remotes {
		if remote.Status != nil {
			view.Remotes = append(view.Remotes, *remote.Status)
		}
	}
	conns, err := s.store.ListAgentConnections(ctx, ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		return QueryResult{Type: QueryDiagnosticsGet, Error: ptr(errorToControlError(err))}, nil
	}
	for _, conn := range conns {
		if conn.Status != nil {
			view.AgentConnections = append(view.AgentConnections, *conn.Status)
		}
	}
	if slots, ok := s.store.(ACPSlotStatusSource); ok {
		items, err := slots.ListACPSlotStatuses(ctx)
		if err != nil {
			return QueryResult{Type: QueryDiagnosticsGet, Error: ptr(errorToControlError(err))}, nil
		}
		view.ACPSlots = items
	}
	return QueryResult{Type: QueryDiagnosticsGet, Diagnostics: &view}, nil
}
```

4. `internal/daemonstore/repository.go` 新增（放在 `GetACPSlotStatus` 旁）：

```go
func (s *Store) ListACPSlotStatuses(ctx context.Context) ([]control.ACPSlotStatusInfo, error) {
	var statuses []ACPSlotStatus
	if err := s.db.WithContext(ctx).
		Order("connection_id, ordinal").
		Find(&statuses).Error; err != nil {
		return nil, err
	}
	out := make([]control.ACPSlotStatusInfo, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, control.ACPSlotStatusInfo{
			SlotID:           status.SlotID,
			ConnectionID:     status.ConnectionID,
			Ordinal:          status.Ordinal,
			Phase:            status.Phase,
			FailureClass:     status.FailureClass,
			LastErrorCode:    status.LastErrorCode,
			LastErrorMessage: status.LastErrorMessage,
			UpdatedAt:        status.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}
```

（`s.db` 字段名以该文件现有查询方法为准；如果既有方法用别的访问器，照抄。）

5. `internal/daemon/runtime_supervisors.go`：`runtimeSupervisors` 加字段 `startedAt time.Time`、`logFilePath string`，并新增：

```go
func (s *runtimeSupervisors) RuntimeDiagnostics(ctx context.Context) control.RuntimeDiagnostics {
	_ = ctx
	out := control.RuntimeDiagnostics{PaxdVersion: s.paxdVersion}
	if !s.startedAt.IsZero() {
		startedAt := s.startedAt
		out.StartedAt = &startedAt
	}
	if s.logFilePath != "" {
		info := control.LogFileInfo{Path: s.logFilePath}
		if stat, err := os.Stat(s.logFilePath); err == nil {
			info.SizeBytes = stat.Size()
		}
		out.LogFile = &info
	}
	if s.transportStats != nil {
		stats := s.transportStats.Stats()
		queueIDs := make([]string, 0, len(stats))
		for queueID := range stats {
			queueIDs = append(queueIDs, queueID)
		}
		sort.Strings(queueIDs)
		for _, queueID := range queueIDs {
			stat := stats[queueID]
			out.TransportQueues = append(out.TransportQueues, control.TransportQueueDiagnostics{
				QueueID:      queueID,
				Bound:        stat.Bound,
				Tail:         stat.Tail,
				AckedThrough: stat.AckedThrough,
				NextToSend:   stat.NextToSend,
				Unacked:      stat.Tail - stat.AckedThrough,
				LastError:    stat.LastError,
			})
		}
	}
	if s.transportFlusher != nil {
		stats := s.transportFlusher.Stats()
		out.TransportWriteBehind = &control.TransportWriteBehindStats{
			DirtyFrames:              stats.DirtyFrames,
			DirtyPatches:             stats.DirtyPatches,
			DirtyBytes:               stats.DirtyBytes,
			Degraded:                 stats.Degraded,
			ConsecutiveFlushFailures: stats.ConsecutiveFlushFailures,
			LastFlushError:           stats.LastFlushError,
		}
	}
	return out
}
```

6. `internal/daemon/bootstrap.go`：构造 `supervisors` 时设置 `startedAt: time.Now().UTC()`、`logFilePath: cfg.Daemon.LogFile`；`control.NewService` 的 `ServiceOptions` 加 `Diagnostics: supervisors`。

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/control/ ./internal/daemonstore/ ./internal/daemon/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/control/ internal/daemonstore/ internal/daemon/
git commit -m "feat: add diagnostics.get control query with transport stats

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: local API `GET /v1/diagnostics`

**Files:**
- Modify: `internal/localapi/routes.go`（加路由）
- Modify: `internal/localapi/handler.go`（加 handler 方法）
- Modify: `internal/localapi/openapi.go`（endpoints 表加一项，~line 161 区域）
- Test: `internal/localapi/handler_test.go`（追加）

**Interfaces:**
- Consumes: `control.QueryDiagnosticsGet` / `control.GetDiagnosticsQuery`（Task 7）。
- Produces: `GET /v1/diagnostics` → `QueryResult`（JSON，`diagnostics` 字段）。

- [ ] **Step 1: Write the failing test**（`internal/localapi/handler_test.go` 追加，fake service 沿用该文件既有基建）

```go
func TestRouteDiagnosticsGet(t *testing.T) {
	service := &fakeService{ // 该文件现有 fake control.Service；record query 的方式照现有用例
		queryResult: control.QueryResult{
			Type:        control.QueryDiagnosticsGet,
			Diagnostics: &control.DiagnosticsView{RuntimeDiagnostics: control.RuntimeDiagnostics{PaxdVersion: "v-test"}},
		},
	}
	handler := NewHandler(service)
	req := httptest.NewRequest(http.MethodGet, "/v1/diagnostics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var result control.QueryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Diagnostics == nil || result.Diagnostics.PaxdVersion != "v-test" {
		t.Fatalf("Diagnostics = %+v", result.Diagnostics)
	}
	// 断言 fake service 收到的 Query.Type == control.QueryDiagnosticsGet 且 GetDiagnostics != nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/localapi/ -run TestRouteDiagnosticsGet -v`
Expected: FAIL（404 / 路由不存在）

- [ ] **Step 3: Write minimal implementation**

`routes.go` 在 `GET /v1/status` 行后加：

```go
	h.mux.HandleFunc("GET /v1/diagnostics", h.routeDiagnosticsGet)
```

`handler.go` 在 `routeStatusGet` 后加：

```go
func (h *Handler) routeDiagnosticsGet(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, control.Query{Type: control.QueryDiagnosticsGet, GetDiagnostics: &control.GetDiagnosticsQuery{}})
}
```

`openapi.go` endpoints 表（`/v1/status` 条目旁）加：

```go
	{Method: "GET", Path: "/v1/diagnostics", Summary: "Get runtime diagnostics: transport queue stats, reconnects, slot phases, log file info.", OperationID: "getDiagnostics", Response: "QueryResult", Tags: []string{"status"}},
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/localapi/ -v`
Expected: PASS

- [ ] **Step 5: 全量验证 + Commit**

Run: `go build ./... && go test ./...`
Expected: 全部 PASS

```bash
git add internal/localapi/
git commit -m "feat: expose GET /v1/diagnostics on local API

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 9: 端到端验证 + README 更新

**Files:**
- Modify: `README.md`（配置示例的 `daemon:` 段加三个新键；「本地 SQLite」节后补一句诊断接口）

**Interfaces:**
- Consumes: 全部前序 task。

- [ ] **Step 1: 前台跑 daemon 验证日志与诊断**

```bash
go build -o /tmp/paxd-dev ./cmd/paxd/
/tmp/paxd-dev run --control-socket /tmp/paxd-dev.sock 2>/dev/null &
sleep 2
curl -s --unix-socket /tmp/paxd-dev.sock http://paxd/v1/diagnostics | head -c 2000; echo
tail -3 ~/.paxd/logs/paxd.log
kill %1
```

Expected: diagnostics 返回 JSON（含 `paxd_version`、`log_file`）；`~/.paxd/logs/paxd.log` 有 `[paxd] starting` 等条目。

- [ ] **Step 2: README 更新**

`daemon:` 配置示例加：

```yaml
  log_file: ~/.paxd/logs/paxd.log
  log_max_size_mb: 20
  log_max_backups: 3
```

并在配置节后补一段（两三句）：paxd 自管滚动日志；harness stderr 进主日志并在 slot 失败时写入 status；`GET /v1/diagnostics`（unix socket）返回运行时诊断。

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document rolling log config and diagnostics endpoint

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```
