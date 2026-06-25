package controltest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
)

type MockService struct {
	t testing.TB

	mu    sync.Mutex
	calls []expectedCall
}

type expectedCall struct {
	kind string
	src  *control.Source

	command *control.Command
	query   *control.Query

	ack    control.CommandAck
	result control.QueryResult
	err    error
}

type CommandExpectation struct {
	service *MockService
	index   int
}

type QueryExpectation struct {
	service *MockService
	index   int
}

func NewMockService(t testing.TB) *MockService {
	t.Helper()
	service := &MockService{t: t}
	t.Cleanup(service.Verify)
	return service
}

func (m *MockService) ExpectCommand(cmd control.Command) *CommandExpectation {
	m.t.Helper()
	return m.ExpectCommandFrom(control.Source{}, cmd)
}

func (m *MockService) ExpectCommandFrom(src control.Source, cmd control.Command) *CommandExpectation {
	m.t.Helper()
	if err := cmd.Validate(); err != nil {
		m.t.Fatalf("expected command does not validate: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, expectedCall{
		kind:    "command",
		src:     sourcePtr(src),
		command: &cmd,
	})
	return &CommandExpectation{service: m, index: len(m.calls) - 1}
}

func (m *MockService) ExpectQuery(query control.Query) *QueryExpectation {
	m.t.Helper()
	return m.ExpectQueryFrom(control.Source{}, query)
}

func (m *MockService) ExpectQueryFrom(src control.Source, query control.Query) *QueryExpectation {
	m.t.Helper()
	if err := query.Validate(); err != nil {
		m.t.Fatalf("expected query does not validate: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, expectedCall{
		kind:  "query",
		src:   sourcePtr(src),
		query: &query,
	})
	return &QueryExpectation{service: m, index: len(m.calls) - 1}
}

func (e *CommandExpectation) ReturnCommandAck(ack control.CommandAck) *MockService {
	e.service.t.Helper()
	e.service.mu.Lock()
	defer e.service.mu.Unlock()
	e.service.calls[e.index].ack = ack
	return e.service
}

func (e *CommandExpectation) ReturnError(err error) *MockService {
	e.service.t.Helper()
	e.service.mu.Lock()
	defer e.service.mu.Unlock()
	e.service.calls[e.index].err = err
	return e.service
}

func (e *QueryExpectation) ReturnQueryResult(result control.QueryResult) *MockService {
	e.service.t.Helper()
	e.service.mu.Lock()
	defer e.service.mu.Unlock()
	e.service.calls[e.index].result = result
	return e.service
}

func (e *QueryExpectation) ReturnError(err error) *MockService {
	e.service.t.Helper()
	e.service.mu.Lock()
	defer e.service.mu.Unlock()
	e.service.calls[e.index].err = err
	return e.service
}

func (m *MockService) HandleCommand(ctx context.Context, src control.Source, cmd control.Command) (control.CommandAck, error) {
	m.t.Helper()
	_ = ctx

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		m.t.Fatalf("unexpected command call: source=%+v command=%+v", src, cmd)
	}

	call := m.calls[0]
	m.calls = m.calls[1:]
	if call.kind != "command" {
		m.t.Fatalf("unexpected command call: next expectation is %s", call.kind)
	}
	if call.src != nil && !reflect.DeepEqual(*call.src, src) {
		m.t.Fatalf("unexpected command source:\nexpected: %+v\nactual:   %+v", *call.src, src)
	}
	if !reflect.DeepEqual(*call.command, cmd) {
		m.t.Fatalf("unexpected command:\nexpected: %+v\nactual:   %+v", *call.command, cmd)
	}
	return call.ack, call.err
}

func (m *MockService) HandleQuery(ctx context.Context, src control.Source, query control.Query) (control.QueryResult, error) {
	m.t.Helper()
	_ = ctx

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		m.t.Fatalf("unexpected query call: source=%+v query=%+v", src, query)
	}

	call := m.calls[0]
	m.calls = m.calls[1:]
	if call.kind != "query" {
		m.t.Fatalf("unexpected query call: next expectation is %s", call.kind)
	}
	if call.src != nil && !reflect.DeepEqual(*call.src, src) {
		m.t.Fatalf("unexpected query source:\nexpected: %+v\nactual:   %+v", *call.src, src)
	}
	if !reflect.DeepEqual(*call.query, query) {
		m.t.Fatalf("unexpected query:\nexpected: %+v\nactual:   %+v", *call.query, query)
	}
	return call.result, call.err
}

func (m *MockService) Verify() {
	m.t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return
	}
	next := m.calls[0]
	switch next.kind {
	case "command":
		m.t.Fatalf("mock service has %d unmet expectation(s); next command: %+v", len(m.calls), *next.command)
	case "query":
		m.t.Fatalf("mock service has %d unmet expectation(s); next query: %+v", len(m.calls), *next.query)
	default:
		m.t.Fatalf("mock service has %d unmet expectation(s)", len(m.calls))
	}
}

func sourcePtr(src control.Source) *control.Source {
	if src.Kind == "" && src.RemoteID == "" {
		return nil
	}
	return &src
}

var ErrMockService = errors.New("mock control service error")
