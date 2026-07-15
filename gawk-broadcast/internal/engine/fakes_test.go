package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"
)

// Fakes shared by the engine's unit tests. They exist because the policies
// under test are defined by failures — a queue-full datagram, a stalled
// keyframe stream, a relay that accepts and never announces — and a real relay
// will not produce those on demand.

var testLog = slog.New(slog.DiscardHandler)

// fakeSession implements RelaySession.
type fakeSession struct {
	mu sync.Mutex

	datagrams [][]byte
	// sendErr, when set, fails every SendDatagram.
	sendErr error
	// sendErrAfter fails SendDatagram once this many have succeeded (-1 = never).
	sendErrAfter int
	sendCount    int
	// sendFunc, when set, decides each send — used to model a real path MTU
	// (quic-go rejects anything above it with DatagramTooLargeError).
	sendFunc func([]byte) error

	streams   []*fakeSendStream
	openErr   error
	openCalls int
	// nextStreamStalls makes the next opened stream block in Write, modelling
	// a saturated uplink.
	nextStreamStalls bool

	incoming chan ReceiveStream
	// receiveDatagrams feeds ReceiveDatagram (e.g. TimeSync pongs).
	receiveDatagrams chan []byte

	closed     bool
	closeCalls int

	ctx    context.Context
	cancel context.CancelFunc
}

func newFakeSession() *fakeSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeSession{
		sendErrAfter:     -1,
		incoming:         make(chan ReceiveStream, 4),
		receiveDatagrams: make(chan []byte, 16),
		ctx:              ctx,
		cancel:           cancel,
	}
}

func (f *fakeSession) SendDatagram(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendFunc != nil {
		if err := f.sendFunc(b); err != nil {
			return err
		}
	}
	if f.sendErr != nil {
		return f.sendErr
	}
	if f.sendErrAfter >= 0 && f.sendCount >= f.sendErrAfter {
		return errors.New("fake: datagram queue full")
	}
	f.sendCount++
	f.datagrams = append(f.datagrams, bytes.Clone(b))
	return nil
}

func (f *fakeSession) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.ctx.Done():
		return nil, errors.New("fake: session closed")
	case d := <-f.receiveDatagrams:
		return d, nil
	}
}

func (f *fakeSession) OpenUniStream() (SendStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls++
	if f.openErr != nil {
		return nil, f.openErr
	}
	str := newFakeSendStream()
	if f.nextStreamStalls {
		str.stall()
		f.nextStreamStalls = false
	}
	f.streams = append(f.streams, str)
	return str, nil
}

func (f *fakeSession) AcceptUniStream(ctx context.Context) (ReceiveStream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.ctx.Done():
		return nil, errors.New("fake: session closed")
	case s := <-f.incoming:
		return s, nil
	}
}

func (f *fakeSession) CloseWithError(code webtransport.SessionErrorCode, msg string) error {
	f.mu.Lock()
	f.closed = true
	f.closeCalls++
	f.mu.Unlock()
	f.cancel()
	return nil
}

func (f *fakeSession) Context() context.Context { return f.ctx }

func (f *fakeSession) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeSession) sentDatagrams() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.datagrams...)
}

func (f *fakeSession) sendStreams() []*fakeSendStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeSendStream(nil), f.streams...)
}

// fakeSendStream implements SendStream.
type fakeSendStream struct {
	mu sync.Mutex

	buf        bytes.Buffer
	writeErr   error
	closed     bool
	cancelled  bool
	cancelCode webtransport.StreamErrorCode
	// block, when non-nil, holds Write until it is closed — a stalled uplink.
	block chan struct{}
}

func newFakeSendStream() *fakeSendStream { return &fakeSendStream{} }

func (s *fakeSendStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	block := s.block
	s.mu.Unlock()
	if block != nil {
		<-block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return 0, errors.New("fake: stream cancelled")
	}
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.buf.Write(p)
}

func (s *fakeSendStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return errors.New("fake: stream cancelled")
	}
	s.closed = true
	return nil
}

func (s *fakeSendStream) CancelWrite(code webtransport.StreamErrorCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = true
	s.cancelCode = code
	if s.block != nil {
		close(s.block)
		s.block = nil
	}
}

func (s *fakeSendStream) stall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.block = make(chan struct{})
}

func (s *fakeSendStream) bytesWritten() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.buf.Bytes())
}

func (s *fakeSendStream) wasCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

// fakeReceiveStream implements ReceiveStream.
//
// It models quic-go's deadline semantics rather than a simpler "block
// forever", because those semantics are exactly what the engine relies on to
// unblock a pending announce read at teardown: a deadline in the past aborts
// an in-flight Read. A fake that ignored deadlines would let a Stop()-hangs
// bug pass.
type fakeReceiveStream struct {
	mu       sync.Mutex
	r        io.Reader
	deadline time.Time
	expired  chan struct{}
	// never, when true, produces no data — a relay that accepts the stream
	// and then says nothing. The read still ends when the deadline passes.
	never bool
}

func (s *fakeReceiveStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.expired == nil {
		s.expired = make(chan struct{})
	}
	expired, never := s.expired, s.never
	s.mu.Unlock()

	if never {
		<-expired
		return 0, errors.New("fake: read deadline exceeded")
	}
	return s.r.Read(p)
}

func (s *fakeReceiveStream) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = t
	if s.expired == nil {
		s.expired = make(chan struct{})
	}
	if !t.IsZero() && t.Before(time.Now()) {
		select {
		case <-s.expired: // already expired
		default:
			close(s.expired)
		}
	}
	return nil
}

func (s *fakeReceiveStream) readDeadline() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deadline
}

// fakeMedia implements MediaSource.
type fakeMedia struct {
	frames    chan AccessUnit
	startErr  error
	err       error
	encoder   string
	stopped   bool
	stopCalls int
	mu        sync.Mutex
	// clock records what the engine handed the factory (Decision 6).
	clock Clock
}

func newFakeMedia() *fakeMedia {
	return &fakeMedia{frames: make(chan AccessUnit, 16), encoder: "fakeenc"}
}

func (m *fakeMedia) factory() MediaSourceFactory {
	return func(cfg MediaConfig, clock Clock, log *slog.Logger) (MediaSource, error) {
		m.mu.Lock()
		m.clock = clock
		m.mu.Unlock()
		return m, nil
	}
}

func (m *fakeMedia) Start(ctx context.Context) (<-chan AccessUnit, error) {
	if m.startErr != nil {
		return nil, m.startErr
	}
	return m.frames, nil
}

func (m *fakeMedia) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	m.stopCalls++
	return nil
}

func (m *fakeMedia) Encoder() string { return m.encoder }

func (m *fakeMedia) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *fakeMedia) wasStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped
}

func (m *fakeMedia) gotClock() Clock {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clock
}

// announceStream builds a ReceiveStream carrying a BroadcastAnnounce.
func announceStream(msg []byte) ReceiveStream {
	return &fakeReceiveStream{r: bytes.NewReader(msg)}
}
