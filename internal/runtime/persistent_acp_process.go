package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pax-beehive/paxkit/reliablemq"
)

// PersistentACPProcessPool keeps the local ACP stdio process alive across
// transient tunnel reconnects. The supervisor still owns the process lifetime:
// canceled supervisor contexts, config changes, and explicit restarts stop the
// cached process.
type PersistentACPProcessPool struct {
	runner               LocalACPProcessRunner
	store                reliablemq.DurableStore
	versionProvider      PaxdVersionProvider
	reporter             ACPPoolCapabilityReporter
	runtimeVersionProber CodexRuntimeVersionProber

	mu               sync.Mutex
	processes        map[string]*persistentACPProcess
	reportGeneration int64
}

func NewPersistentACPProcessPool(
	runner LocalACPProcessRunner,
	store reliablemq.DurableStore,
	opts ...PersistentACPProcessPoolOption,
) *PersistentACPProcessPool {
	if runner == nil {
		runner = ExecLocalACPProcessRunner{}
	}
	pool := &PersistentACPProcessPool{
		runner:               runner,
		store:                store,
		versionProvider:      NoopPaxdVersionProvider{},
		reporter:             NoopACPPoolCapabilityReporter{},
		runtimeVersionProber: ExecCodexRuntimeVersionProber{},
		processes:            make(map[string]*persistentACPProcess),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(pool)
		}
	}
	if pool.versionProvider == nil {
		pool.versionProvider = NoopPaxdVersionProvider{}
	}
	if pool.reporter == nil {
		pool.reporter = NoopACPPoolCapabilityReporter{}
	}
	if pool.runtimeVersionProber == nil {
		pool.runtimeVersionProber = ExecCodexRuntimeVersionProber{}
	}
	return pool
}

type PersistentACPProcessPoolOption func(*PersistentACPProcessPool)

func WithPaxdVersionProvider(provider PaxdVersionProvider) PersistentACPProcessPoolOption {
	return func(pool *PersistentACPProcessPool) {
		if provider != nil {
			pool.versionProvider = provider
		}
	}
}

func WithACPPoolCapabilityReporter(reporter ACPPoolCapabilityReporter) PersistentACPProcessPoolOption {
	return func(pool *PersistentACPProcessPool) {
		if reporter != nil {
			pool.reporter = reporter
		}
	}
}

func WithPersistentACPCodexRuntimeVersionProber(prober CodexRuntimeVersionProber) PersistentACPProcessPoolOption {
	return func(pool *PersistentACPProcessPool) {
		if prober != nil {
			pool.runtimeVersionProber = prober
		}
	}
}

func (p *PersistentACPProcessPool) Acquire(
	ctx context.Context,
	spec AgentConnectionSpec,
) (*persistentACPProcess, error) {
	if p == nil {
		return nil, fmt.Errorf("persistent ACP process pool is required")
	}
	if p.store == nil {
		return nil, fmt.Errorf("transport store is required")
	}
	key := persistentACPProcessKey(spec)
	if key == "" {
		return nil, fmt.Errorf("connection id is required")
	}
	fingerprint := persistentACPProcessFingerprint(spec)

	p.mu.Lock()
	if current := p.processes[key]; current != nil {
		if current.fingerprint == fingerprint && !current.Exited() {
			p.mu.Unlock()
			return current, nil
		}
		delete(p.processes, key)
		p.mu.Unlock()
		current.Terminate(context.Background())
	} else {
		p.mu.Unlock()
	}

	proc, err := newPersistentACPProcess(
		ctx,
		p.runner,
		p.store,
		spec,
		fingerprint,
		p.versionProvider.PaxdVersion(),
		p.nextReportGeneration(),
		p.reporter,
		p.runtimeVersionProber,
	)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if current := p.processes[key]; current != nil {
		if current.fingerprint == fingerprint && !current.Exited() {
			p.mu.Unlock()
			proc.Terminate(context.Background())
			return current, nil
		}
		delete(p.processes, key)
		p.mu.Unlock()
		current.Terminate(context.Background())
	} else {
		p.mu.Unlock()
	}

	p.mu.Lock()
	p.processes[key] = proc
	p.mu.Unlock()
	return proc, nil
}

