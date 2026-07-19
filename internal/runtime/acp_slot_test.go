package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPSlotInitializesAndEmitsOneTerminalEventOnProcessExit(t *testing.T) {
	ctx := context.Background()
	proc := newSlotFakeLocalACPProcess()
	runner := slotFakeLocalACPProcessRunner{proc: proc}
	var mu sync.Mutex
	var events []ACPSlotEvent
	slot := NewACPSlot(ACPSlotSpec{
		ConnectionID: "conn_1",
		SlotID:       "slot_a",
		Ordinal:      0,
		ProcessEpoch: "epoch_a",
		Command:      []string{"fake-acp"},
		PaxdVersion:  "test",
	}, runner, WithACPSlotEventSink(ACPSlotEventSinkFunc(func(event ACPSlotEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	})))

	started := make(chan error, 1)
	go func() { started <- slot.Start(ctx) }()
	require.Eventually(t, func() bool {
		return bytes.Contains(proc.stdinBytes(), []byte(`"method":"initialize"`))
	}, eventuallyWait, eventuallyTick)
	proc.writeStdout([]byte(`{"jsonrpc":"2.0","id":"paxd.initialize","result":{"protocolVersion":1,"agentCapabilities":{"prompt":true}}}` + "\n"))
	require.NoError(t, <-started)
	assert.True(t, slot.Ready())
	stdin := proc.stdinBytes()
	assert.Contains(t, string(stdin), `"method":"notifications/initialized"`)
	assert.Less(t,
		bytes.Index(stdin, []byte(`"method":"initialize"`)),
		bytes.Index(stdin, []byte(`"method":"notifications/initialized"`)),
	)

	proc.finish(nil)
	require.Eventually(t, func() bool {
		select {
		case <-slot.Done():
			return true
		default:
			return false
		}
	}, eventuallyWait, eventuallyTick)

	mu.Lock()
	defer mu.Unlock()
	var terminal []ACPSlotEvent
	for _, event := range events {
		if event.Type == ACPSlotEventTerminal {
			terminal = append(terminal, event)
		}
	}
	require.Len(t, terminal, 1)
	assert.Equal(t, "slot_a", terminal[0].SlotID)
	assert.Equal(t, "epoch_a", terminal[0].ProcessEpoch)
	assert.Equal(t, ACPSlotPhaseStopped, terminal[0].Phase)
}

func TestACPSlotSessionReportsReadyAfterInitialize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proc := newSlotFakeLocalACPProcess()
	ready := make(chan ACPSlotSpec, 1)
	capabilities := make(chan ACPPoolCapabilityReport, 1)
	capabilityRemoved := make(chan ACPSlotSpec, 1)
	registry := NewACPPoolRegistry(ACPRouteStoreFactoryFunc(func(connectionID string) ACPRouteStore {
		return newFakeACPRouteStore(connectionID)
	}))
	session := NewACPSlotSession(ACPSlotSessionConfig{
		Spec: ACPSlotSpec{
			ConnectionID:       "conn_1",
			RemoteID:           "local",
			SlotID:             "slot_a",
			Ordinal:            0,
			ProcessEpoch:       "epoch_a",
			Command:            []string{"fake-acp"},
			PaxdVersion:        "test",
			CommandFingerprint: "fingerprint_a",
		},
		Runner:   slotFakeLocalACPProcessRunner{proc: proc},
		Registry: registry,
		ReadyHandler: func(ctx context.Context, spec ACPSlotSpec) {
			ready <- spec
		},
		CapabilityHandler: func(ctx context.Context, spec ACPSlotSpec, report *ACPPoolCapabilityReport) {
			if report == nil {
				capabilityRemoved <- spec
				return
			}
			capabilities <- *report
		},
	})
	done := make(chan Exit, 1)
	go func() { done <- session.Run(ctx) }()
	require.Eventually(t, func() bool {
		return bytes.Contains(proc.stdinBytes(), []byte(`"method":"initialize"`))
	}, eventuallyWait, eventuallyTick)
	proc.writeStdout([]byte(`{"jsonrpc":"2.0","id":"paxd.initialize","result":{"protocolVersion":1,"agentCapabilities":{}}}` + "\n"))

	select {
	case spec := <-ready:
		assert.Equal(t, "conn_1", spec.ConnectionID)
		assert.Equal(t, "slot_a", spec.SlotID)
		assert.Equal(t, "epoch_a", spec.ProcessEpoch)
	case <-time.After(eventuallyWait):
		t.Fatal("slot session did not report ready")
	}
	select {
	case report := <-capabilities:
		assert.Equal(t, "conn_1", report.ConnectionID)
		assert.Equal(t, "test", report.PaxdVersion)
		assert.Equal(t, "fingerprint_a", report.CommandFingerprint)
		assert.NotEmpty(t, report.ClientProfileHash)
		assert.NotEmpty(t, report.WorkerResultHash)
		assert.Equal(t, 1, report.ProtocolVersion)
		assert.Equal(t, ACPPoolInitPhaseReady, report.InitPhase)
		assert.False(t, report.InitializedAt.IsZero())
	case <-time.After(eventuallyWait):
		t.Fatal("slot session did not report capability")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(eventuallyWait):
		t.Fatal("slot session did not stop after cancellation")
	}
	select {
	case spec := <-capabilityRemoved:
		assert.Equal(t, "slot_a", spec.SlotID)
		assert.Equal(t, "epoch_a", spec.ProcessEpoch)
	case <-time.After(eventuallyWait):
		t.Fatal("slot session did not remove capability after stopping")
	}
}

