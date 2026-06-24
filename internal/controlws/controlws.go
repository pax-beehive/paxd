package controlws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

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

type CommandResultWatcher interface {
	WatchCommandResults(ctx context.Context, src control.Source) (<-chan control.CommandAck, error)
}

type Runner struct {
	Service control.Service
}

func NewRunner(service control.Service) Runner {
	return Runner{Service: service}
}

func (r Runner) RunNodeControl(ctx context.Context, conn runtimes.WebSocketConn, spec runtimes.RemoteSpec) runtimes.Exit {
	return Run(ctx, conn, control.Source{Kind: control.SourceRemote, RemoteID: spec.RemoteID}, r.Service)
}

func Run(ctx context.Context, conn WebSocketConn, src control.Source, service control.Service) runtimes.Exit {
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
	return writer.write(AckFrame{Kind: KindAck, CommandID: ack.CommandID, CommandAck: ack})
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