func (p *PersistentACPProcessPool) nextReportGeneration() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reportGeneration++
	return p.reportGeneration
}

func (p *PersistentACPProcessPool) Stop(spec AgentConnectionSpec) {
	if p == nil {
		return
	}
	key := persistentACPProcessKey(spec)
	p.mu.Lock()
	proc := p.processes[key]
	delete(p.processes, key)
	p.mu.Unlock()
	if proc != nil {
		proc.Terminate(context.Background())
	}
}

func (p *PersistentACPProcessPool) Forget(
	spec AgentConnectionSpec,
	proc *persistentACPProcess,
) {
	if p == nil || proc == nil {
		return
	}
	key := persistentACPProcessKey(spec)
	p.mu.Lock()
	if p.processes[key] == proc {
		delete(p.processes, key)
	}
	p.mu.Unlock()
}

type persistentACPProcess struct {
	spec                 AgentConnectionSpec
	fingerprint          string
	paxdVersion          string
	reportGen            int64
	proc                 LocalACPProcess
	store                reliablemq.DurableStore
	reporter             ACPPoolCapabilityReporter
	runtimeVersionProber CodexRuntimeVersionProber

	mu              sync.Mutex
	outputSink      func(context.Context, []byte) error
	outputSinkToken int64
	err             error
	initProfile     acpClientInitProfile
	initResult      acpWorkerInitResult
	runtimeFallback *ACPRuntimeImplementation

	initCh   chan acpInitCapture
	done     chan struct{}
	stopOnce sync.Once
}

func newPersistentACPProcess(
	ctx context.Context,
	runner LocalACPProcessRunner,
	store reliablemq.DurableStore,
	spec AgentConnectionSpec,
	fingerprint string,
	paxdVersion string,
	reportGeneration int64,
	reporter ACPPoolCapabilityReporter,
	runtimeVersionProber CodexRuntimeVersionProber,
) (*persistentACPProcess, error) {
	proc, err := runner.Start(ctx, LocalACPProcessSpec{
		Command:    spec.Command,
		WorkingDir: spec.WorkingDir,
		Env:        spec.Env,
	})
	if err != nil {
		return nil, err
	}
	p := &persistentACPProcess{
		spec:                 spec,
		fingerprint:          fingerprint,
		paxdVersion:          StaticPaxdVersionProvider(paxdVersion).PaxdVersion(),
		reportGen:            reportGeneration,
		proc:                 proc,
		store:                store,
		reporter:             reporter,
		runtimeVersionProber: runtimeVersionProber,
		initCh:               make(chan acpInitCapture, 1),
		done:                 make(chan struct{}),
	}
	if p.reporter == nil {
		p.reporter = NoopACPPoolCapabilityReporter{}
	}
	if p.runtimeVersionProber == nil {
		p.runtimeVersionProber = ExecCodexRuntimeVersionProber{}
	}
	go newStderrTail(stderrTailLimit).Consume(proc.Stderr(), fmt.Sprintf(
		"[harness stderr] connection_id=%s", spec.ConnectionID))
	go p.copyStdout()
	go p.wait()
	if err := p.initialize(ctx); err != nil {
		p.Terminate(context.Background())
		return nil, err
	}
	return p, nil
}

func (p *persistentACPProcess) Stdin() io.Writer {
	return p.proc.Stdin()
}

func (p *persistentACPProcess) Done() <-chan struct{} {
	return p.done
}

func (p *persistentACPProcess) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *persistentACPProcess) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *persistentACPProcess) AttachOutputSink(sink func(context.Context, []byte) error) func() {
	p.mu.Lock()
	p.outputSinkToken++
	token := p.outputSinkToken
	p.outputSink = sink
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		if p.outputSinkToken == token {
			p.outputSink = nil
		}
		p.mu.Unlock()
	}
}

