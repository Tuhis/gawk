package popsrv

// The PP9 acceptance rows (docs/76 §11) in-process: home pods on the fake
// clientset and one linked PoP, whose party viewers watch home broadcasts
// over /pop/subscribe (docs/76 §6.6, items 23-25).

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// --- media helpers ------------------------------------------------------

func dlKeyframe(t *testing.T, frameID uint32, payloadLen int) []byte {
	t.Helper()
	cfg, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: "avc1.42E02A", Extradata: []byte{0x01, 0x42, 0xE0, 0x2A}})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{byte(frameID)}, payloadLen)
	msg, err := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{
		Keyframe: true, FrameID: frameID, TimestampUs: uint64(frameID) * 16_667,
		ConfigLen: uint32(len(cfg)), PayloadLen: uint32(len(payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(append(msg, cfg...), payload...)
}

func dlDelta(t *testing.T, frameID uint32) []byte {
	t.Helper()
	d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
		FrameID: frameID, ChunkIndex: 0, ChunkCount: 1, TimestampUs: uint64(frameID) * 16_667,
	}, bytes.Repeat([]byte{byte(frameID)}, 600))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func sendKeyframe(t *testing.T, pub *webtransport.Session, msg []byte) {
	t.Helper()
	st, err := pub.OpenUniStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(msg); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// homeBroadcast publishes on a home pod and returns the session and ID.
func homeBroadcast(t *testing.T, f *HomeFleet, pod *HomePod) (*webtransport.Session, string) {
	t.Helper()
	pub := clientDial(t, f, "https://"+pod.Addr()+"/publish")
	t.Cleanup(func() { pub.CloseWithError(0, "") })
	id, _ := publisherHandshake(t, pairCtx(t), pub)
	return pub, id
}

// statusDial dials target as a client, returning the session or the
// refusal's status.
func statusDial(t *testing.T, f *HomeFleet, target string, h http.Header) (*webtransport.Session, int) {
	t.Helper()
	d := &webtransport.Transport{
		TLSClientConfig: f.ClientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })
	rsp, sess, err := d.Dial(pairCtx(t), target, h)
	if err != nil {
		if rsp == nil {
			t.Fatalf("dial %s: %v", target, err)
		}
		return nil, rsp.StatusCode
	}
	t.Cleanup(func() { sess.CloseWithError(0, "") })
	return sess, http.StatusOK
}

// partyViewer joins a broadcast on the PoP.
func partyViewer(t *testing.T, f *HomeFleet, pop *PoP, id string) *webtransport.Session {
	t.Helper()
	sess, status := statusDial(t, f, "https://"+pop.Addr()+"/subscribe/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("party viewer of %s: status %d", id, status)
	}
	return sess
}

// readKeyframeFrameID reads uni streams until a keyframe (skipping the
// notices a viewer also gets on uni streams) and returns its frameID.
func readKeyframeFrameID(t *testing.T, sess *webtransport.Session) uint32 {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := sess.AcceptUniStream(ctx)
		cancel()
		if err != nil {
			t.Fatalf("no keyframe: %v", err)
		}
		data, _ := io.ReadAll(st)
		if _, typ, err := wire.PeekType(data); err == nil && typ == wire.TypeStreamFrame {
			hdr, err := wire.ParseStreamFrameHeader(data)
			if err != nil {
				t.Fatal(err)
			}
			return hdr.FrameID
		}
	}
}

// popCredential is the PoP's stored link credential.
func popCredential(t *testing.T, pop *PoP) string {
	t.Helper()
	raw, err := relaystate.LoadSecret(pop.StateDir, popclient.CredentialSecret)
	if err != nil {
		t.Fatal(err)
	}
	var st struct{ Credential string }
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st.Credential
}

// rawLeg dials /pop/subscribe on a pod as the PoP's downlink does.
func rawLeg(t *testing.T, f *HomeFleet, pod *HomePod, pop *PoP, id string, q url.Values) (*webtransport.Session, int, string) {
	t.Helper()
	if q == nil {
		q = url.Values{}
	}
	if !q.Has("proto") {
		q.Set("proto", poplink.ProtoQuery())
	}
	h := http.Header{"Authorization": {"Bearer " + popCredential(t, pop)}}
	return rawDial(t, f, pod, "/pop/subscribe/"+id+"?"+q.Encode(), h)
}

// --- PP9 rows -----------------------------------------------------------

