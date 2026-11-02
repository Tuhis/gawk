package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/publisher"
	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
)

// relayClient dials the relay's WebTransport routes the way a native
// publisher does: the same Origin, the same TLS posture.
type relayClient struct {
	url      string
	origin   string
	insecure bool
}

// dial opens one WebTransport session. Status is the HTTP status of a
// refused CONNECT, 0 when the failure never reached HTTP.
func (c relayClient) dial(ctx context.Context, rawURL string) (*webtransport.Session, func(), int, error) {
	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.insecure}, //nolint:gosec // -insecure, dev certificates only
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			KeepAlivePeriod:                  10 * time.Second,
		},
	}
	hdr := http.Header{}
	if c.origin != "" {
		hdr.Set("Origin", c.origin)
	}
	rsp, sess, err := d.Dial(ctx, rawURL, hdr)
	if err != nil {
		_ = d.Close()
		status := 0
		if rsp != nil {
			status = rsp.StatusCode
		}
		return nil, nil, status, err
	}
	return sess, func() { _ = sess.CloseWithError(0, ""); _ = d.Close() }, 0, nil
}

// mintURL is CONNECT /publish?mint=1, with the publish secret if there is one.
func mintURL(relayURL, secret string) (string, error) {
	u, err := publisher.PublishURL(relayURL, "", secret, "")
	if err != nil {
		return "", err
	}
	p, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	q := p.Query()
	q.Set("mint", "1")
	p.RawQuery = q.Encode()
	return p.String(), nil
}

// mintResult gathers what a mint-only session announces.
type mintResult struct {
	mu        sync.Mutex
	id, token string
	closing   uint32 // a SessionClosing's code, 0 if none arrived
	done      chan struct{}
	once      sync.Once
}

func newMintResult() *mintResult { return &mintResult{done: make(chan struct{})} }

// add dispatches one server message by its wire type, never by arrival
// order: webtransport-go accepts the relay's uni streams in any order, so the
// 4017 SessionClosing can come before the announce or the token
// (docs/gotchas.md).
func (m *mintResult) add(msg []byte) error {
	if len(msg) < 2 {
		return fmt.Errorf("server message of %d bytes", len(msg))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch msg[1] {
	case wire.TypeBroadcastAnnounce:
		id, err := wire.ParseBroadcastAnnounce(msg)
		if err != nil {
			return err
		}
		m.id = id
	case wire.TypeResumeToken:
		raw, err := wire.ParseResumeToken(msg)
		if err != nil {
			return err
		}
		m.token = hex.EncodeToString(raw)
	case wire.TypeSessionClosing:
		code, err := wire.ParseSessionClosing(msg)
		if err != nil {
			return err
		}
		m.closing = code
	default:
		return nil // a hello, capabilities: not ours to read
	}
	if m.id != "" && m.token != "" {
		m.once.Do(func() { close(m.done) })
	}
	return nil
}

func (m *mintResult) key() (streamkey.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return streamkey.Parse(streamkey.Prefix + m.id + "_" + m.token)
}

// mint asks the relay for a fresh stream key (docs/83 D3): the announce and
// the resume token arrive on uni streams, then the relay closes with 4017.
func (c relayClient) mint(ctx context.Context, secret string) (streamkey.Key, int, error) {
	raw, err := mintURL(c.url, secret)
	if err != nil {
		return streamkey.Key{}, 0, err
	}
	sess, closeSess, status, err := c.dial(ctx, raw)
	if err != nil {
		return streamkey.Key{}, status, err
	}
	defer closeSess()
	res := newMintResult()
	go func() {
		for {
			str, err := sess.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go func() {
				_ = str.SetReadDeadline(time.Now().Add(5 * time.Second))
				buf, err := io.ReadAll(io.LimitReader(str, 4096))
				if err == nil {
					_ = res.add(buf)
				}
			}()
		}
	}()
	select {
	case <-res.done:
		k, err := res.key()
		return k, 0, err
	case <-sess.Context().Done():
		// Every message was on the wire before the close (the relay's
		// closeWithNotice), so this is a mint that failed.
		select {
		case <-res.done:
			k, err := res.key()
			return k, 0, err
		case <-time.After(500 * time.Millisecond):
		}
		res.mu.Lock()
		code := res.closing
		res.mu.Unlock()
		return streamkey.Key{}, 0, fmt.Errorf("the mint session closed (code %d) before the ID and the token arrived", code)
	case <-ctx.Done():
		return streamkey.Key{}, 0, ctx.Err()
	}
}

// mintWithRetry mints, retrying while the relay is not up yet. A refusal
// that retrying cannot fix (a wrong secret, a ban) ends it.
func mintWithRetry(ctx context.Context, c relayClient, secret string, log *slog.Logger) (streamkey.Key, error) {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		k, status, err := c.mint(ctx, secret)
		if err == nil {
			log.Info("stream key minted", "broadcast", k.ID)
			return k, nil
		}
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusUnavailableForLegalReasons:
			return streamkey.Key{}, fmt.Errorf("the relay refused the mint (%d): %w", status, err)
		}
		log.Info("mint failed; retrying", "attempt", attempt, "status", status, "err", err)
		if !sleep(ctx, delay) {
			return streamkey.Key{}, ctx.Err()
		}
		delay = min(2*delay, 10*time.Second)
	}
}

// roomRejectError is a CommandRejected answer to the Attach.
type roomRejectError struct {
	reason  uint8
	message string
}

func (e *roomRejectError) Error() string {
	return fmt.Sprintf("the relay refused the attach (reason %d): %s", e.reason, e.message)
}

