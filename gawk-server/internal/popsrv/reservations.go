package popsrv

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
)

// Reserve results (docs/76 item 14): the PoP's reserve record is answered
// with one of these.
const (
	// ReserveOK: the ID is now this PoP's reservation.
	ReserveOK = "ok"
	// ReserveConflict: the ID is taken (a home broadcast or any PoP's
	// reservation); the PoP keeps the broadcast local-only.
	ReserveConflict = "conflict"
	// ReserveLimit: the PoP is at its spec.limits.maxBroadcasts; the
	// broadcast stays party-only.
	ReserveLimit = "limit"
)

// Reservations is the home fleet's record of which broadcast IDs belong to
// which PoP (docs/76 item 14). The link-home pod writes it on the PoP's
// behalf; every pod reads it — the mint's IDReserved through Owner, and the
// subscribe miss through the cluster Lease itself.
type Reservations interface {
	// Reserve makes id pop's reservation unless it is taken (ReserveConflict)
	// or pop already holds limit reservations (ReserveLimit; limit <= 0 is
	// unlimited). err is only for a failure to decide.
	Reserve(ctx context.Context, id, pop string, limit int) (result string, err error)
	// Release ends one reservation (the PoP ended the broadcast). Missing is
	// a no-op; another PoP's is cluster.ErrReserved and is kept.
	Release(ctx context.Context, id, pop string) error
	// ReleaseAll ends every reservation of pop (a revoke).
	ReleaseAll(ctx context.Context, pop string) error
	// RenewAll keeps pop's dormant reservations off the cluster janitor;
	// the link-home pod calls it every poplink.LeaseRenew.
	RenewAll(ctx context.Context, pop string) error
	// Owner names the PoP id is reserved for. Non-blocking (the lease cache
	// or a map), so it is safe under the hub's lock: it backs the mint's
	// IDReserved. In cluster mode it is false until the cache has synced.
	Owner(id string) (pop string, ok bool)
	// List is pop's reserved IDs, sorted; non-blocking like Owner.
	List(pop string) []string
}

// NewClusterReservations keeps reservations as gawk/pop Leases (cluster
// mode): creation is the atomic reservation, so two pods racing for one ID
// cannot both win, and a home mint's claim of a reserved ID is refused by
// the coordinator itself.
func NewClusterReservations(c *cluster.Coordinator) Reservations {
	return &clusterReservations{c: c}
}

type clusterReservations struct{ c *cluster.Coordinator }

func (r *clusterReservations) Reserve(ctx context.Context, id, pop string, limit int) (string, error) {
	if limit > 0 {
		// Counted from the API, not the cache: only the link-home pod
		// reserves for a PoP, so the count it just wrote is the count.
		n, err := r.c.CountReservations(ctx, pop)
		if err != nil {
			return "", err
		}
		if n >= limit {
			return ReserveLimit, nil
		}
	}
	switch err := r.c.Reserve(ctx, id, pop); {
	case err == nil:
		return ReserveOK, nil
	case errors.Is(err, cluster.ErrConflict):
		return ReserveConflict, nil
	default:
		return "", err
	}
}

func (r *clusterReservations) Release(ctx context.Context, id, pop string) error {
	return r.c.DeleteReservation(ctx, id, pop)
}

func (r *clusterReservations) ReleaseAll(ctx context.Context, pop string) error {
	ids, err := r.c.ReservedIDs(ctx, pop)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := r.c.DeleteReservation(ctx, id, pop); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *clusterReservations) RenewAll(ctx context.Context, pop string) error {
	return r.c.RenewReservations(ctx, pop)
}

func (r *clusterReservations) Owner(id string) (string, bool) {
	origin, _, _, ok := r.c.Lookup(id)
	if !ok || origin.Pop == "" {
		return "", false
	}
	return origin.Pop, true
}

func (r *clusterReservations) List(pop string) []string { return r.c.ListReservations(pop) }

// NewMemReservations keeps reservations in memory, for a single-pod home
// (no cluster Leases). It knows only reservations, not the home's own
// broadcasts: an ID collision with a home broadcast is kept off by the
// mint's IDReserved on one side and the PoP minting IDs the home never
// announced on the other.
func NewMemReservations() Reservations {
	return &memReservations{owner: make(map[string]string)}
}

type memReservations struct {
	mu    sync.Mutex
	owner map[string]string // id -> pop
}

func (r *memReservations) Reserve(_ context.Context, id, pop string, limit int) (string, error) {
	if pop == "" {
		return "", errors.New("popsrv: Reserve needs a PoP name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.owner[id]; taken {
		return ReserveConflict, nil
	}
	if limit > 0 && r.countLocked(pop) >= limit {
		return ReserveLimit, nil
	}
	r.owner[id] = pop
	return ReserveOK, nil
}

func (r *memReservations) countLocked(pop string) int {
	n := 0
	for _, p := range r.owner {
		if p == pop {
			n++
		}
	}
	return n
}

func (r *memReservations) Release(_ context.Context, id, pop string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner, ok := r.owner[id]
	if !ok {
		return nil
	}
	if owner != pop {
		return fmt.Errorf("%w: %s is not %q's reservation", cluster.ErrReserved, id, pop)
	}
	delete(r.owner, id)
	return nil
}

func (r *memReservations) ReleaseAll(_ context.Context, pop string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.owner {
		if p == pop {
			delete(r.owner, id)
		}
	}
	return nil
}

// RenewAll is a no-op: nothing expires an in-memory reservation but its
// release, and a home restart forgets them all (the PoP re-reserves on
// relink).
func (r *memReservations) RenewAll(context.Context, string) error { return nil }

func (r *memReservations) Owner(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pop, ok := r.owner[id]
	return pop, ok
}

func (r *memReservations) List(pop string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id, p := range r.owner {
		if p == pop {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}
