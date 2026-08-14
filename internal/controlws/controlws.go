package controlws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pax-beehive/paxd/internal/control"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
)

const (
	KindCommand       = "command"
	KindAck           = "ack"
	KindQuery         = "query"
	KindResponse      = "response"
	KindError         = "error"
	KindCommandResult = "command_result"
	KindReport        = "report"
)

type WebSocketConn = runtimes.WebSocketConn

type IncomingFrame struct {
	Kind      string           `json:"kind"`
	RequestID string           `json:"request_id,omitempty"`
	CommandID string           `json:"command_id,omitempty"`
	Command   *control.Command `json:"command,omitempty"`
	Query     *control.Query   `json:"query,omitempty"`
}

type AckFrame struct {
	Kind       string             `json:"kind"`
	CommandID  string             `json:"command_id"`
	CommandAck control.CommandAck `json:"command_ack"`
}

type QueryResponseFrame struct {
	Kind        string              `json:"kind"`
	RequestID   string              `json:"request_id"`
	QueryResult control.QueryResult `json:"query_result"`
}

type ErrorFrame struct {
	Kind      string               `json:"kind"`
	RequestID string               `json:"request_id,omitempty"`
	CommandID string               `json:"command_id,omitempty"`
	Error     control.ControlError `json:"error"`
}

type CommandResultFrame struct {
	Kind       string             `json:"kind"`
	CommandID  string             `json:"command_id"`
	CommandAck control.CommandAck `json:"command_ack"`
}

type ReportFrame struct {
	Kind     string         `json:"kind"`
	Version  int            `json:"version"`
	ReportID string         `json:"report_id"`
	Report   control.Report `json:"report"`
}

type CommandResultWatcher interface {
	WatchCommandResults(ctx context.Context, src control.Source) (<-chan control.CommandAck, error)
}

type ReportOptions struct {
	HeartbeatInterval                 time.Duration
	SendInitialHeartbeat              bool
	Heartbeat                         func() control.HeartbeatReport
	SnapshotInterval                  time.Duration
	SendInitialSnapshot               bool
	Now                               func() time.Time
	NewID                             func(prefix string) string
	StatusSubscribe                   func(remoteID string) (<-chan struct{}, func())
	AttachmentSubscribe               func(remoteID string) (<-chan control.AttachmentLocalState, func())
	PokeDebounce                      time.Duration
	SessionRuntimeSnapshotInterval    time.Duration
	SendInitialSessionRuntimeSnapshot bool
}

type Runner struct {
	Service control.Service
	Reports ReportOptions
}

func NewRunner(service control.Service) Runner {
	return Runner{Service: service}
}

func (r Runner) RunNodeControl(ctx context.Context, conn runtimes.WebSocketConn, spec runtimes.RemoteSpec) runtimes.Exit {
	return run(ctx, conn, control.Source{Kind: control.SourceRemote, RemoteID: spec.RemoteID}, r.Service, spec.NodeID, r.Reports)
}

func Run(ctx context.Context, conn WebSocketConn, src control.Source, service control.Service) runtimes.Exit {
	return run(ctx, conn, src, service, "", ReportOptions{})
}

func run(ctx context.Context, conn WebSocketConn, src control.Source, service control.Service, nodeID string, reports ReportOptions) runtimes.Exit {
	if conn == nil {
		return runtimes.ConfigExit("missing_websocket", "websocket connection is required")
	}
	if service == nil {
		return runtimes.ConfigExit("missing_control_service", "control service is required")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go closeOnContextDone(runCtx, conn)

	writer := &frameWriter{conn: conn}
	if watcher, ok := service.(CommandResultWatcher); ok {
		startCommandResultPump(runCtx, cancel, writer, src, watcher)
	}
	startReportPumps(runCtx, cancel, writer, src.RemoteID, nodeID, service, reports)

	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if runCtx.Err() != nil {
				return runtimes.CanceledExit(runCtx.Err())
			}
			return runtimes.TransientExit("node_control_read_failed", "node-control websocket read failed")
		}
		if messageType != websocket.TextMessage {
			if err := writer.write(ErrorFrame{Kind: KindError, Error: invalidFrame("message_type", "node-control frames must be text messages")}); err != nil {
				return writeExit(err)
			}
			continue
		}
		if err := handlePayload(runCtx, writer, src, service, payload); err != nil {
			return writeExit(err)
		}
	}
}

