package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// docs/76 PP5/PP7: every gawk-relay session learns it is on a local relay.
// An unpaired relay's 0x18 names it and has linked clear.
func TestStandaloneRelaySendsLocalRelayInfo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stateDir := filepath.Join(t.TempDir(), "state")
	port := freeUDPPort(t)
	stop := startRelay(t, ctx, stateDir, port)
	defer stop()

	sess := dialRetry(t, ctx, dialer(t, pinnedTLS(t, stateDir)), fmt.Sprintf("https://127.0.0.1:%d/echo", port))
	defer sess.CloseWithError(0, "")
	for {
		actx, acancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := sess.AcceptUniStream(actx)
		acancel()
		if err != nil {
			t.Fatalf("no LocalRelayInfo on /echo: %v", err)
		}
		data, _ := io.ReadAll(st)
		if len(data) < 2 || data[1] != wire.TypeLocalRelayInfo {
			continue
		}
		info, err := wire.ParseLocalRelayInfo(data)
		if err != nil {
			t.Fatal(err)
		}
		if info.Linked || info.HomeOrigin != "" || info.Name == "" {
			t.Fatalf("standalone 0x18 = %+v", info)
		}
		if popLink == nil || popLink.State().Header != "standalone" {
			t.Fatalf("link state = %+v", popLink)
		}
		return
	}
}
