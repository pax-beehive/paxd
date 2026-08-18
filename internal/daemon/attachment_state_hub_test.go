package daemon

import (
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachmentStateHubGivenTwoRemotesWhenStatePublishedThenOnlyOwningRemoteReceivesIt(t *testing.T) {
	hub := newAttachmentStateHub()
	prod, unsubscribeProd := hub.Subscribe("remote_prod")
	defer unsubscribeProd()
	staging, unsubscribeStaging := hub.Subscribe("remote_staging")
	defer unsubscribeStaging()
	want := control.AttachmentLocalState{
		AttachmentID: "att_shared",
		State:        control.AttachmentLocalReady,
	}

	hub.Publish(control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"}, want)

	select {
	case got := <-prod:
		assert.Equal(t, want, got)
	default:
		require.Fail(t, "owning remote did not receive attachment state")
	}
	select {
	case <-staging:
		require.Fail(t, "another remote received attachment state")
	default:
	}
}

func TestAttachmentStateHubGivenLocalStateWhenPublishedThenNoRemoteReceivesIt(t *testing.T) {
	hub := newAttachmentStateHub()
	remote, unsubscribe := hub.Subscribe("remote_prod")
	defer unsubscribe()

	hub.Publish(control.Source{Kind: control.SourceLocal}, control.AttachmentLocalState{
		AttachmentID: "att_local",
		State:        control.AttachmentLocalReady,
	})

	select {
	case <-remote:
		require.Fail(t, "remote received a local-only attachment state")
	default:
	}
}

func TestAttachmentStateHubGivenEmptySubscriptionWhenSubscribedThenChannelIsClosed(t *testing.T) {
	hub := newAttachmentStateHub()
	states, unsubscribe := hub.Subscribe("")
	defer unsubscribe()

	_, open := <-states
	assert.False(t, open)
}