func startReportPumps(ctx context.Context, cancel context.CancelFunc, writer *frameWriter, remoteID string, nodeID string, service control.Service, opts ReportOptions) {
	if opts.HeartbeatInterval <= 0 && !opts.SendInitialHeartbeat && opts.SnapshotInterval <= 0 && !opts.SendInitialSnapshot && opts.StatusSubscribe == nil && opts.AttachmentSubscribe == nil &&
		opts.SessionRuntimeSnapshotInterval <= 0 && !opts.SendInitialSessionRuntimeSnapshot {
		return
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newID := opts.NewID
	if newID == nil {
		newID = func(prefix string) string {
			return fmt.Sprintf("%s_%d", prefix, time.Now().UTC().UnixNano())
		}
	}
	if opts.HeartbeatInterval > 0 || opts.SendInitialHeartbeat {
		go func() {
			send := func() bool {
				heartbeat := control.HeartbeatReport{}
				if opts.Heartbeat != nil {
					heartbeat = opts.Heartbeat()
				}
				frame := reportFrame(newID("rpt"), control.Report{
					Type:      control.ReportHeartbeat,
					RemoteID:  remoteID,
					NodeID:    nodeID,
					SentAt:    now().UTC().Format(time.RFC3339Nano),
					Heartbeat: &heartbeat,
				})
				if err := writer.write(frame); err != nil {
					cancel()
					return false
				}
				return true
			}
			if opts.SendInitialHeartbeat && !send() {
				return
			}
			if opts.HeartbeatInterval <= 0 {
				return
			}
			ticker := time.NewTicker(opts.HeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if !send() {
						return
					}
				}
			}
		}()
	}
	if opts.AttachmentSubscribe != nil {
		states, unsubscribe := opts.AttachmentSubscribe(remoteID)
		if states != nil {
			go func() {
				if unsubscribe != nil {
					defer unsubscribe()
				}
				for {
					select {
					case <-ctx.Done():
						return
					case state, ok := <-states:
						if !ok {
							return
						}
						stateCopy := state
						frame := reportFrame(newID("rpt"), control.Report{
							Type:                 control.ReportAttachmentLocalState,
							RemoteID:             remoteID,
							NodeID:               nodeID,
							SentAt:               now().UTC().Format(time.RFC3339Nano),
							AttachmentLocalState: &stateCopy,
						})
						if err := writer.write(frame); err != nil {
							cancel()
							return
						}
					}
				}
			}()
		}
	}
	startSessionRuntimeReportPump(ctx, cancel, writer, remoteID, nodeID, service, opts, now, newID)
	reporter, ok := service.(control.ReportService)
	if !ok {
		return
	}
	sendSnapshot := func() {
		snapshot, err := reporter.BuildRuntimeSnapshot(ctx, remoteID, nodeID)
		if err != nil {
			return
		}
		frame := reportFrame(newID("rpt"), control.Report{
			Type:            control.ReportRuntimeSnapshot,
			RemoteID:        remoteID,
			NodeID:          nodeID,
			SentAt:          now().UTC().Format(time.RFC3339Nano),
			RuntimeSnapshot: &snapshot,
		})
		if err := writer.write(frame); err != nil {
			cancel()
		}
	}
	if opts.SendInitialSnapshot {
		go sendSnapshot()
	}
	if opts.SnapshotInterval > 0 {
		go func() {
			ticker := time.NewTicker(opts.SnapshotInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sendSnapshot()
				}
			}
		}()
	}
	if opts.StatusSubscribe != nil {
		pokes, unsubscribe := opts.StatusSubscribe(remoteID)
		if pokes != nil {
			go func() {
				if unsubscribe != nil {
					defer unsubscribe()
				}
				debounce := opts.PokeDebounce
				if debounce <= 0 {
					debounce = time.Second
				}
				var timer *time.Timer
				var timerC <-chan time.Time
				defer func() {
					if timer != nil {
						timer.Stop()
					}
				}()
				for {
					select {
					case <-ctx.Done():
						return
					case _, ok := <-pokes:
						if !ok {
							return
						}
						if timer == nil {
							timer = time.NewTimer(debounce)
							timerC = timer.C
						}
					case <-timerC:
						timer.Stop()
						timer = nil
						timerC = nil
						sendSnapshot()
					}
				}
			}()
		}
	}
}

