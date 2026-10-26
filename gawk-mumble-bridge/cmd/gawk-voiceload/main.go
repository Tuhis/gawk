// Command gawk-voiceload is the one-pod load measurement of docs/80 MV7: it
// puts N voice participants into one room the way N browsers would — a room
// control session each (RoomHello with RoomCapVoice, VoiceJoin), a ticket
// each, a voice session each on the bridge — and lets K of them talk at
// once, 10 ms Opus frames from the committed fixture. Every participant
// counts the VoiceFrames it receives. When -duration ends it prints one JSON
// summary: frames sent, frames received, the receive ratio against what
// murmur should have delivered, and the join latency.
//
// It measures delivery, not quality: the payload is a real Opus fixture so
// murmur and the bridge handle real packet sizes, but nothing decodes it.
// CPU is the harness's to measure (the bridge's /proc and `docker stats` for
// murmur), because only the harness knows which processes are the pod.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/opusfixture"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

type summary struct {
	Users          int     `json:"users"`
	Talkers        int     `json:"talkers"`
	Joined         int     `json:"joined"`
	DurationS      float64 `json:"durationS"`
	Sent           int64   `json:"sent"`
	Received       int64   `json:"received"`
	Expected       int64   `json:"expected"`
	ReceiveRatio   float64 `json:"receiveRatio"`
	JoinMedianMs   float64 `json:"joinMedianMs"`
	JoinP95Ms      float64 `json:"joinP95Ms"`
	JoinFailures   int     `json:"joinFailures"`
	FirstJoinError string  `json:"firstJoinError,omitempty"`
}

func main() {
	relay := flag.String("relay", "https://127.0.0.1:4433", "relay URL")
	room := flag.String("room", "", "room code")
	origin := flag.String("origin", "http://127.0.0.1:8080", "Origin header (the bridge's -allowed-origins)")
	caFile := flag.String("ca-file", "", "PEM roots for the relay and the bridge")
	users := flag.Int("users", 20, "voice participants")
	talkers := flag.Int("talkers", 3, "participants talking at once")
	fixture := flag.String("fixture", "testdata/tone-440hz-10ms.opus", "length-prefixed 10 ms Opus packets")
	duration := flag.Duration("duration", 30*time.Second, "how long the talkers talk")
	flag.Parse()
	if *room == "" {
		fmt.Fprintln(os.Stderr, "gawk-voiceload: -room is required")
		os.Exit(2)
	}
	f, err := os.Open(*fixture)
	if err != nil {
		fatal(err)
	}
	packets, err := opusfixture.Read(f)
	f.Close()
	if err != nil {
		fatal(err)
	}
	tlsConf := &tls.Config{}
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		if err != nil {
			fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tlsConf.RootCAs = pool
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		received atomic.Int64
		sent     atomic.Int64
		mu       sync.Mutex
		joins    []time.Duration
		failures int
		firstErr string
		parts    []*participant
	)
	var wg sync.WaitGroup
	for i := 0; i < *users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			p, err := join(ctx, tlsConf, *relay, *room, *origin, fmt.Sprintf("load%03d", i), &received)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures++
				if firstErr == "" {
					firstErr = err.Error()
				}
				return
			}
			joins = append(joins, time.Since(start))
			parts = append(parts, p)
		}(i)
		time.Sleep(50 * time.Millisecond) // a join wave, not a thundering herd
	}
	wg.Wait()
	// Let every bridged user settle into the channel before anyone talks.
	time.Sleep(3 * time.Second)
	received.Store(0)

	talking := parts
	if len(talking) > *talkers {
		talking = talking[:*talkers]
	}
	tctx, tcancel := context.WithTimeout(ctx, *duration)
	began := time.Now()
	var tw sync.WaitGroup
	for _, p := range talking {
		tw.Add(1)
		go func(p *participant) {
			defer tw.Done()
			p.talk(tctx, packets, &sent)
		}(p)
	}
	tw.Wait()
	tcancel()
	time.Sleep(time.Second) // the last frames in flight
	elapsed := time.Since(began)

	// Murmur sends each talker's frames to everyone else in the channel.
	n := int64(len(parts))
	var expected int64
	if n > 0 {
		expected = sent.Load() * (n - 1)
	}
	sort.Slice(joins, func(a, b int) bool { return joins[a] < joins[b] })
	s := summary{
		Users: *users, Talkers: len(talking), Joined: len(parts), DurationS: elapsed.Seconds(),
		Sent: sent.Load(), Received: received.Load(), Expected: expected,
		JoinFailures: failures, FirstJoinError: firstErr,
	}
	if expected > 0 {
		s.ReceiveRatio = float64(s.Received) / float64(expected)
	}
	if len(joins) > 0 {
		s.JoinMedianMs = float64(joins[len(joins)/2].Milliseconds())
		s.JoinP95Ms = float64(joins[len(joins)*95/100].Milliseconds())
	}
	for _, p := range parts {
		p.close()
	}
	out, _ := json.Marshal(s)
	fmt.Println(string(out))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gawk-voiceload:", err)
	os.Exit(1)
}

type participant struct {
	room, voice *webtransport.Session
	dialers     []*webtransport.Transport
}

func (p *participant) close() {
	_ = p.voice.CloseWithError(0, "")
	_ = p.room.CloseWithError(0, "")
	for _, d := range p.dialers {
		_ = d.Close()
	}
}

