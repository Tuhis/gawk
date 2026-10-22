package main

import (
	"context"

	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/serve"
)

func init() { registerPart(popPart) }

// popLink is the relay's PoP link client, built in the pre-run hook (it
// needs the server and the registry serve.Run builds). The console reads
// it for its pairing and header sections.
var popLink *popclient.Client

// popPart links the relay to a home fleet when it holds a credential or
// was given -pair (docs/76 §6.2-6.3), and installs the 0x18 LocalRelayInfo
// provider every client session reads, linked or standalone (item 45).
func popPart(r *Relay) ([]func(ctx context.Context, w serve.Wired) error, []func(w serve.Wired) error, error) {
	pre := func(w serve.Wired) error {
		popLink = popclient.New(popclient.Options{
			StateDir: r.StateDir,
			Version:  version,
			Pair:     r.Relay.Pair,
			Registry: w.Registry,
			Server:   w.Server,
			Rolling:  r.Rolling,
			LAN:      r.LAN,
			Relay:    r.Relay,
			Cfg:      r.Cfg,
			Log:      r.Log,
		})
		w.Server.SetLocalRelay(popLink)
		return nil
	}
	run := func(ctx context.Context, _ serve.Wired) error {
		if popLink == nil {
			return nil
		}
		return popLink.Run(ctx)
	}
	return []func(context.Context, serve.Wired) error{run}, []func(serve.Wired) error{pre}, nil
}