// Ten party viewers of one home broadcast ride one leg (counted at home
// and at the PoP), and home's G includes them — also when the leg lands on
// an edge pod, which must report the PoP's viewers upstream.
func TestDownlinkTenPartyViewersShareOneLegAndCountInG(t *testing.T) {
	f, pop := StartHomeAndPop(t, 2, WithPopConfig(func(c *config.Config) { c.MaxSubscribers = 16 }))
	f.Route(0)
	pairLinked(t, f, pop)
	pub, id := homeBroadcast(t, f, f.Pods[0])
	sendKeyframe(t, pub, dlKeyframe(t, 0, 2000))
	WaitFor(t, "keyframe cached at the origin", func() bool {
		return f.Pods[0].Registry.Stats().Broadcasts[f.Pods[0].Registry.ObfuscateID(id)].KeyframeStreamsIn > 0
	})

	// The leg lands on pod 1, an edge for this broadcast.
	f.Route(1)
	for i := range 10 {
		v := partyViewer(t, f, pop, id)
		if got := readKeyframeFrameID(t, v); got != 0 {
			t.Fatalf("viewer %d: keyframe %d, want the primed 0", i, got)
		}
	}
	if got := f.Pods[1].Server.PopLegs(popName); got != 1 {
		t.Fatalf("legs at home pod 1 = %d, want 1 for ten party viewers", got)
	}
	if got := f.Pods[0].Server.PopLegs(popName); got != 0 {
		t.Fatalf("legs at home pod 0 = %d, want 0", got)
	}
	use := pop.Server.PopDownlinkUse()
	if use.LegsOpened != 1 || use.Streams != 1 {
		t.Fatalf("PoP downlink use = %+v, want one leg, one stream", use)
	}
	if got := pop.Client.DownlinkUse(); got.Streams != 1 {
		t.Fatalf("Client.DownlinkUse = %+v", got)
	}

	obf := f.Pods[0].Registry.ObfuscateID(id)
	WaitFor(t, "home's G counts the ten party viewers", func() bool {
		return f.Pods[0].Registry.Stats().Broadcasts[obf].ViewersGlobal == 10
	})
}

// The leg cap (spec.limits.maxDownlinkStreams, default 20) refuses the 21st
// distinct broadcast; the party viewer reads 429. A second viewer of a
// broadcast already pulled still joins.
func TestDownlinkLegCapRefusesThe21stBroadcast(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	pairLinked(t, f, pop)
	var ids []string
	for range pops.DefaultMaxDownlinkStreams + 1 {
		_, id := homeBroadcast(t, f, f.Pods[0])
		ids = append(ids, id)
	}
	for _, id := range ids[:pops.DefaultMaxDownlinkStreams] {
		partyViewer(t, f, pop, id)
	}
	if got := f.Pods[0].Server.PopLegs(popName); got != pops.DefaultMaxDownlinkStreams {
		t.Fatalf("legs = %d, want %d", got, pops.DefaultMaxDownlinkStreams)
	}
	if _, status := statusDial(t, f, "https://"+pop.Addr()+"/subscribe/"+ids[pops.DefaultMaxDownlinkStreams], nil); status != http.StatusTooManyRequests {
		t.Fatalf("the 21st broadcast: status %d, want 429", status)
	}
	_, status, reason := rawLeg(t, f, f.Pods[0], pop, ids[pops.DefaultMaxDownlinkStreams], nil)
	if status != http.StatusTooManyRequests || reason != "downlink-limit" {
		t.Fatalf("a raw 21st leg: %d %q, want 429 downlink-limit", status, reason)
	}
	partyViewer(t, f, pop, ids[0])
}

// The PoP's own downlink budget (the host's -downlink-budget lowering
// home's spec.downlink) admits pulls up to its stream count; the next
// distinct broadcast is a 429 at the PoP, and home sees no leg for it.
func TestDownlinkBudgetRefusesAtThePoP(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1, WithPopClient(func(o *popclient.Options) {
		o.Relay.DownlinkBudget = config.Budget{Streams: 1}
	}))
	pairLinked(t, f, pop)
	_, a := homeBroadcast(t, f, f.Pods[0])
	_, b := homeBroadcast(t, f, f.Pods[0])
	partyViewer(t, f, pop, a)
	if _, status := statusDial(t, f, "https://"+pop.Addr()+"/subscribe/"+b, nil); status != http.StatusTooManyRequests {
		t.Fatalf("over the PoP's budget: status %d, want 429", status)
	}
	if got := f.Pods[0].Server.PopLegs(popName); got != 1 {
		t.Fatalf("legs at home = %d, want 1", got)
	}
}

