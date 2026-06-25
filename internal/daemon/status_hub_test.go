package daemon

import (
	"testing"
)

func TestStatusHubRoutesPokesByRemote(t *testing.T) {
	hub := newStatusHub()
	prod, unsubProd := hub.Subscribe("remote_prod")
	defer unsubProd()
	staging, unsubStaging := hub.Subscribe("remote_staging")
	defer unsubStaging()

	hub.Poke("remote_prod")

	select {
	case <-prod:
	default:
		t.Fatal("remote_prod subscriber did not receive poke")
	}
	select {
	case <-staging:
		t.Fatal("remote_staging subscriber received remote_prod poke")
	default:
	}
}

func TestStatusHubDropsPokesAfterUnsubscribe(t *testing.T) {
	hub := newStatusHub()
	ch, unsubscribe := hub.Subscribe("remote_prod")
	unsubscribe()

	hub.Poke("remote_prod")

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unsubscribed channel received poke")
		}
	default:
		t.Fatal("unsubscribed channel was not closed")
	}
}