func TestACPSlotCapturesStderrTail(t *testing.T) {
	ctx := context.Background()
	proc := newSlotFakeLocalACPProcess()
	proc.stderr = strings.NewReader("boom: harness crashed\n")
	runner := slotFakeLocalACPProcessRunner{proc: proc}
	slot := NewACPSlot(ACPSlotSpec{
		ConnectionID: "conn_1",
		SlotID:       "slot_a",
		Ordinal:      0,
		ProcessEpoch: "epoch_a",
		Command:      []string{"fake-acp"},
		PaxdVersion:  "test",
	}, runner)

	started := make(chan error, 1)
	go func() { started <- slot.Start(ctx) }()
	require.Eventually(t, func() bool {
		return bytes.Contains(proc.stdinBytes(), []byte(`"method":"initialize"`))
	}, eventuallyWait, eventuallyTick)
	proc.writeStdout([]byte(`{"jsonrpc":"2.0","id":"paxd.initialize","result":{"protocolVersion":1}}` + "\n"))
	require.NoError(t, <-started)

	proc.finish(errors.New("exit status 1"))
	require.Eventually(t, func() bool {
		select {
		case <-slot.Done():
			return true
		default:
			return false
		}
	}, eventuallyWait, eventuallyTick)
	require.Eventually(t, func() bool {
		return strings.Contains(slot.StderrTail(0), "boom: harness crashed")
	}, eventuallyWait, eventuallyTick)
	assert.Contains(t, slot.StderrTail(stderrStatusTailLimit), "boom: harness crashed")
}

type slotFakeLocalACPProcessRunner struct {
	proc *slotFakeLocalACPProcess
}

func (r slotFakeLocalACPProcessRunner) Start(ctx context.Context, spec LocalACPProcessSpec) (LocalACPProcess, error) {
	return r.proc, nil
}

type slotFakeLocalACPProcess struct {
	stdin  *bytes.Buffer
	stdout *io.PipeReader
	outw   *io.PipeWriter
	stderr io.Reader
	done   chan error
	mu     sync.Mutex
}

func newSlotFakeLocalACPProcess() *slotFakeLocalACPProcess {
	stdout, outw := io.Pipe()
	return &slotFakeLocalACPProcess{
		stdin:  &bytes.Buffer{},
		stdout: stdout,
		outw:   outw,
		done:   make(chan error, 1),
	}
}

func (p *slotFakeLocalACPProcess) Stdin() io.WriteCloser {
	return fakeWriteCloser{write: func(data []byte) (int, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.stdin.Write(data)
	}}
}

func (p *slotFakeLocalACPProcess) Stdout() io.Reader {
	return p.stdout
}

func (p *slotFakeLocalACPProcess) Stderr() io.Reader {
	if p.stderr != nil {
		return p.stderr
	}
	return bytes.NewReader(nil)
}

func (p *slotFakeLocalACPProcess) Wait() error {
	return <-p.done
}

func (p *slotFakeLocalACPProcess) Terminate(ctx context.Context) error {
	p.finish(nil)
	return nil
}

func (p *slotFakeLocalACPProcess) stdinBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.stdin.Bytes()...)
}

func (p *slotFakeLocalACPProcess) writeStdout(payload []byte) {
	_, _ = p.outw.Write(payload)
}

func (p *slotFakeLocalACPProcess) finish(err error) {
	_ = p.outw.Close()
	select {
	case p.done <- err:
	default:
	}
}

type fakeWriteCloser struct {
	write func([]byte) (int, error)
}

func (w fakeWriteCloser) Write(data []byte) (int, error) {
	return w.write(data)
}

func (w fakeWriteCloser) Close() error {
	return nil
}