func dialer(tlsConf *tls.Config) *webtransport.Transport {
	return &webtransport.Transport{
		TLSClientConfig: tlsConf.Clone(),
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, KeepAlivePeriod: 5 * time.Second},
	}
}

func writeRecord(w io.Writer, msg []byte) error {
	rec, err := wire.AppendRoomRecord(nil, msg)
	if err != nil {
		return err
	}
	_, err = w.Write(rec)
	return err
}

func readRecord(r io.Reader) ([]byte, error) {
	var hdr [wire.RoomRecordHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n, err := wire.ParseRoomRecordLength(hdr[:])
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	_, err = io.ReadFull(r, buf)
	return buf, err
}

// join runs one participant's whole join: room session, VoiceJoin, ticket,
// voice session, hello, and the first VoiceState that says connected.
func join(ctx context.Context, tlsConf *tls.Config, relay, room, origin, nick string, received *atomic.Int64) (*participant, error) {
	hdr := http.Header{"Origin": []string{origin}}
	jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rd := dialer(tlsConf)
	_, rs, err := rd.Dial(jctx, relay+"/room/"+url.PathEscape(room)+"?name="+url.QueryEscape(nick), hdr)
	if err != nil {
		_ = rd.Close()
		return nil, fmt.Errorf("room dial: %w", err)
	}
	p := &participant{room: rs, dialers: []*webtransport.Transport{rd}}
	fail := func(err error) (*participant, error) {
		_ = rs.CloseWithError(0, "")
		for _, d := range p.dialers {
			_ = d.Close()
		}
		return nil, err
	}
	ctl, err := rs.OpenStreamSync(jctx)
	if err != nil {
		return fail(err)
	}
	hello, _ := wire.AppendRoomHello(nil, wire.RoomHello{Protocol: wire.RoomProtocolVersion, ClientKind: wire.RoomClientWebViewer, WantCaps: wire.RoomCapVoice, Nickname: nick})
	if err := writeRecord(ctl, hello); err != nil {
		return fail(err)
	}
	cmd, _ := wire.AppendRoomCommand(nil, wire.RoomCommand{Kind: wire.RoomCommandVoiceJoin})
	if err := writeRecord(ctl, cmd); err != nil {
		return fail(err)
	}
	var ticketURL string
	var ticket []byte
	for ticket == nil {
		_ = ctl.SetReadDeadline(time.Now().Add(15 * time.Second))
		rec, err := readRecord(ctl)
		if err != nil {
			return fail(fmt.Errorf("waiting for a ticket: %w", err))
		}
		if rec[1] != wire.TypeRoomEvent {
			continue
		}
		ev, err := wire.ParseRoomEvent(rec)
		if err != nil {
			continue
		}
		switch ev.Kind {
		case wire.RoomEventVoiceTicket:
			ticketURL, ticket = ev.VoiceURL, append([]byte(nil), ev.VoiceTicket...)
		case wire.RoomEventCommandRejected:
			return fail(fmt.Errorf("VoiceJoin rejected: reason %d %s", ev.Reason, ev.Message))
		}
	}
	// Keep draining the room stream so the relay never sees it stall.
	go func() { _, _ = io.Copy(io.Discard, ctl) }()

	vd := dialer(tlsConf)
	p.dialers = append(p.dialers, vd)
	_, vs, err := vd.Dial(jctx, ticketURL, hdr)
	if err != nil {
		return fail(fmt.Errorf("voice dial: %w", err))
	}
	p.voice = vs
	vctl, err := vs.OpenStreamSync(jctx)
	if err != nil {
		return fail(err)
	}
	vh, err := wire.AppendVoiceHello(nil, wire.VoiceHello{Protocol: wire.VoiceProtocolVersion, Ticket: ticket})
	if err != nil {
		return fail(err)
	}
	if err := writeRecord(vctl, vh); err != nil {
		return fail(err)
	}
	for {
		_ = vctl.SetReadDeadline(time.Now().Add(20 * time.Second))
		rec, err := readRecord(vctl)
		if err != nil {
			return fail(fmt.Errorf("waiting for the Mumble connection: %w", err))
		}
		if rec[1] != wire.TypeVoiceState {
			continue
		}
		st, err := wire.ParseVoiceState(rec)
		if err == nil && st.Flags&wire.VoiceStateFlagConnected != 0 {
			break
		}
	}
	go func() { _, _ = io.Copy(io.Discard, vctl) }()
	go func() {
		for {
			d, err := vs.ReceiveDatagram(context.Background())
			if err != nil {
				return
			}
			if len(d) >= 2 && d[1] == wire.TypeVoiceFrame {
				received.Add(1)
			}
		}
	}()
	return p, nil
}

// talk sends the fixture on a 10 ms clock, looping, in 2 s spurts with the
// end flag on each spurt's last frame.
func (p *participant) talk(ctx context.Context, packets [][]byte, sent *atomic.Int64) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var seq uint32
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		end := (i+1)%200 == 0
		d, err := wire.AppendVoiceFrame(nil, wire.VoiceFrameHeader{End: end, Seq: seq}, packets[i%len(packets)])
		if err != nil {
			return
		}
		if err := p.voice.SendDatagram(d); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			continue
		}
		sent.Add(1)
		seq++
		i++
	}
}
