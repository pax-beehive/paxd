package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int64           `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type authMethod struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
	Vars []struct {
		Name string `json:"name"`
	} `json:"vars,omitempty"`
}

type initializeResult struct {
	AuthMethods []authMethod `json:"authMethods"`
}

type probe struct {
	start time.Time
}

func main() {
	timeout := flag.Duration("timeout", 10*time.Second, "timeout per JSON-RPC call")
	waitDelay := flag.Duration("wait-delay", 2*time.Second, "exec.Cmd WaitDelay")
	shutdownGrace := flag.Duration("shutdown-grace", 2*time.Second, "time to wait after closing stdin before killing")
	skipKill := flag.Bool("skip-kill", false, "do not kill the process after closing stdin; useful to test whether Wait would remain blocked")
	authMethodID := flag.String("auth", "auto", "auth method id, auto, or empty to skip")
	workingDir := flag.String("cwd", "", "working directory for ACP command")
	flag.Parse()

	command := flag.Args()
	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/acp-session-list-probe.go [flags] -- <acp-command> [args...]")
		os.Exit(2)
	}

	p := probe{start: time.Now()}
	p.log("start command=%q timeout=%s waitDelay=%s shutdownGrace=%s", command, *timeout, *waitDelay, *shutdownGrace)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = *workingDir
	cmd.WaitDelay = *waitDelay

	stdin, err := cmd.StdinPipe()
	must("stdin pipe", err)
	stdout, err := cmd.StdoutPipe()
	must("stdout pipe", err)
	stderr, err := cmd.StderrPipe()
	must("stderr pipe", err)

	p.log("cmd.Start")
	must("start", cmd.Start())
	p.log("started pid=%d", cmd.Process.Pid)

	go copyStderr(p, stderr)

	reader := bufio.NewReader(stdout)
	nextID := int64(1)

	initRaw, err := call(p, *timeout, stdin, reader, nextID, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{
			"name":    "acp-session-list-probe",
			"version": "0.1.0",
		},
	})
	if err != nil {
		p.log("initialize error: %v", err)
		shutdown(p, cmd, stdin, *shutdownGrace, *skipKill)
		os.Exit(1)
	}
	nextID++

	var initResult initializeResult
	if len(initRaw) > 0 {
		if err := json.Unmarshal(initRaw, &initResult); err != nil {
			p.log("initialize decode error: %v", err)
		}
	}
	p.log("initialize authMethods=%s", describeAuthMethods(initResult.AuthMethods))

	chosenAuth := chooseAuth(*authMethodID, initResult.AuthMethods)
	if chosenAuth != "" {
		_, err := call(p, *timeout, stdin, reader, nextID, "authenticate", map[string]any{
			"methodId": chosenAuth,
		})
		if err != nil {
			p.log("authenticate method=%q error: %v", chosenAuth, err)
			shutdown(p, cmd, stdin, *shutdownGrace, *skipKill)
			os.Exit(1)
		}
		nextID++
	} else {
		p.log("authenticate skipped")
	}

	listRaw, err := call(p, *timeout, stdin, reader, nextID, "session/list", map[string]any{})
	if err != nil {
		p.log("session/list error: %v", err)
		shutdown(p, cmd, stdin, *shutdownGrace, *skipKill)
		os.Exit(1)
	}
	p.log("session/list result bytes=%d preview=%s", len(listRaw), preview(listRaw, 800))

	shutdown(p, cmd, stdin, *shutdownGrace, *skipKill)
}

func call(
	p probe,
	timeout time.Duration,
	stdin io.Writer,
	reader *bufio.Reader,
	id int64,
	method string,
	params any,
) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}

	p.log("write id=%d method=%s payload=%s", id, method, payload)
	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		lineCh := make(chan readResult, 1)
		go func() {
			line, err := reader.ReadBytes('\n')
			lineCh <- readResult{line: line, err: err}
		}()

		select {
		case <-deadline.C:
			return nil, fmt.Errorf("timeout waiting for id=%d method=%s", id, method)
		case result := <-lineCh:
			if result.err != nil {
				return nil, fmt.Errorf("read response: %w", result.err)
			}
			line := bytes.TrimSpace(result.line)
			if len(line) == 0 {
				continue
			}
			p.log("read line=%s", preview(line, 1000))
			var msg rpcMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				p.log("ignore non-json line: %v", err)
				continue
			}
			if msg.ID != id {
				p.log("ignore message for id=%d while waiting for id=%d", msg.ID, id)
				continue
			}
			if msg.Error != nil {
				return nil, fmt.Errorf("rpc error %d: %s", msg.Error.Code, msg.Error.Message)
			}
			p.log("matched id=%d method=%s", id, method)
			return msg.Result, nil
		}
	}
}

type readResult struct {
	line []byte
	err  error
}

func shutdown(p probe, cmd *exec.Cmd, stdin io.Closer, grace time.Duration, skipKill bool) {
	p.log("shutdown: close stdin")
	if err := stdin.Close(); err != nil {
		p.log("shutdown: close stdin error: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		p.log("shutdown: Wait start")
		waitCh <- cmd.Wait()
	}()

	select {
	case err := <-waitCh:
		p.log("shutdown: Wait after stdin close returned: %v", err)
		return
	case <-time.After(grace):
		p.log("shutdown: still running after %s; Kill", grace)
	}

	if skipKill {
		p.log("shutdown: skip-kill enabled; leaving process running")
		return
	}

	if cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil {
			p.log("shutdown: Kill error: %v", err)
		}
	}

	select {
	case err := <-waitCh:
		p.log("shutdown: Wait after Kill returned: %v", err)
	case <-time.After(grace):
		p.log("shutdown: Wait still blocked %s after Kill", grace)
	}
}

func copyStderr(p probe, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		p.log("stderr: %s", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		p.log("stderr read error: %v", err)
	}
	p.log("stderr EOF")
}

func chooseAuth(flagValue string, methods []authMethod) string {
	switch flagValue {
	case "":
		return ""
	case "auto":
		for _, method := range methods {
			if method.Type != "env_var" {
				continue
			}
			for _, envVar := range method.Vars {
				if envVar.Name != "" && os.Getenv(envVar.Name) != "" {
					return method.ID
				}
			}
		}
		for _, method := range methods {
			if method.ID != "" && !interactiveAuthMethod(method) {
				return method.ID
			}
		}
		return ""
	default:
		return flagValue
	}
}

func interactiveAuthMethod(method authMethod) bool {
	if method.Type == "terminal" || method.Type == "oauth" {
		return true
	}
	text := strings.ToLower(method.ID + " " + method.Name)
	return strings.Contains(text, "oauth") ||
		strings.Contains(text, "api-key") ||
		strings.Contains(text, "api key") ||
		strings.Contains(text, "gateway") ||
		strings.Contains(text, "vertex") ||
		strings.Contains(text, "login")
}

func describeAuthMethods(methods []authMethod) string {
	if len(methods) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(methods))
	for _, method := range methods {
		parts = append(parts, fmt.Sprintf("{id:%q type:%q name:%q}", method.ID, method.Type, method.Name))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func preview(data []byte, limit int) string {
	text := string(data)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "...(truncated)"
}

func (p probe) log(format string, args ...any) {
	elapsed := time.Since(p.start).Truncate(time.Millisecond)
	fmt.Fprintf(os.Stderr, "[%s] %s\n", elapsed, fmt.Sprintf(format, args...))
}

func must(label string, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", label, err)
	os.Exit(1)
}
