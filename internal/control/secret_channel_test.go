package control_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/secretchannel"
	"github.com/stretchr/testify/require"
)

func newTestSecretChannelRegistry(t *testing.T, writer secretchannel.Writer) *secretchannel.Registry {
	t.Helper()
	if writer == nil {
		writer = func(plaintext []byte) (string, time.Time, error) {
			return "file:/tmp/secretchannel-control-test", time.Now().Add(time.Minute), nil
		}
	}
	return secretchannel.NewRegistry(secretchannel.Options{
		NodeID: "node_1",
		Writer: writer,
	})
}

func sealPushCommand(t *testing.T, open control.QueryResult, commandID string, plaintext []byte) control.Command {
	t.Helper()
	require.NotNil(t, open.SecretChannelOpen)
	pub, err := base64.StdEncoding.DecodeString(open.SecretChannelOpen.PublicKey)
	require.NoError(t, err)
	expiresAt, err := time.Parse(time.RFC3339, open.SecretChannelOpen.ExpiresAt)
	require.NoError(t, err)

	sealed, err := secretchannel.SealSecret(pub, plaintext, secretchannel.PushContext{
		NodeID:        "node_1",
		ChannelID:     open.SecretChannelOpen.ChannelID,
		CommandID:     commandID,
		ExpiresAtUnix: expiresAt.Unix(),
	}, rand.Reader)
	require.NoError(t, err)

	return control.Command{
		CommandID: commandID,
		Type:      control.CommandSecretChannelPush,
		PushSecretChannel: &control.PushSecretChannelCommand{
			ChannelID:       open.SecretChannelOpen.ChannelID,
			SenderPublicKey: base64.StdEncoding.EncodeToString(sealed.SenderPublicKey),
			Nonce:           base64.StdEncoding.EncodeToString(sealed.Nonce),
			Ciphertext:      base64.StdEncoding.EncodeToString(sealed.Ciphertext),
		},
	}
}

func TestServiceOpenSecretChannelReturnsPublicKey(t *testing.T) {
	service := control.NewService(control.ServiceOptions{SecretChannel: newTestSecretChannelRegistry(t, nil)})

	result, err := service.HandleQuery(context.Background(), control.Source{Kind: control.SourceRemote, RemoteID: "remote_a"}, control.Query{
		Type:              control.QuerySecretChannelOpen,
		OpenSecretChannel: &control.OpenSecretChannelQuery{},
	})
	require.NoError(t, err)
	require.Nil(t, result.Error)
	require.NotNil(t, result.SecretChannelOpen)
	require.NotEmpty(t, result.SecretChannelOpen.ChannelID)
	require.NotEmpty(t, result.SecretChannelOpen.PublicKey)
	require.NotEmpty(t, result.SecretChannelOpen.ExpiresAt)
}

func TestServiceOpenSecretChannelNotConfiguredReturnsInternalError(t *testing.T) {
	service := control.NewService(control.ServiceOptions{})

	result, err := service.HandleQuery(context.Background(), control.Source{Kind: control.SourceRemote, RemoteID: "remote_a"}, control.Query{
		Type:              control.QuerySecretChannelOpen,
		OpenSecretChannel: &control.OpenSecretChannelQuery{},
	})
	require.NoError(t, err)
	require.NotNil(t, result.Error)
	require.Equal(t, control.ErrCodeInternal, result.Error.Code)
}

func TestServicePushSecretChannelAppliesAndPersistsFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	drop := secretchannel.FileDrop{Dir: dir, TTL: 10 * time.Minute}
	service := control.NewService(control.ServiceOptions{SecretChannel: newTestSecretChannelRegistry(t, drop.Write)})
	src := control.Source{Kind: control.SourceRemote, RemoteID: "remote_a"}

	open, err := service.HandleQuery(ctx, src, control.Query{Type: control.QuerySecretChannelOpen, OpenSecretChannel: &control.OpenSecretChannelQuery{}})
	require.NoError(t, err)

	cmd := sealPushCommand(t, open, "cmd_push_1", []byte("sk-super-secret"))
	ack, err := service.HandleCommand(ctx, src, cmd)
	require.NoError(t, err)
	require.True(t, ack.OK)
	require.Equal(t, control.CommandStatusApplied, ack.Status)
	require.NotNil(t, ack.Result)
	require.NotNil(t, ack.Result.SecretChannelPush)
	require.NotEmpty(t, ack.Result.SecretChannelPush.FileRef)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	got, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	require.NoError(t, err)
	require.Equal(t, []byte("sk-super-secret"), got)
}

