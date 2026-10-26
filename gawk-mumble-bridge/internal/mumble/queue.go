package mumble

import (
	"context"
	"sync"
	"sync/atomic"
)

// AudioQueue is one listener's bounded audio queue (D6). The connection's
// read loop pushes into it without ever blocking: when the queue is full the
// OLDEST packet is dropped and counted, because a late voice frame is worth
// less than the next one (favor dropped frames over stalled playback). The
// consumer pops at its own pace.
type AudioQueue struct {
	mu     sync.Mutex
	buf    []AudioPacket
	head   int // index of the oldest packet
	n      int // packets queued
	closed bool
	notify chan struct{} // capacity 1: "something was pushed"

	dropped atomic.Uint64
}

// NewAudioQueue returns a queue holding at most size packets (minimum 1).
func NewAudioQueue(size int) *AudioQueue {
	if size < 1 {
		size = 1
	}
	return &AudioQueue{buf: make([]AudioPacket, size), notify: make(chan struct{}, 1)}
}

// Push enqueues p, dropping the oldest packet when full. It never blocks.
// The payload is copied: the caller's buffer is reused for the next read.
func (q *AudioQueue) Push(p AudioPacket) {
	p.Opus = append([]byte(nil), p.Opus...)
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	if q.n == len(q.buf) {
		q.buf[q.head] = AudioPacket{}
		q.head = (q.head + 1) % len(q.buf)
		q.n--
		q.dropped.Add(1)
	}
	q.buf[(q.head+q.n)%len(q.buf)] = p
	q.n++
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// TryPop returns the oldest packet, if any.
func (q *AudioQueue) TryPop() (AudioPacket, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.n == 0 {
		return AudioPacket{}, false
	}
	p := q.buf[q.head]
	q.buf[q.head] = AudioPacket{}
	q.head = (q.head + 1) % len(q.buf)
	q.n--
	return p, true
}

// Pop waits for the oldest packet. It returns ErrClosed once the queue is
// closed and empty, or ctx's error.
func (q *AudioQueue) Pop(ctx context.Context) (AudioPacket, error) {
	for {
		if p, ok := q.TryPop(); ok {
			return p, nil
		}
		q.mu.Lock()
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return AudioPacket{}, ErrClosed
		}
		select {
		case <-q.notify:
		case <-ctx.Done():
			return AudioPacket{}, ctx.Err()
		}
	}
}

// Len is how many packets are queued.
func (q *AudioQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.n
}

// Dropped is how many packets overflow has discarded.
func (q *AudioQueue) Dropped() uint64 { return q.dropped.Load() }

// Close stops the queue: later pushes are discarded and Pop returns
// ErrClosed once it has drained what is queued.
func (q *AudioQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}
