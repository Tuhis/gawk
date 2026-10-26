package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/voice"
)

// The observer links (R79, docs/80 D4, §11.3). For every voice-live room
// this pod is HOME for, one link to the bridge's internal listener: CONNECT
// voice.ObservePath?psk=, one bidirectional stream, an Observe naming the
// room and its channel, then the bridge's Roster reports on every change.
// The bridge pod that accepts the link runs the room's observer Mumble
// connection; holding the link IS the ownership (OD27), so a drop is
// redialled with a backoff and lands wherever the bridge's Service sends it.
//
// The set of links follows the registry (Registry.VoiceRooms): a reconcile
// on every change it reports (OnVoiceChanged → VoiceChanged) and on a
// ticker, which also runs the cluster keeper — the D12 rule that a static
// room with a Mumble mapping is homed "as if the first join had happened",
// and re-homed by some pod after its home drains.
//
// None of this exists without StartVoice, which serve.Run calls only when
// voice is configured (config.Config.VoiceConfigured): the off-path dials
// nothing.

// Observer link timings. The backoff doubles from voiceBackoffBase to
// voiceBackoffMax with ±25% jitter, so a bridge restart is not met by every
// relay pod redialling every room in the same instant; a link that got as
// far as a Roster resets it.
const (
	voiceBackoffBase  = time.Second
	voiceBackoffMax   = 30 * time.Second
	voiceKeeperEvery  = 10 * time.Second
	voiceDialTimeout  = 10 * time.Second
	voiceAdoptTimeout = 10 * time.Second
)

// VoiceBridgeOptions is what the observer dial needs (docs/80 §11.4): the
// bridge's internal address, the TLS name it presents and the roots that
// verify it (nil = the system's). The PSK is read from the config, as the
// internal PSK is.
type VoiceBridgeOptions struct {
	Addr       string
	ServerName string
	RootCAs    *x509.CertPool
}

// voiceObserverLink is one connected observer link.
type voiceObserverLink interface {
	Send(voice.Message) error
	Recv() (voice.Message, error)
	Close()
}

// voiceObserverDialer opens one observer link; injectable for tests.
type voiceObserverDialer func(ctx context.Context) (voiceObserverLink, error)

// voiceKeeperStore is the slice of the room store the keeper needs: the
// static voice rooms no pod homes. *roomcluster.Store implements it; a
// RoomCluster that does not (a test fake) gets no keeper.
type voiceKeeperStore interface {
	UnhomedVoiceRooms() []string
	AdoptVoice(ctx context.Context, code string) error
}

type voiceManager struct {
	s    *Server
	dial voiceObserverDialer
	// kick asks for a reconcile, keep for a keeper pass; both buffered,
	// never closed.
	kick chan struct{}
	keep chan struct{}

	backoffBase, backoffMax, keeperEvery time.Duration

	mu   sync.Mutex
	runs map[string]*observerRun
}

type observerRun struct {
	spec   roomsrv.VoiceRoom
	cancel context.CancelFunc
	done   chan struct{}
}

// voiceManagerBox lets StartVoice publish the manager atomically: the
// registry's change hook may fire from any goroutine.
type voiceManagerBox struct{ m *voiceManager }

// StartVoice turns on the observer links and the cluster keeper until ctx
// ends. Call once, with voice configured; the registry may be installed
// before or after (every pass reads it late).
func (s *Server) StartVoice(ctx context.Context, o VoiceBridgeOptions) {
	m := &voiceManager{
		s: s, dial: newVoiceObserverDialer(o.Addr, o.ServerName, s.cfg.VoiceBridgePSK, o.RootCAs),
		kick: make(chan struct{}, 1), keep: make(chan struct{}, 1),
		backoffBase: voiceBackoffBase, backoffMax: voiceBackoffMax, keeperEvery: voiceKeeperEvery,
		runs: make(map[string]*observerRun),
	}
	if hook := s.testHookVoiceManager.Load(); hook != nil {
		(*hook)(m)
	}
	s.voice.Store(&voiceManagerBox{m: m})
	go m.loop(ctx)
}

