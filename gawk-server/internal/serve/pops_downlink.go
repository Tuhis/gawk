package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopDownlink wires the home side of the R74 downlink (docs/76 §6.6).
// wirePops calls it with -pops on. There is nothing to install: the
// /pop/subscribe handler authenticates through the manager wirePops has
// already set with SetPopHome, reads each PoP's spec.limits from the Pop
// it returns, and serves the leg from the registry and edge manager the
// server already has. The PoP side needs no wiring either: a relay's
// popclient.Client, installed with SetLocalRelay, is its downlink source.
func wirePopDownlink(_ context.Context, _ Wired, _ *popsrv.Manager, _ *slog.Logger) error {
	return nil
}