func startSessionRuntimeReportPump(
	ctx context.Context,
	cancel context.CancelFunc,
	writer *frameWriter,
	remoteID string,
	nodeID string,
	service control.Service,
	opts ReportOptions,
	now func() time.Time,
	newID func(string) string,
) {
	reporter, ok := service.(control.SessionRuntimeReportService)
	if !ok || opts.SessionRuntimeSnapshotInterval <= 0 && !opts.SendInitialSessionRuntimeSnapshot {
		return
	}
	go func() {
		changes, unsubscribe := reporter.SubscribeSessionRuntime(remoteID)
		if unsubscribe != nil {
			defer unsubscribe()
		}
		var ticker *time.Ticker
		var tickerC <-chan time.Time
		if opts.SessionRuntimeSnapshotInterval > 0 {
			ticker = time.NewTicker(opts.SessionRuntimeSnapshotInterval)
			tickerC = ticker.C
			defer ticker.Stop()
		}
		sequence := int64(0)
		send := func() bool {
			snapshots, err := reporter.BuildSessionRuntimeSnapshots(ctx, remoteID, nodeID)
			if err != nil {
				log.Printf("[paxd] session runtime snapshot build failed remote_id=%s node_id=%s: %v", remoteID, nodeID, err)
				return true
			}
			for i := range snapshots {
				sequence++
				snapshot := snapshots[i]
				snapshot.Sequence = sequence
				snapshot.GeneratedAt = now().UTC().Format(time.RFC3339Nano)
				frame := reportFrame(newID("rpt"), control.Report{
					Type:                   control.ReportSessionRuntimeSnapshot,
					RemoteID:               remoteID,
					NodeID:                 nodeID,
					SentAt:                 now().UTC().Format(time.RFC3339Nano),
					SessionRuntimeSnapshot: &snapshot,
				})
				if err := writer.write(frame); err != nil {
					cancel()
					return false
				}
			}
			return true
		}
		if opts.SendInitialSessionRuntimeSnapshot && !send() {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-changes:
				if changes == nil {
					continue
				}
				if !ok || !send() {
					return
				}
			case <-tickerC:
				if !send() {
					return
				}
			}
		}
	}()
}

func reportFrame(reportID string, report control.Report) ReportFrame {
	return ReportFrame{
		Kind:     KindReport,
		Version:  1,
		ReportID: reportID,
		Report:   report,
	}
}

func startCommandResultPump(ctx context.Context, cancel context.CancelFunc, writer *frameWriter, src control.Source, watcher CommandResultWatcher) {
	results, err := watcher.WatchCommandResults(ctx, src)
	if err != nil || results == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ack, ok := <-results:
				if !ok {
					return
				}
				if err := writer.write(CommandResultFrame{Kind: KindCommandResult, CommandID: ack.CommandID, CommandAck: ack}); err != nil {
					cancel()
					return
				}
			}
		}
	}()
}

