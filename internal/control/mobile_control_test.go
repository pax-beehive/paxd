package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeMobileControl struct {
	calls  int
	source string
	err    error
}

func (f *fakeMobileControl) Request(_ context.Context, source, operation string, payload json.RawMessage) (json.RawMessage, error) {
	f.calls++
	f.source = source
	return json.RawMessage(`{"available":true}`), f.err
}

func TestMobileQueryValidationAndSourceAuthorization(t *testing.T) {
	valid := Query{Type: QueryMobileControl, MobileControl: &MobileControlQuery{Operation: "state", Payload: json.RawMessage(`{}`)}}
	require.NoError(t, valid.Validate())
	for _, invalid := range []Query{
		{Type: QueryMobileControl},
		{Type: QueryMobileControl, MobileControl: &MobileControlQuery{Operation: "live"}},
		{Type: QueryMobileControl, MobileControl: &MobileControlQuery{Operation: "state", Payload: json.RawMessage(`{`)}},
		{Type: QueryMobileControl, MobileControl: &MobileControlQuery{Operation: "state", Payload: json.RawMessage(strings.Repeat("x", 16385))}},
		{Type: QueryBrowserControl, MobileControl: &MobileControlQuery{Operation: "state"}},
		{Type: QueryMobileControl, MobileControl: &MobileControlQuery{Operation: "state"}, BrowserControl: &BrowserControlQuery{Operation: "state"}},
	} {
		require.Error(t, invalid.Validate())
	}
	for _, src := range []Source{{}, {Kind: SourceRemote}, {Kind: SourceKind("unknown")}} {
		fake := &fakeMobileControl{}
		result, err := NewService(ServiceOptions{MobileControl: fake}).HandleQuery(context.Background(), src, valid)
		require.NoError(t, err)
		require.NotNil(t, result.Error)
		require.Equal(t, 0, fake.calls)
	}
	for _, src := range []Source{{Kind: SourceLocal}, {Kind: SourceRemote, RemoteID: "owner"}} {
		fake := &fakeMobileControl{}
		result, err := NewService(ServiceOptions{MobileControl: fake}).HandleQuery(context.Background(), src, valid)
		require.NoError(t, err)
		require.Nil(t, result.Error)
		require.JSONEq(t, `{"available":true}`, string(result.MobileControl))
		require.Equal(t, string(src.Kind)+":"+src.RemoteID, fake.source)
	}
	result, err := NewService(ServiceOptions{MobileControl: &fakeMobileControl{err: errors.New("mobile unavailable")}}).HandleQuery(context.Background(), Source{Kind: SourceLocal}, valid)
	require.NoError(t, err)
	require.Equal(t, "mobile unavailable", result.Error.Message)
}