// attach puts the broadcast in a room with its ID and resume token as the
// proof (docs/44 D8, D9): a room control session that is not the
// publisher's, which the relay allows by design. The attachment outlives
// this session (a reload must not detach a broadcast, docs/44 §4.4), so the
// session closes once the relay confirms it.
func (c relayClient) attach(ctx context.Context, room, attachSecret, label string, key streamkey.Key) (int, error) {
	raw, err := publisher.RoomURL(c.url, room, attachSecret, "")
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sess, closeSess, status, err := c.dial(ctx, raw)
	if err != nil {
		return status, err
	}
	defer closeSess()
	str, err := sess.OpenStreamSync(ctx)
	if err != nil {
		return 0, err
	}
	stopWatch := context.AfterFunc(ctx, func() { _ = str.SetReadDeadline(time.Now().Add(-time.Second)) })
	defer stopWatch()
	hello, err := wire.AppendRoomHello(nil, wire.RoomHello{
		Protocol:   wire.RoomProtocolVersion,
		ClientKind: wire.RoomClientNative,
		Nickname:   "gawk-whipsim",
	})
	if err != nil {
		return 0, err
	}
	if err := writeRecord(str, hello); err != nil {
		return 0, err
	}
	token, err := hex.DecodeString(key.Token)
	if err != nil {
		return 0, err
	}
	sent := false
	for {
		rec, err := readRecord(str)
		if err != nil {
			return 0, fmt.Errorf("room session: %w", err)
		}
		switch rec[1] {
		case wire.TypeRoomState:
			if sent {
				continue
			}
			cmd, err := wire.AppendRoomCommand(nil, wire.RoomCommand{
				Kind:        wire.RoomCommandAttach,
				BroadcastID: key.ID,
				ResumeToken: token,
				Label:       label,
			})
			if err != nil {
				return 0, err
			}
			if err := writeRecord(str, cmd); err != nil {
				return 0, err
			}
			sent = true
		case wire.TypeRoomEvent:
			ev, err := wire.ParseRoomEvent(rec)
			if err != nil {
				continue // a kind this tool doesn't know
			}
			switch ev.Kind {
			case wire.RoomEventAttachmentAdded, wire.RoomEventAttachmentUpdated:
				if ev.Attachment.BroadcastID == key.ID {
					return 0, nil
				}
			case wire.RoomEventCommandRejected:
				if ev.Command == wire.RoomCommandAttach {
					return 0, &roomRejectError{reason: ev.Reason, message: ev.Message}
				}
			}
		}
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
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	if len(buf) < 2 {
		return nil, fmt.Errorf("room record of %d bytes", len(buf))
	}
	return buf, nil
}

// attachWithRetry retries an attach the relay could not take yet: the
// broadcast it names exists only once the gateway's reclaim has reached the
// relay, a moment after the gateway answers the POST.
func attachWithRetry(ctx context.Context, c relayClient, room, attachSecret, label string, key streamkey.Key, log *slog.Logger) error {
	var last error
	for attempt := 1; attempt <= 10; attempt++ {
		status, err := c.attach(ctx, room, attachSecret, label, key)
		if err == nil {
			log.Info("broadcast attached to the room", "room", room, "broadcast", key.ID)
			return nil
		}
		last = err
		var rej *roomRejectError
		if status == http.StatusForbidden || status == http.StatusNotFound ||
			(errors.As(err, &rej) && rej.reason != wire.RoomRejectNotFound) {
			return err
		}
		log.Info("attach failed; retrying", "attempt", attempt, "status", status, "err", err)
		if !sleep(ctx, time.Second) {
			return ctx.Err()
		}
	}
	return last
}

// received is what a viewer session saw.
type received struct {
	keyframes int
	frames    int
}

// watch subscribes to the broadcast as a viewer does and reports once it
// has a keyframe and want frames, the round trip OBS → gateway → relay →
// viewer.
func (c relayClient) watch(ctx context.Context, id string, want int) (received, error) {
	base, err := url.Parse(c.url)
	if err != nil {
		return received{}, err
	}
	u, err := base.Parse("/subscribe/" + id)
	if err != nil {
		return received{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sess, closeSess, status, err := c.dial(ctx, u.String())
	if err != nil {
		return received{}, fmt.Errorf("subscribe (status %d): %w", status, err)
	}
	defer closeSess()
	var mu sync.Mutex
	var got received
	enough := make(chan struct{})
	var once sync.Once
	check := func() {
		if got.keyframes > 0 && got.frames >= want {
			once.Do(func() { close(enough) })
		}
	}
	go func() {
		for {
			str, err := sess.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go func() {
				// A keyframe rides a reliable stream: count it once read
				// to the end, as a viewer only decodes a whole one.
				if _, err := io.Copy(io.Discard, str); err != nil {
					return
				}
				mu.Lock()
				got.keyframes++
				check()
				mu.Unlock()
			}()
		}
	}()
	go func() {
		for {
			d, err := sess.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if h, _, err := wire.ParseVideoChunk(d); err == nil && h.ChunkIndex == 0 {
				mu.Lock()
				got.frames++
				check()
				mu.Unlock()
			}
		}
	}()
	select {
	case <-enough:
		mu.Lock()
		defer mu.Unlock()
		return got, nil
	case <-ctx.Done():
		mu.Lock()
		defer mu.Unlock()
		return got, fmt.Errorf("a viewer saw %d keyframes and %d frames, want ≥ 1 and %d: %w", got.keyframes, got.frames, want, ctx.Err())
	}
}

func watchWithRetry(ctx context.Context, c relayClient, id string, want int, log *slog.Logger) (received, error) {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		got, err := c.watch(ctx, id, want)
		if err == nil {
			return got, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
		log.Info("watch failed; retrying", "attempt", attempt, "err", err)
	}
	return received{}, last
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
