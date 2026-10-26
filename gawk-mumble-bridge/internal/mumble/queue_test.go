package mumble

import (
	"context"
	"errors"
	"testing"
	"time"
)

// D6: a full queue drops its OLDEST packet, counts it, and never blocks the
// pusher (the connection's read loop).
func TestAudioQueueDropsOldestWithoutBlocking(t *testing.T) {
	q := NewAudioQueue(4)
	buf := []byte{0}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			buf[0] = byte(i) // the queue must copy: the reader reuses its buffer
			q.Push(AudioPacket{Seq: int64(i), Opus: buf})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Push blocked with nobody popping")
	}
	if q.Len() != 4 || q.Dropped() != 6 {
		t.Fatalf("len %d dropped %d, want 4 and 6", q.Len(), q.Dropped())
	}
	for want := int64(6); want < 10; want++ {
		p, ok := q.TryPop()
		if !ok || p.Seq != want || p.Opus[0] != byte(want) {
			t.Fatalf("pop %+v %v, want seq %d", p, ok, want)
		}
	}
	if _, ok := q.TryPop(); ok {
		t.Fatal("queue should be empty")
	}
}

func TestAudioQueuePopWaitsAndCloses(t *testing.T) {
	q := NewAudioQueue(0) // clamps to 1
	go func() {
		time.Sleep(20 * time.Millisecond)
		q.Push(AudioPacket{Seq: 1, Opus: []byte{1}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p, err := q.Pop(ctx)
	if err != nil || p.Seq != 1 {
		t.Fatalf("pop %+v %v", p, err)
	}
	q.Push(AudioPacket{Seq: 2, Opus: []byte{2}})
	q.Close()
	q.Push(AudioPacket{Seq: 3, Opus: []byte{3}}) // discarded after close
	if p, err := q.Pop(ctx); err != nil || p.Seq != 2 {
		t.Fatalf("queued packet survives close: %+v %v", p, err)
	}
	if _, err := q.Pop(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed and empty: %v", err)
	}
	short, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if _, err := NewAudioQueue(1).Pop(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx: %v", err)
	}
}