func (p *persistentACPProcess) Terminate(ctx context.Context) {
	p.stopOnce.Do(func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = p.proc.Terminate(stopCtx)
	})
}

func (p *persistentACPProcess) InitializeResult() json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.initResult.Result) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), p.initResult.Result...)
}

func (p *persistentACPProcess) initialize(ctx context.Context) error {
	profile, err := buildACPClientInitProfile(p.paxdVersion)
	if err != nil {
		return fmt.Errorf("build acp initialize profile: %w", err)
	}
	request, err := buildInternalACPInitializeRequest(profile)
	if err != nil {
		return fmt.Errorf("build acp initialize request: %w", err)
	}
	initializedNotification, err := buildInternalACPInitializedNotification()
	if err != nil {
		return fmt.Errorf("build acp initialized notification: %w", err)
	}
	p.mu.Lock()
	p.initProfile = profile
	p.mu.Unlock()
	if err := writeACPStdin(p.proc.Stdin(), request); err != nil {
		return fmt.Errorf("send acp initialize: %w", err)
	}
	log.Printf(
		"[paxd] persistent ACP initialize request sent connection_id=%s transport_queue_id=%s request_id=%q",
		p.spec.ConnectionID,
		p.spec.TransportQueueID,
		internalACPInitializeID,
	)

	select {
	case capture := <-p.initCh:
		if capture.err != nil {
			_ = p.reportCapability(ctx, ACPPoolInitPhaseFailed, "initialize_failed", capture.err.Error())
			return capture.err
		}
		if err := writeACPStdin(p.proc.Stdin(), initializedNotification); err != nil {
			_ = p.reportCapability(ctx, ACPPoolInitPhaseFailed, "initialized_notification_failed", err.Error())
			return fmt.Errorf("send acp initialized notification: %w", err)
		}
		p.captureRuntimeFallback(ctx)
		_ = p.reportCapability(ctx, ACPPoolInitPhaseReady, "", "")
		return nil
	case <-p.done:
		if err := p.Err(); err != nil {
			_ = p.reportCapability(ctx, ACPPoolInitPhaseFailed, "process_exited", err.Error())
			return fmt.Errorf("acp process exited before initialize completed: %w", err)
		}
		_ = p.reportCapability(ctx, ACPPoolInitPhaseFailed, "process_exited", "acp process exited before initialize completed")
		return fmt.Errorf("acp process exited before initialize completed")
	case <-ctx.Done():
		_ = p.reportCapability(ctx, ACPPoolInitPhaseFailed, "initialize_canceled", ctx.Err().Error())
		return fmt.Errorf("acp initialize: %w", ctx.Err())
	}
}

func (p *persistentACPProcess) reportCapability(ctx context.Context, phase string, errCode string, errMessage string) error {
	p.mu.Lock()
	profile := p.initProfile
	result := p.initResult
	runtimeFallback := p.runtimeFallback
	p.mu.Unlock()
	report := ACPPoolCapabilityReport{
		ConnectionID:         p.spec.ConnectionID,
		ReportGeneration:     p.reportGen,
		PaxdVersion:          p.paxdVersion,
		CommandFingerprint:   p.fingerprint,
		ClientProfileHash:    profile.ProfileHash,
		WorkerResultHash:     result.ResultHash,
		ProtocolVersion:      protocolVersionFromResult(result.Result),
		ClientCapabilityKeys: capabilityKeys(profile.Params, "clientCapabilities", "client_capabilities"),
		WorkerCapabilityKeys: capabilityKeys(result.Result, "agentCapabilities", "agent_capabilities", "capabilities"),
		Implementation:       implementationIdentityWithRuntimeFallback(result.Result, runtimeFallback),
		InitPhase:            phase,
		InitializedAt:        result.InitializedAt,
		LastErrorCode:        errCode,
		LastErrorMessage:     errMessage,
	}.WithDefaults()
	return p.reporter.ReportACPPoolCapability(ctx, report)
}