// A broadcast home does not have is the party viewer's 404, and an
// unlinked PoP never dials home for a miss.
func TestDownlinkMissesAre404(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	_, id := homeBroadcast(t, f, f.Pods[0])
	if _, status := statusDial(t, f, "https://"+pop.Addr()+"/subscribe/"+id, nil); status != http.StatusNotFound {
		t.Fatalf("unlinked PoP: status %d, want 404", status)
	}
	if got := pop.Server.PopDownlinkUse().LegsOpened; got != 0 {
		t.Fatalf("an unlinked PoP opened %d legs", got)
	}
	pairLinked(t, f, pop)
	if _, status := statusDial(t, f, "https://"+pop.Addr()+"/subscribe/ZZZZZZ", nil); status != http.StatusNotFound {
		t.Fatalf("unknown broadcast: status %d, want 404", status)
	}
	partyViewer(t, f, pop, id)
}

// /pop/subscribe's refusals are legible statuses, and a PoP never pulls
// its own (reserved) broadcast back.
func TestPopSubscribeRefusals(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	pairLinked(t, f, pop)
	_, id := homeBroadcast(t, f, f.Pods[0])
	pod := f.Pods[0]
	if _, status, _ := rawDial(t, f, pod, "/pop/subscribe/"+id, http.Header{"Authorization": {"Bearer " + popCredential(t, pop)}}); status != http.StatusUpgradeRequired {
		t.Fatalf("no proto: %d, want 426", status)
	}
	if _, status, _ := rawDial(t, f, pod, "/pop/subscribe/"+id+"?proto=1", http.Header{"Authorization": {"Bearer nope"}}); status != http.StatusUnauthorized {
		t.Fatalf("bad credential: %d, want 401", status)
	}
	if _, status, _ := rawLeg(t, f, pod, pop, "ZZZZZZ", nil); status != http.StatusNotFound {
		t.Fatalf("unknown broadcast: %d, want 404", status)
	}
	if _, status, _ := rawLeg(t, f, pod, pop, id, nil); status != http.StatusOK {
		t.Fatalf("a good leg: %d", status)
	}

	// A reservation of this PoP's own: 404, never an edge pull of itself.
	const own = "PQRS23"
	if err := pod.Coord.Reserve(context.Background(), own, popName); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	WaitFor(t, "the reservation in the informer", func() bool {
		o, _, _, ok := pod.Coord.Lookup(own)
		return ok && o.Pop == popName
	})
	if _, status, _ := rawLeg(t, f, pod, pop, own, nil); status != http.StatusNotFound {
		t.Fatalf("the PoP's own reservation: %d, want 404", status)
	}
}

// legFeed is what a raw leg received: its uni streams classified, and its
// datagrams by type.
type legFeed struct {
	mu        sync.Mutex
	keyframes []uint32
	carriers  int
	records   [][]byte
	dgrams    map[byte]int
	deltas    []uint32 // video chunks that came as datagrams
	malformed error
}

func readLeg(t *testing.T, sess *webtransport.Session) *legFeed {
	t.Helper()
	feed := &legFeed{dgrams: map[byte]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			st, err := sess.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go feed.stream(st)
		}
	}()
	go func() {
		for {
			d, err := sess.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			_, typ, err := wire.PeekType(d)
			if err != nil {
				continue
			}
			feed.mu.Lock()
			feed.dgrams[typ]++
			if typ == wire.TypeVideoChunk {
				if hdr, _, err := wire.ParseVideoChunk(d); err == nil {
					feed.deltas = append(feed.deltas, hdr.FrameID)
				}
			}
			feed.mu.Unlock()
		}
	}()
	return feed
}

func (f *legFeed) stream(st io.Reader) {
	var head [2]byte
	if _, err := io.ReadFull(st, head[:]); err != nil {
		return
	}
	if head[1] == wire.TypeReliableCarrier {
		f.mu.Lock()
		f.carriers++
		f.mu.Unlock()
		var n [wire.CarrierRecordHeaderSize]byte
		for {
			if _, err := io.ReadFull(st, n[:]); err != nil {
				return
			}
			rec := make([]byte, binary.BigEndian.Uint16(n[:]))
			if _, err := io.ReadFull(st, rec); err != nil {
				return
			}
			f.mu.Lock()
			if _, typ, err := wire.PeekType(rec); err != nil || (typ != wire.TypeVideoChunk && typ != wire.TypeParityChunk) {
				f.malformed = fmt.Errorf("a carrier record of type %#x", rec[1])
			}
			f.records = append(f.records, rec)
			f.mu.Unlock()
		}
	}
	data, _ := io.ReadAll(io.MultiReader(bytes.NewReader(head[:]), st))
	if hdr, err := wire.ParseStreamFrameHeader(data); err == nil {
		f.mu.Lock()
		f.keyframes = append(f.keyframes, hdr.FrameID)
		f.mu.Unlock()
	}
}