// VoiceChanged is the registry's OnVoiceChanged: reconcile soon. Never
// blocks; a no-op before StartVoice.
func (s *Server) VoiceChanged() {
	if b := s.voice.Load(); b != nil {
		signal(b.m.kick)
	}
}

// StaticVoiceRoomSeen is the room store's OnStaticVoiceRoom: a static CR
// with a Mumble mapping appeared or changed, so run the keeper now rather
// than at its next tick (D12 "when its CR appears").
func (s *Server) StaticVoiceRoomSeen(string) {
	if b := s.voice.Load(); b != nil {
		signal(b.m.keep)
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (m *voiceManager) loop(ctx context.Context) {
	t := time.NewTicker(m.keeperEvery)
	defer t.Stop()
	m.keepRooms(ctx)
	m.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return
		case <-m.kick:
			m.reconcile(ctx)
		case <-m.keep:
			m.keepRooms(ctx)
			m.reconcile(ctx)
		case <-t.C:
			m.keepRooms(ctx)
			m.reconcile(ctx)
		}
	}
}

// keepRooms is the cluster keeper (D12): adopt every static voice room with
// no live home, by the same lease claim a join makes. Losing the race to
// another pod is the normal outcome on all but one pod and is not logged.
func (m *voiceManager) keepRooms(ctx context.Context) {
	rc := m.s.roomClusterWiring()
	if rc == nil {
		return
	}
	ks, ok := rc.store.(voiceKeeperStore)
	if !ok {
		return
	}
	for _, code := range ks.UnhomedVoiceRooms() {
		if ctx.Err() != nil {
			return
		}
		actx, cancel := context.WithTimeout(ctx, voiceAdoptTimeout)
		err := ks.AdoptVoice(actx, code)
		cancel()
		switch {
		case err == nil:
			m.s.log.Info("voice room homed", "room_key", m.s.broadcastKey(code))
		case errors.Is(err, roomcluster.ErrHeldElsewhere), errors.Is(err, roomcluster.ErrNotFound):
		default:
			m.s.log.Warn("voice room homing failed", "room_key", m.s.broadcastKey(code), "err", err)
		}
	}
}

// reconcile makes the running links match the registry's voice-live rooms:
// a room gone or changed (remapped, re-created) stops its link, a new one
// starts one.
func (m *voiceManager) reconcile(ctx context.Context) {
	var want []roomsrv.VoiceRoom
	if reg := m.s.roomRegistry(); reg != nil {
		want = reg.VoiceRooms()
	}
	byCode := make(map[string]roomsrv.VoiceRoom, len(want))
	for _, v := range want {
		byCode[v.Code] = v
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for code, run := range m.runs {
		if v, ok := byCode[code]; !ok || v != run.spec {
			run.cancel()
			delete(m.runs, code)
		}
	}
	if ctx.Err() != nil {
		return
	}
	for code, v := range byCode {
		if _, running := m.runs[code]; running {
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		run := &observerRun{spec: v, cancel: cancel, done: make(chan struct{})}
		m.runs[code] = run
		go m.observe(rctx, run)
	}
}

func (m *voiceManager) stopAll() {
	m.mu.Lock()
	runs := m.runs
	m.runs = make(map[string]*observerRun)
	m.mu.Unlock()
	for _, run := range runs {
		run.cancel()
		<-run.done
	}
}

// running reports the observed rooms (tests).
func (m *voiceManager) running() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.runs))
	for code := range m.runs {
		out = append(out, code)
	}
	return out
}

