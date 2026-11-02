package status

import (
	"container/list"
	"sync"
	"time"
)

// Hash is the SHA-256 of a full stream key: the only thing records are keyed
// by, so knowing a broadcast code never lets anyone read or write another
// broadcaster's record (D9).
type Hash [32]byte

// Table is the bounded per-key record store: an LRU of at most Max records,
// each forgotten TTL after its last change (or at its backoff's end, if
// later). Bounded because a 503 or 400 is recorded before the relay dial,
// for a key the gateway never verified (D9). Live records are never evicted
// for space: the session cap bounds them far below Max.
type Table struct {
	mu  sync.Mutex
	max int
	ttl time.Duration
	ll  *list.List // front = most recently used
	m   map[Hash]*list.Element
}

type entry struct {
	h   Hash
	rec Record
}

// NewTable builds a table.
func NewTable(max int, ttl time.Duration) *Table {
	if max < 1 {
		max = 1
	}
	return &Table{max: max, ttl: ttl, ll: list.New(), m: make(map[Hash]*list.Element)}
}

// Len is the number of records held (expired ones included until touched).
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ll.Len()
}

// lookup returns the live element for h, dropping it if expired.
func (t *Table) lookup(h Hash, now time.Time) *list.Element {
	el, ok := t.m[h]
	if !ok {
		return nil
	}
	e := el.Value.(*entry)
	if !e.rec.Live() && !now.Before(e.rec.expiresAt(t.ttl)) {
		t.ll.Remove(el)
		delete(t.m, h)
		return nil
	}
	return el
}

// Get returns a copy of h's record.
func (t *Table) Get(h Hash, now time.Time) (Record, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el := t.lookup(h, now)
	if el == nil {
		return Record{}, false
	}
	t.ll.MoveToFront(el)
	return el.Value.(*entry).rec, true
}

// Update applies fn to h's record, creating an empty one if there is none,
// then stamps UpdatedAt and bumps Version. It returns the new record.
func (t *Table) Update(h Hash, now time.Time, fn func(*Record)) Record {
	t.mu.Lock()
	defer t.mu.Unlock()
	el := t.lookup(h, now)
	if el == nil {
		el = t.ll.PushFront(&entry{h: h})
		t.m[h] = el
		t.evict()
	} else {
		t.ll.MoveToFront(el)
	}
	e := el.Value.(*entry)
	fn(&e.rec)
	e.rec.UpdatedAt = now
	e.rec.Version++
	return e.rec
}

// Put stores rec as h's record (D10's takeover adopting a peer's copy). It
// keeps the peer's UpdatedAt, so the newest-record rule still compares the
// time the record last changed, and bumps Version past any local copy.
func (t *Table) Put(h Hash, rec Record) Record {
	t.mu.Lock()
	defer t.mu.Unlock()
	var prev uint64
	if el, ok := t.m[h]; ok {
		prev = el.Value.(*entry).rec.Version
		t.ll.Remove(el)
		delete(t.m, h)
	}
	if rec.Version <= prev {
		rec.Version = prev + 1
	}
	t.m[h] = t.ll.PushFront(&entry{h: h, rec: rec})
	t.evict()
	return rec
}

// Delete forgets h.
func (t *Table) Delete(h Hash) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.m[h]; ok {
		t.ll.Remove(el)
		delete(t.m, h)
	}
}

// DeleteIfVersion forgets h only if its record is still at version v and not
// live: D10's acknowledged takeover drops exactly what the other pod adopted.
func (t *Table) DeleteIfVersion(h Hash, v uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.m[h]
	if !ok {
		return false
	}
	e := el.Value.(*entry)
	if e.rec.Version != v || e.rec.Live() {
		return false
	}
	t.ll.Remove(el)
	delete(t.m, h)
	return true
}

// evict drops least-recently-used non-live records beyond max.
func (t *Table) evict() {
	for el := t.ll.Back(); t.ll.Len() > t.max && el != nil; {
		prev := el.Prev()
		e := el.Value.(*entry)
		if !e.rec.Live() {
			t.ll.Remove(el)
			delete(t.m, e.h)
		}
		el = prev
	}
}
