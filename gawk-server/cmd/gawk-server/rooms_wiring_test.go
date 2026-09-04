package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// chainHook is what lets the room registry ride the hub's publisher hooks
// without displacing whatever the cluster coordinator already installed
// (docs/44 §4.4 "Attachment"): both must run, in install order, and a
// nil first hook must not cost a wrapper that calls nil.
func TestChainHookRunsBothHooksInOrder(t *testing.T) {
	var calls []string
	a := func(id string) { calls = append(calls, "a:"+id) }
	b := func(id string) { calls = append(calls, "b:"+id) }

	chainHook(nil, b)("X")
	if len(calls) != 1 || calls[0] != "b:X" {
		t.Fatalf("nil first hook: calls = %v", calls)
	}
	calls = nil
	chainHook(a, b)("Y")
	if len(calls) != 2 || calls[0] != "a:Y" || calls[1] != "b:Y" {
		t.Fatalf("both hooks: calls = %v, want a then b", calls)
	}
}

// nopConn is the least a hub subscriber needs: the room adapter only
// counts it, no media ever reaches it here.
type nopConn struct{}

func (nopConn) SendDatagram([]byte) error                       { return nil }
func (nopConn) OpenKeyframeStream() (hub.KeyframeStream, error) { return nil, io.ErrClosedPipe }
func (nopConn) OpenCarrierStream() (hub.KeyframeStream, error)  { return nil, io.ErrClosedPipe }
func (nopConn) CloseWithError(uint32, string) error             { return nil }

// hubBroadcasts is the registry's only view of a broadcast (docs/44 D1:
// an attachment is a broadcast ID the hub is asked about). Against a real
// hub: unknown → not known; publishing → live with the viewer count;
// publisher gone within the grace → known but away.
func TestHubBroadcastsReportsLiveViewersAndKnown(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := hub.NewRegistry(log, hub.Options{BroadcastGrace: time.Hour})
	src := hubBroadcasts{r}

	if st, known := src.BroadcastState("ZZZZZZ"); known || st.Live || st.Viewers != 0 {
		t.Fatalf("unknown broadcast: %+v, known=%v", st, known)
	}
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if st, known := src.BroadcastState(id); !known || !st.Live || st.Viewers != 0 {
		t.Fatalf("fresh publisher: %+v, known=%v", st, known)
	}
	if _, err := r.Subscribe(id, nopConn{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if st, known := src.BroadcastState(id); !known || !st.Live || st.Viewers != 1 {
		t.Fatalf("with a viewer: %+v, known=%v", st, known)
	}
	p.Close()
	if st, known := src.BroadcastState(id); !known || st.Live || st.Viewers != 1 {
		t.Fatalf("publisher away within the grace: %+v, known=%v", st, known)
	}
}