func TestServicePushSecretChannelUnknownChannelIsRejectedAsExpired(t *testing.T) {
	ctx := context.Background()
	service := control.NewService(control.ServiceOptions{SecretChannel: newTestSecretChannelRegistry(t, nil)})
	src := control.Source{Kind: control.SourceRemote, RemoteID: "remote_a"}

	ack, err := service.HandleCommand(ctx, src, control.Command{
		CommandID: "cmd_push_1",
		Type:      control.CommandSecretChannelPush,
		PushSecretChannel: &control.PushSecretChannelCommand{
			ChannelID:       "chan_does_not_exist",
			SenderPublicKey: base64.StdEncoding.EncodeToString([]byte("not-empty")),
			Nonce:           base64.StdEncoding.EncodeToString([]byte("not-empty")),
			Ciphertext:      base64.StdEncoding.EncodeToString([]byte("not-empty")),
		},
	})
	require.NoError(t, err)
	require.False(t, ack.OK)
	require.Equal(t, control.CommandStatusRejected, ack.Status)
	require.NotNil(t, ack.Error)
	require.Equal(t, "expired", ack.Error.Code)
}

func TestServicePushSecretChannelMalformedBase64IsRejectedAsInvalidArgument(t *testing.T) {
	ctx := context.Background()
	service := control.NewService(control.ServiceOptions{SecretChannel: newTestSecretChannelRegistry(t, nil)})
	src := control.Source{Kind: control.SourceRemote, RemoteID: "remote_a"}

	open, err := service.HandleQuery(ctx, src, control.Query{Type: control.QuerySecretChannelOpen, OpenSecretChannel: &control.OpenSecretChannelQuery{}})
	require.NoError(t, err)

	ack, err := service.HandleCommand(ctx, src, control.Command{
		CommandID: "cmd_push_1",
		Type:      control.CommandSecretChannelPush,
		PushSecretChannel: &control.PushSecretChannelCommand{
			ChannelID:       open.SecretChannelOpen.ChannelID,
			SenderPublicKey: "not-base64!!!",
			Nonce:           base64.StdEncoding.EncodeToString([]byte("nonce123456789")),
			Ciphertext:      base64.StdEncoding.EncodeToString([]byte("ciphertext")),
		},
	})
	require.NoError(t, err)
	require.False(t, ack.OK)
	require.Equal(t, control.CommandStatusRejected, ack.Status)
	require.NotNil(t, ack.Error)
	require.Equal(t, control.ErrCodeInvalidArgument, ack.Error.Code)
}

func TestServicePushSecretChannelMissingChannelIDIsRejectedByValidate(t *testing.T) {
	ctx := context.Background()
	service := control.NewService(control.ServiceOptions{SecretChannel: newTestSecretChannelRegistry(t, nil)})

	ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceRemote, RemoteID: "remote_a"}, control.Command{
		CommandID: "cmd_push_1",
		Type:      control.CommandSecretChannelPush,
		PushSecretChannel: &control.PushSecretChannelCommand{
			SenderPublicKey: base64.StdEncoding.EncodeToString([]byte("not-empty")),
			Nonce:           base64.StdEncoding.EncodeToString([]byte("not-empty")),
			Ciphertext:      base64.StdEncoding.EncodeToString([]byte("not-empty")),
		},
	})
	require.NoError(t, err)
	require.False(t, ack.OK)
	require.Equal(t, control.CommandStatusRejected, ack.Status)
	require.Equal(t, control.ErrCodeInvalidArgument, ack.Error.Code)
}

func TestServicePushSecretChannelDifferentSourceIsUnauthorizedButOwnerCanStillUse(t *testing.T) {
	ctx := context.Background()
	service := control.NewService(control.ServiceOptions{SecretChannel: newTestSecretChannelRegistry(t, nil)})
	owner := control.Source{Kind: control.SourceRemote, RemoteID: "remote_owner"}
	attacker := control.Source{Kind: control.SourceRemote, RemoteID: "remote_attacker"}

	open, err := service.HandleQuery(ctx, owner, control.Query{Type: control.QuerySecretChannelOpen, OpenSecretChannel: &control.OpenSecretChannelQuery{}})
	require.NoError(t, err)
	cmd := sealPushCommand(t, open, "cmd_push_1", []byte("sk-secret"))

	attackerAck, err := service.HandleCommand(ctx, attacker, cmd)
	require.NoError(t, err)
	require.False(t, attackerAck.OK)
	require.Equal(t, "unauthorized", attackerAck.Error.Code)

	ownerCmd := cmd
	ownerCmd.CommandID = "cmd_push_2"
	ownerCmd = sealPushCommand(t, open, "cmd_push_2", []byte("sk-secret"))
	ownerAck, err := service.HandleCommand(ctx, owner, ownerCmd)
	require.NoError(t, err)
	require.True(t, ownerAck.OK)
	require.Equal(t, control.CommandStatusApplied, ownerAck.Status)
}
