package kube_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// errRecordsDown stands in for "Postgres is unreachable" — the condition under
// which the reconciler must not garbage-collect a single CR (docs/42 §6).
var errRecordsDown = errors.New("postgres is unreachable")

// fakeRecords is an in-memory kube.Records. Using it rather than a real store
// keeps the reconciler's rules (convergence, adoption, no-GC-without-Postgres)
// testable without a database, which is exactly what makes the failure-mode
// test possible at all.
type fakeRecords struct {
	mu     sync.Mutex
	bans   []store.Ban
	events []store.Event
	// down makes every method fail, simulating an unreachable database.
	down bool
	now  func() time.Time
}

func newFakeRecords() *fakeRecords {
	return &fakeRecords{now: time.Now}
}

func (f *fakeRecords) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakeRecords) add(b store.Ban) store.Ban {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.insertLocked(b)
}

func (f *fakeRecords) insertLocked(b store.Ban) store.Ban {
	rec, err := moderation.Normalize(b.Record())
	if err != nil {
		panic(err)
	}
	b.Target = rec.Target
	if name, err := moderation.CRName(b.Target); err == nil {
		b.CRName = name
	}
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	if b.State == "" {
		b.State = store.BanActive
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = f.now()
	}
	f.bans = append(f.bans, b)
	return b
}

func (f *fakeRecords) snapshot() []store.Ban {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Ban(nil), f.bans...)
}

func (f *fakeRecords) eventTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.events))
	for _, e := range f.events {
		out = append(out, e.Type)
	}
	return out
}

func (f *fakeRecords) ExpireDueBans(_ context.Context, now time.Time) ([]store.Ban, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errRecordsDown
	}
	var moved []store.Ban
	for i := range f.bans {
		b := &f.bans[i]
		if b.State != store.BanActive || b.ExpiresAt == nil || b.ExpiresAt.After(now) {
			continue
		}
		b.State = store.BanExpired
		moved = append(moved, *b)
	}
	return moved, nil
}

func (f *fakeRecords) ListBans(_ context.Context, state string) ([]store.Ban, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errRecordsDown
	}
	out := []store.Ban{}
	for _, b := range f.bans {
		if state == store.FilterAll || b.State == store.BanState(state) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeRecords) CreateBan(_ context.Context, b store.Ban) (store.Ban, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return store.Ban{}, errRecordsDown
	}
	rec, err := moderation.Normalize(b.Record())
	if err != nil {
		return store.Ban{}, err
	}
	for _, existing := range f.bans {
		if existing.State == store.BanActive && existing.Target == rec.Target {
			return store.Ban{}, store.ErrDuplicateActive
		}
	}
	return f.insertLocked(b), nil
}

func (f *fakeRecords) RoomEndedSince(_ context.Context, room string, since time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, errRecordsDown
	}
	for _, e := range f.events {
		if e.Type == store.EventRoomEnded && e.PayloadString(store.PayloadRoom) == room && !e.OccurredAt.Before(since) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRecords) AppendEvent(_ context.Context, e store.Event) (store.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return store.Event{}, errRecordsDown
	}
	e.ID = int64(len(f.events) + 1)
	f.events = append(f.events, e)
	return e, nil
}

// countingBans wraps a BanClient and records every write, so a test can assert
// that two replicas under leader election produced no duplicate CR writes.
type countingBans struct {
	inner kube.BanClient

	mu      sync.Mutex
	upserts int
	deletes int
	stamps  int
	// failList makes the k8s API look unreachable.
	failList bool
}

func (c *countingBans) List(ctx context.Context) ([]kube.BanObject, error) {
	c.mu.Lock()
	fail := c.failList
	c.mu.Unlock()
	if fail {
		return nil, errors.New("kubernetes API is unreachable")
	}
	return c.inner.List(ctx)
}

func (c *countingBans) Upsert(ctx context.Context, rec moderation.Record, banID string) error {
	c.mu.Lock()
	c.upserts++
	c.mu.Unlock()
	return c.inner.Upsert(ctx, rec, banID)
}

func (c *countingBans) Adopt(ctx context.Context, name, banID string) error {
	c.mu.Lock()
	c.stamps++
	c.mu.Unlock()
	return c.inner.Adopt(ctx, name, banID)
}

func (c *countingBans) Delete(ctx context.Context, name string) error {
	c.mu.Lock()
	c.deletes++
	c.mu.Unlock()
	return c.inner.Delete(ctx, name)
}

func (c *countingBans) counts() (upserts, deletes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.upserts, c.deletes
}

// remove is the portal's unban, applied straight to the record store: the row
// leaves `active` without going near the CR, which is what a test needs to
// then assert what the next sweep does to the objects left behind.
func (f *fakeRecords) remove(id uuid.UUID) store.Ban {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.bans {
		if f.bans[i].ID != id {
			continue
		}
		now := f.now()
		f.bans[i].State = store.BanRemoved
		f.bans[i].RemovedAt = &now
		return f.bans[i]
	}
	panic("no such ban")
}