func (f *legFeed) snapshot() (keyframes []uint32, carriers int, records int, deltas []uint32, dgrams map[byte]int, malformed error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := map[byte]int{}
	for k, v := range f.dgrams {
		d[k] = v
	}
	return append([]uint32(nil), f.keyframes...), f.carriers, len(f.records), append([]uint32(nil), f.deltas...), d, f.malformed
}

// With ?delivery=carrier, home writes the leg with the leg writer: the
// keyframe on its own stream, the GOP's deltas on one carrier, and audio,
// a mid-GOP DecoderConfig and the count pump's ViewerCount as datagrams,
// none of which ends the carrier. Without the parameter the same broadcast
// reaches the leg as an edge's datagrams.
func TestPopSubscribeDeliveryCarrierAndDatagrams(t *testing.T) {
	f, pop := StartHomeAndPop(t, 1)
	pairLinked(t, f, pop)
	pod := f.Pods[0]
	pub, id := homeBroadcast(t, f, pod)
	sendKeyframe(t, pub, dlKeyframe(t, 0, 2000))
	WaitFor(t, "keyframe cached", func() bool {
		return pod.Registry.Stats().Broadcasts[pod.Registry.ObfuscateID(id)].KeyframeStreamsIn > 0
	})

	carrierLeg, status, _ := rawLeg(t, f, pod, pop, id, url.Values{"delivery": {"carrier"}})
	if status != http.StatusOK {
		t.Fatalf("carrier leg: %d", status)
	}
	datagramLeg, status, _ := rawLeg(t, f, pod, pop, id, nil)
	if status != http.StatusOK {
		t.Fatalf("datagram leg: %d", status)
	}
	car, dg := readLeg(t, carrierLeg), readLeg(t, datagramLeg)
	WaitFor(t, "both legs primed", func() bool {
		k1, _, _, _, _, _ := car.snapshot()
		k2, _, _, _, _, _ := dg.snapshot()
		return len(k1) == 1 && len(k2) == 1
	})

	audio, err := wire.AppendAudioFrame(nil, wire.AudioFrameHeader{Seq: 1, TimestampUs: 16_667}, []byte{0xFC, 1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: "avc1.42E02A", Extradata: []byte{0x01, 0x42, 0xE0, 0x2A}})
	if err != nil {
		t.Fatal(err)
	}
	const deltas = 20
	for i := uint32(1); i <= deltas; i++ {
		if err := pub.SendDatagram(dlDelta(t, i)); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 5:
			_ = pub.SendDatagram(audio)
		case 8:
			_ = pub.SendDatagram(cfg)
		case 10:
			pod.Registry.PumpViewerCounts(time.Now())
		}
		time.Sleep(2 * time.Millisecond)
	}
	WaitFor(t, "every delta on both legs", func() bool {
		_, _, recs, _, _, _ := car.snapshot()
		_, _, _, dd, _, _ := dg.snapshot()
		return recs == deltas && len(dd) == deltas
	})
	WaitFor(t, "the sidebands as datagrams on the carrier leg", func() bool {
		_, _, _, _, d, _ := car.snapshot()
		return d[wire.TypeAudioFrame] == 1 && d[wire.TypeDecoderConfig] >= 1 && d[wire.TypeViewerCount] >= 1
	})
	keyframes, carriers, _, carDeltas, _, malformed := car.snapshot()
	if malformed != nil {
		t.Fatal(malformed)
	}
	if len(keyframes) != 1 || carriers != 1 || len(carDeltas) != 0 {
		t.Fatalf("carrier leg: keyframes %v, %d carriers, %d deltas as datagrams; want 1 keyframe, the GOP on one carrier", keyframes, carriers, len(carDeltas))
	}
	_, dgCarriers, dgRecords, _, dgDgrams, _ := dg.snapshot()
	if dgCarriers != 0 || dgRecords != 0 {
		t.Fatalf("datagram leg: %d carriers, %d records; want none", dgCarriers, dgRecords)
	}
	if dgDgrams[wire.TypeAudioFrame] != 1 {
		t.Fatalf("datagram leg: %d audio datagrams, want 1", dgDgrams[wire.TypeAudioFrame])
	}
}
