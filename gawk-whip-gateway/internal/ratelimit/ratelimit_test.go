package ratelimit

import (
	"net/netip"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	l := New(1, 5, 2)
	a := netip.MustParseAddr("203.0.113.7")
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow(a, t0); !ok {
			t.Fatalf("burst %d refused", i)
		}
	}
	ok, wait := l.Allow(a, t0)
	if ok || wait != time.Second {
		t.Fatalf("over the burst: %v %v", ok, wait)
	}
	if ok, _ := l.Allow(a, t0.Add(time.Second)); !ok {
		t.Fatal("refilled token refused")
	}
	if ok, _ := l.Allow(netip.MustParseAddr("203.0.113.8"), t0); !ok {
		t.Fatal("another address shares the bucket")
	}
	l.Allow(netip.MustParseAddr("203.0.113.9"), t0) // evicts the LRU bucket (a)
	if ok, _ := l.Allow(a, t0.Add(time.Second)); !ok {
		t.Fatal("an evicted bucket starts full")
	}
	if ok, _ := New(0, 1, 1).Allow(a, t0); !ok {
		t.Fatal("rate 0 disables the limit")
	}
}
