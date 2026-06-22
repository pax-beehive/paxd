package daemonstore

import "github.com/pax-beehive/paxd/internal/control"

var (
	_ control.Store       = (*Store)(nil)
	_ control.TxStore     = (*Store)(nil)
	_ control.StoreReader = (*Store)(nil)
)
