package transport

// R79 MV3 in cluster mode (docs/80 D2, D7, D12): two in-process pods over
// the fake control plane and a fake bridge. The keeper homes a static voice
// room nobody has joined by the ordinary lease claim — homed, observed, and
// still not live — a session link reaching the non-home pod is proxied to
// the home, the cluster-PSK route is fenced on the lease generation, and
// after the home drains the other pod's keeper takes the room over at the
// next generation, which is the epoch every new ticket carries.

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func staticVoiceRoomObject(t *testing.T, name, channel string) *unstructured.Unstructured {
	t.Helper()
	r := &rooms.Room{TypeMeta: metav1.TypeMeta{APIVersion: rooms.SchemeGroupVersion.String(), Kind: rooms.Kind},
		Spec: rooms.RoomSpec{Kind: rooms.KindStatic, DisplayCode: "Friends",
			Integrations: rooms.Integrations{Mumble: &rooms.MumbleIntegration{Channel: channel}}}}
	r.Name = name
	r.Namespace = "gawk"
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(r)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: raw}
}

func TestVoiceKeeperHomesAStaticRoomAndRehomesAfterDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newRoomFleet(t, staticVoiceRoomObject(t, "friends", "Friends/Gaming"))
	f.voiceBridge = startFakeBridge(t)
	aCtx, aCancel := context.WithCancel(ctx)
	defer aCancel()
	a := f.startRoomPod(t, aCtx, "pod-a")

	// Homed with nobody joined: observed, but not live.
	obs := f.voiceBridge.nextLink(t)
	if obs.observe.Code != "friends" || obs.observe.Channel != "Friends/Gaming" || obs.observe.Epoch != 1 {
		t.Fatalf("observe = %+v", obs.observe)
	}
	if gen, held := a.store.Holding("friends"); !held || gen != 1 {
		t.Fatalf("pod-a holding = %d %v", gen, held)
	}
	if got := a.reg.AdminRooms(); len(got) != 0 {
		t.Fatalf("a room homed only for voice is live in the R49 API: %+v", got)
	}
	b := f.startRoomPod(t, ctx, "pod-b")
	waitFor(t, 15*time.Second, func() bool {
		home, ok := b.store.Resolve("friends")
		return ok && home.Live && home.Holder == "pod-a"
	}, "pod-b to resolve the home")
	obs.roster(t, voice.Message{Available: true, Users: []voice.User{{Session: 3, Name: "native"}}})
	waitFor(t, 5*time.Second, func() bool { return voiceStats(a.reg).VoiceObserver == "available" }, "observer available")
	if _, held := b.store.Holding("friends"); held {
		t.Fatal("pod-b's keeper took a room with a live home")
	}

	// A participant on pod-b is proxied to the home and gets the voice.
	c := openVoiceControl(t, ctx, b.url("/room/friends"), f.clientTLS, "alice")
	if st := c.nextState(t); st.Caps != wire.RoomCapVoice {
		t.Fatalf("proxied caps = 0x%02x", st.Caps)
	}
	if e := c.nextVoice(t, wire.RoomEventVoiceRosterUpdated); e.VoiceFlags != wire.RoomVoiceRosterFlagAvailable {
		t.Fatalf("proxied roster = %+v", e)
	}
	ticket := voiceTicket(t, c)

	// The cluster-PSK route: fenced on the generation, and only at the home.
	expectStatus(t, ctx, a.url("/internal/voice/friends?psk=fleet-psk&gen=9"), f.clientTLS, http.StatusConflict)
	expectStatus(t, ctx, b.url("/internal/voice/friends?psk=fleet-psk&gen=1"), f.clientTLS, http.StatusNotFound)

	// The bridge's session link lands on pod-b and is proxied home.
	l, acc := register(t, ctx, b.url("/internal/voice/friends?psk="+testVoicePSK), f.clientTLS, ticket, "198.51.100.1:1")
	if acc.Type != voice.TypeAccepted || acc.Channel != "Friends/Gaming" {
		t.Fatalf("proxied accept = %+v", acc)
	}
	waitFor(t, 5*time.Second, func() bool { return voiceStats(a.reg).VoiceSessions == 1 }, "the home's voice session")
	// A newer registration, straight at the home: the older one's Close
	// travels back through the proxy, code and all.
	l2, acc2 := register(t, ctx, a.url("/internal/voice/friends?psk="+testVoicePSK), f.clientTLS, voiceTicket(t, c), "198.51.100.1:2")
	if acc2.Type != voice.TypeAccepted {
		t.Fatalf("direct accept = %+v", acc2)
	}
	l.expectClose(t, wire.CloseCodeVoiceRemoved)
	if code := sessionCloseCode(t, l.sess); code != wire.CloseCodeVoiceRemoved {
		t.Fatalf("proxied link close = %d, want the home's 4015", code)
	}
	_ = l2

	// The home drains: pod-b's keeper takes the room at generation 2 and
	// the bridge sees a new observer with the new epoch.
	aCancel()
	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		t.Fatal("pod-a did not drain")
	}
	again := f.voiceBridge.nextLink(t)
	for again.observe.Epoch != 2 {
		again = f.voiceBridge.nextLink(t)
	}
	if gen, held := b.store.Holding("friends"); !held || gen != 2 {
		t.Fatalf("pod-b holding = %d %v", gen, held)
	}
	// A ticket from the old home's epoch is refused retryably.
	if _, m := register(t, ctx, b.url("/internal/voice/friends?psk="+testVoicePSK), f.clientTLS, ticket, ""); m.Type != voice.TypeClose || m.CloseCode != wire.CloseCodeVoiceUnavailable {
		t.Fatalf("old-epoch ticket = %+v", m)
	}
}