func handlePayload(ctx context.Context, writer *frameWriter, src control.Source, service control.Service, payload []byte) error {
	frame, err := decodeFrame(payload)
	if err != nil {
		return writer.write(ErrorFrame{Kind: KindError, Error: invalidFrame("", "malformed JSON frame")})
	}
	switch frame.Kind {
	case KindCommand:
		return handleCommandFrame(ctx, writer, src, service, frame)
	case KindQuery:
		return handleQueryFrame(ctx, writer, src, service, frame)
	default:
		return writer.write(ErrorFrame{Kind: KindError, RequestID: frame.RequestID, CommandID: frame.CommandID, Error: control.ControlError{
			Code:    "unsupported_frame",
			Message: fmt.Sprintf("unsupported node-control frame kind %q", frame.Kind),
		}})
	}
}

func handleCommandFrame(ctx context.Context, writer *frameWriter, src control.Source, service control.Service, frame IncomingFrame) error {
	if frame.Command == nil {
		return writer.write(ErrorFrame{Kind: KindError, RequestID: frame.RequestID, CommandID: frame.CommandID, Error: invalidFrame("command", "command frame requires command payload")})
	}
	if frame.CommandID != "" && frame.CommandID != frame.Command.CommandID {
		return writer.write(ErrorFrame{Kind: KindError, RequestID: frame.RequestID, CommandID: frame.CommandID, Error: invalidFrame("command_id", "frame command id must match command payload")})
	}
	if frame.Command.CommandID == "" {
		return writer.write(ErrorFrame{Kind: KindError, RequestID: frame.RequestID, CommandID: frame.CommandID, Error: invalidFrame("command_id", "command id is required for remote commands")})
	}
	ack, err := service.HandleCommand(ctx, src, *frame.Command)
	if err != nil {
		return writer.write(ErrorFrame{Kind: KindError, RequestID: frame.RequestID, CommandID: frame.Command.CommandID, Error: internalFrameError("control command failed")})
	}
	if err := writer.write(AckFrame{Kind: KindAck, CommandID: ack.CommandID, CommandAck: ack}); err != nil {
		return err
	}
	if ack.OK && ack.Status == control.CommandStatusReceived {
		if deferred, ok := service.(control.DeferredCommandAction); ok {
			deferred.ConfirmCommandAckDelivered(ack.CommandID)
		}
	}
	return nil
}

func handleQueryFrame(ctx context.Context, writer *frameWriter, src control.Source, service control.Service, frame IncomingFrame) error {
	if frame.Query == nil {
		return writer.write(ErrorFrame{Kind: KindError, RequestID: frame.RequestID, Error: invalidFrame("query", "query frame requires query payload")})
	}
	result, err := service.HandleQuery(ctx, src, *frame.Query)
	if err != nil {
		return writer.write(ErrorFrame{Kind: KindError, RequestID: frame.RequestID, Error: internalFrameError("control query failed")})
	}
	return writer.write(QueryResponseFrame{Kind: KindResponse, RequestID: frame.RequestID, QueryResult: result})
}

func decodeFrame(payload []byte) (IncomingFrame, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var frame IncomingFrame
	if err := decoder.Decode(&frame); err != nil {
		return IncomingFrame{}, err
	}
	var extra struct{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return IncomingFrame{}, err
	}
	return frame, nil
}

func invalidFrame(target string, message string) control.ControlError {
	return control.ControlError{Code: control.ErrCodeInvalidArgument, Message: message, Target: target}
}

func internalFrameError(message string) control.ControlError {
	return control.ControlError{Code: control.ErrCodeInternal, Message: message}
}

func writeExit(err error) runtimes.Exit {
	return runtimes.TransientExit("node_control_write_failed", fmt.Sprintf("node-control websocket write failed: %v", err))
}

func closeOnContextDone(ctx context.Context, conn WebSocketConn) {
	<-ctx.Done()
	_ = conn.Close()
}

type frameWriter struct {
	mu   sync.Mutex
	conn WebSocketConn
}

func (w *frameWriter) write(frame any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteMessage(websocket.TextMessage, raw)
}