// observe holds one room's observer link until ctx ends, redialling with
// the backoff after every failure or drop and marking the roster
// unavailable in between.
func (m *voiceManager) observe(ctx context.Context, run *observerRun) {
	defer close(run.done)
	spec := run.spec
	log := m.s.log.With("room_key", m.s.broadcastKey(spec.Code))
	down := func(reason string) {
		if reg := m.s.roomRegistry(); reg != nil {
			reg.VoiceObserverDown(spec.Code, spec.ID, reason)
		}
	}
	backoff := m.backoffBase
	for {
		got, err := m.observeOnce(ctx, spec)
		if ctx.Err() != nil {
			return
		}
		down("observer link lost")
		if got {
			backoff = m.backoffBase
			log.Info("voice observer link dropped, redialling", "err", err)
		} else {
			log.Warn("voice observer link failed", "err", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		if backoff *= 2; backoff > m.backoffMax {
			backoff = m.backoffMax
		}
	}
}

// observeOnce dials, sends Observe and applies Roster reports until the
// link ends; got says whether any report arrived.
func (m *voiceManager) observeOnce(ctx context.Context, spec roomsrv.VoiceRoom) (got bool, err error) {
	reg := m.s.roomRegistry()
	if reg == nil {
		return false, errors.New("no room registry")
	}
	dctx, cancel := context.WithTimeout(ctx, voiceDialTimeout)
	link, err := m.dial(dctx)
	cancel()
	if err != nil {
		return false, err
	}
	defer link.Close()
	stop := context.AfterFunc(ctx, link.Close)
	defer stop()
	epoch, _ := reg.VoiceEpoch(spec.Code)
	obs := voice.Message{Type: voice.TypeObserve, Code: spec.Code, DisplayCode: spec.DisplayCode,
		Channel: spec.Channel, Temporary: spec.Temporary, Epoch: epoch}
	if err := link.Send(obs); err != nil {
		return false, fmt.Errorf("send observe: %w", err)
	}
	for {
		msg, err := link.Recv()
		if err != nil {
			return got, err
		}
		if msg.Type != voice.TypeRoster {
			continue
		}
		got = true
		reg.ApplyVoiceRoster(spec.Code, spec.ID, msg)
	}
}

// jitter spreads d by ±25%.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d*3/4 + time.Duration(rand.Int64N(int64(d)/2+1))
}

// wtVoiceLink is a voice link over one WebTransport bidirectional stream.
type wtVoiceLink struct {
	d      *webtransport.Transport
	sess   *webtransport.Session
	stream *webtransport.Stream
	wmu    sync.Mutex
	closed atomic.Bool
}

func (l *wtVoiceLink) Send(m voice.Message) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	_ = l.stream.SetWriteDeadline(time.Now().Add(roomWriteTimeout))
	return voice.WriteMessage(l.stream, m)
}

func (l *wtVoiceLink) Recv() (voice.Message, error) { return voice.ReadMessage(l.stream) }

func (l *wtVoiceLink) Close() {
	if l.closed.CompareAndSwap(false, true) {
		_ = l.sess.CloseWithError(0, "")
		_ = l.d.Close()
	}
}

// newVoiceObserverDialer is the production dial: newRoomProxyDialer's
// transport settings, the bridge's TLS name and roots, the voice PSK and
// the link Origin.
func newVoiceObserverDialer(addr, serverName, psk string, roots *x509.CertPool) voiceObserverDialer {
	return func(ctx context.Context) (voiceObserverLink, error) {
		d := &webtransport.Transport{
			TLSClientConfig: &tls.Config{ServerName: serverName, RootCAs: roots},
			QUICConfig: &quic.Config{
				EnableDatagrams:                  true,
				EnableStreamResetPartialDelivery: true,
				MaxIdleTimeout:                   edgeIdleTimeout,
				KeepAlivePeriod:                  edgeKeepAlive,
			},
		}
		target := "https://" + addr + voice.ObservePath + "?psk=" + url.QueryEscape(psk)
		rsp, sess, err := d.Dial(ctx, target, http.Header{"Origin": []string{voice.LinkOrigin}})
		if err != nil {
			_ = d.Close()
			if rsp != nil {
				return nil, fmt.Errorf("observer dial: status %d: %w", rsp.StatusCode, err)
			}
			return nil, fmt.Errorf("observer dial: %w", err)
		}
		stream, err := sess.OpenStreamSync(ctx)
		if err != nil {
			_ = sess.CloseWithError(0, "")
			_ = d.Close()
			return nil, fmt.Errorf("observer stream: %w", err)
		}
		return &wtVoiceLink{d: d, sess: sess, stream: stream}, nil
	}
}
