package main

import "testing"

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

// The registry's broadcast source is the transport's (RoomBroadcasts):
// its single-pod and cluster shapes are pinned in internal/transport
// (TestRoomBroadcastsAnswersLocalHubThenOriginLease).
