package control_test

import (
	"context"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachmentGivenEnsureCommandWhenHandledThenDownloadStartsAndAckReturns(t *testing.T) {
	store := openControlTestStore(t)
	localizer := &fakeAttachmentLocalizer{}
	service := control.NewService(control.ServiceOptions{Store: store, Attachments: localizer})
	command := control.Command{
		CommandID: "cmd_attachment_1",
		Type:      control.CommandAttachmentEnsureLocal,
		EnsureAttachmentLocal: &control.EnsureAttachmentLocalCommand{
			Attachment: control.AttachmentDescriptor{
				AttachmentID: "att_1",
				Filename:     "notes.txt",
				SizeBytes:    5,
				Generation:   7,
			},
			Download: control.AttachmentDownloadTicket{URL: "https://download.example/notes.txt"},
		},
	}

	source := control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"}
	ack, err := service.HandleCommand(context.Background(), source, command)
	require.NoError(t, err)
	require.True(t, ack.OK)
	assert.Equal(t, control.CommandStatusReceived, ack.Status)
	assert.Equal(t, "attachment", ack.TargetType)
	assert.Equal(t, "att_1", ack.TargetID)
	require.Len(t, localizer.ensure, 1)
	assert.Equal(t, "att_1", localizer.ensure[0].Attachment.AttachmentID)
	require.Len(t, localizer.ensureSources, 1)
	assert.Equal(t, source, localizer.ensureSources[0])
}

func TestAttachmentGivenReadyFileWhenStatusQueriedThenLocalURIIsReturned(t *testing.T) {
	store := openControlTestStore(t)
	localizer := &fakeAttachmentLocalizer{states: []control.AttachmentLocalState{{
		AttachmentID: "att_1",
		State:        control.AttachmentLocalReady,
		LocalURI:     "file:///tmp/attachments/att_1/notes.txt",
		SizeBytes:    5,
	}}}
	service := control.NewService(control.ServiceOptions{Store: store, Attachments: localizer})

	source := control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"}
	result, err := service.HandleQuery(context.Background(), source, control.Query{
		Type: control.QueryAttachmentLocalStatus,
		GetAttachmentLocalStatus: &control.GetAttachmentLocalStatusQuery{
			AttachmentIDs: []string{"att_1"},
		},
	})
	require.NoError(t, err)
	require.Nil(t, result.Error)
	require.NotNil(t, result.AttachmentLocalStatus)
	require.Len(t, result.AttachmentLocalStatus.Items, 1)
	assert.Equal(t, control.AttachmentLocalReady, result.AttachmentLocalStatus.Items[0].State)
	assert.Equal(t, "file:///tmp/attachments/att_1/notes.txt", result.AttachmentLocalStatus.Items[0].LocalURI)
	require.Len(t, localizer.statusSources, 1)
	assert.Equal(t, source, localizer.statusSources[0])
}

func TestAttachmentGivenDuplicateCommandIDFromAnotherRemoteWhenHandledThenItIsRejected(t *testing.T) {
	store := openControlTestStore(t)
	localizer := &fakeAttachmentLocalizer{}
	service := control.NewService(control.ServiceOptions{Store: store, Attachments: localizer})
	command := control.Command{
		CommandID: "cmd_attachment_shared",
		Type:      control.CommandAttachmentEnsureLocal,
		EnsureAttachmentLocal: &control.EnsureAttachmentLocalCommand{
			Attachment: control.AttachmentDescriptor{
				AttachmentID: "att_shared",
				Filename:     "notes.txt",
				SizeBytes:    5,
			},
			Download: control.AttachmentDownloadTicket{URL: "https://download.example/notes.txt"},
		},
	}

	first, err := service.HandleCommand(
		context.Background(),
		control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"},
		command,
	)
	require.NoError(t, err)
	require.True(t, first.OK)
	duplicate, err := service.HandleCommand(
		context.Background(),
		control.Source{Kind: control.SourceRemote, RemoteID: "remote_staging"},
		command,
	)

	require.NoError(t, err)
	assert.False(t, duplicate.OK)
	assert.Equal(t, control.CommandStatusRejected, duplicate.Status)
	require.NotNil(t, duplicate.Error)
	assert.Equal(t, control.ErrCodeConflict, duplicate.Error.Code)
	assert.Contains(t, duplicate.Error.Message, "different remote source")
	require.Len(t, localizer.ensureSources, 1)
	assert.Equal(t, "remote_prod", localizer.ensureSources[0].RemoteID)
}

func TestAttachmentGivenLocalCommandWhenHandledThenExplicitLocalScopeIsForwarded(t *testing.T) {
	store := openControlTestStore(t)
	localizer := &fakeAttachmentLocalizer{}
	service := control.NewService(control.ServiceOptions{Store: store, Attachments: localizer})
	command := control.Command{
		CommandID: "cmd_attachment_local",
		Type:      control.CommandAttachmentEnsureLocal,
		EnsureAttachmentLocal: &control.EnsureAttachmentLocalCommand{
			Attachment: control.AttachmentDescriptor{
				AttachmentID: "att_local",
				Filename:     "notes.txt",
				SizeBytes:    5,
			},
			Download: control.AttachmentDownloadTicket{URL: "https://download.example/notes.txt"},
		},
	}

	ack, err := service.HandleCommand(context.Background(), control.Source{Kind: control.SourceLocal}, command)

	require.NoError(t, err)
	require.True(t, ack.OK)
	require.Len(t, localizer.ensureSources, 1)
	assert.Equal(t, control.Source{Kind: control.SourceLocal}, localizer.ensureSources[0])
}

type fakeAttachmentLocalizer struct {
	ensure        []control.EnsureAttachmentLocalCommand
	ensureSources []control.Source
	statusSources []control.Source
	states        []control.AttachmentLocalState
}

func (f *fakeAttachmentLocalizer) Ensure(_ context.Context, source control.Source, command control.EnsureAttachmentLocalCommand) error {
	f.ensureSources = append(f.ensureSources, source)
	f.ensure = append(f.ensure, command)
	return nil
}

func (f *fakeAttachmentLocalizer) Status(_ context.Context, source control.Source, attachmentIDs []string) ([]control.AttachmentLocalState, error) {
	f.statusSources = append(f.statusSources, source)
	if len(f.states) > 0 {
		return append([]control.AttachmentLocalState(nil), f.states...), nil
	}
	items := make([]control.AttachmentLocalState, 0, len(attachmentIDs))
	for _, id := range attachmentIDs {
		items = append(items, control.AttachmentLocalState{AttachmentID: id, State: control.AttachmentLocalUnknown})
	}
	return items, nil
}