func (p *persistentACPProcess) captureRuntimeFallback(ctx context.Context) {
	p.mu.Lock()
	result := append(json.RawMessage(nil), p.initResult.Result...)
	p.mu.Unlock()
	fallback := fallbackCodexRuntimeIdentity(ctx, result, LocalACPProcessSpec{
		Command:    p.spec.Command,
		WorkingDir: p.spec.WorkingDir,
		Env:        p.spec.Env,
	}, p.runtimeVersionProber)
	p.mu.Lock()
	p.runtimeFallback = fallback
	p.mu.Unlock()
}

func (p *persistentACPProcess) wait() {
	err := p.proc.Wait()
	p.mu.Lock()
	p.err = err
	p.outputSink = nil
	p.mu.Unlock()
	close(p.done)
}

func (p *persistentACPProcess) copyStdout() {
	reader := bufio.NewReader(p.proc.Stdout())
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineDelimiter(line)
			if len(line) > 0 {
				if !json.Valid(line) {
					log.Printf(
						"[paxd] persistent ACP process id=%s invalid stdout payload; terminating process",
						p.spec.ConnectionID,
					)
					p.Terminate(context.Background())
					return
				}
				if p.captureInitializeResponse(line) {
					continue
				}
				if sendErr := p.sendOutbound(context.Background(), line); sendErr != nil {
					log.Printf(
						"[paxd] persistent ACP process id=%s outbound journal failed: %v",
						p.spec.ConnectionID,
						sendErr,
					)
					p.Terminate(context.Background())
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF && !p.Exited() {
				log.Printf(
					"[paxd] persistent ACP process id=%s stdout read failed: %v",
					p.spec.ConnectionID,
					err,
				)
				p.Terminate(context.Background())
			}
			return
		}
	}
}

func (p *persistentACPProcess) captureInitializeResponse(line []byte) bool {
	msg, ok := parseACPRPCMessage(line)
	if !ok || !isInternalACPInitializeResponse(msg) {
		return false
	}
	capture := acpInitCapture{}
	if msg.Error != nil {
		capture.err = fmt.Errorf("acp initialize rpc error %d: %s", msg.Error.Code, msg.Error.Message)
	} else {
		result := bytes.TrimSpace(msg.Result)
		if len(result) == 0 {
			result = []byte(`{}`)
		}
		canonicalResult, err := canonicalJSON(json.RawMessage(result))
		if err != nil {
			capture.err = fmt.Errorf("canonicalize acp initialize result: %w", err)
		} else {
			p.mu.Lock()
			p.initResult = acpWorkerInitResult{
				Result:        canonicalResult,
				ResultHash:    hashBytes(canonicalResult),
				InitializedAt: time.Now(),
			}
			p.mu.Unlock()
		}
	}
	select {
	case p.initCh <- capture:
	default:
	}
	return true
}

func (p *persistentACPProcess) sendOutbound(ctx context.Context, payload []byte) error {
	p.mu.Lock()
	sink := p.outputSink
	p.mu.Unlock()
	if sink == nil {
		return fmt.Errorf("persistent ACP process output sink is not attached")
	}
	return sink(ctx, append([]byte(nil), payload...))
}

func persistentACPProcessKey(spec AgentConnectionSpec) string {
	return firstNonEmpty(spec.ConnectionID, spec.TransportQueueID, spec.CloudAgentID)
}

func persistentACPProcessFingerprint(spec AgentConnectionSpec) string {
	envKeys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	env := make([]string, 0, len(envKeys))
	for _, key := range envKeys {
		env = append(env, key+"="+spec.Env[key])
	}
	value := strings.Join([]string{
		spec.TransportQueueID,
		spec.CloudAgentID,
		spec.AgentType,
		spec.Harness,
		spec.WorkingDir,
		strings.Join(spec.Command, "\x00"),
		strings.Join(env, "\x00"),
	}, "\x01")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
