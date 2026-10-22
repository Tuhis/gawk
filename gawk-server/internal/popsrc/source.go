// Package popsrc feeds R74 Pop definitions (docs/76 §9) to the home side:
// the Pop CRs through an informer in cluster mode (StartK8s), or a
// -pops-file of pops.FilePop entries elsewhere (StartFile). Both are read
// only here; the status writer that pairing and the link lease need lives
// beside them (status.go, W2).
//
// Every reader gets a deep copy, so a caller mutating what it got can never
// corrupt the next authentication.
package popsrc

import (
	"reflect"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

// Source is the read side of the Pop definitions.
type Source interface {
	// Get returns a deep copy of the named Pop.
	Get(name string) (*pops.Pop, bool)
	// List returns deep copies of every Pop, sorted by name.
	List() []pops.Pop
	// Subscribe registers fn for every add, update and delete, called with
	// the Pop's name from the source's goroutine, never under its lock.
	Subscribe(fn func(name string)) (cancel func())
	// Synced reports whether the first full load has landed.
	Synced() bool
}

// store is the shared in-memory half of both sources.
type store struct {
	mu   sync.RWMutex
	pops map[string]*pops.Pop

	subMu  sync.Mutex
	subs   map[uint64]func(string)
	nextID uint64

	synced atomic.Bool
}

func newStore() *store {
	return &store{pops: map[string]*pops.Pop{}, subs: map[uint64]func(string){}}
}

func (s *store) Get(name string) (*pops.Pop, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pops[name]
	if !ok {
		return nil, false
	}
	return p.DeepCopy(), true
}

func (s *store) List() []pops.Pop {
	s.mu.RLock()
	out := make([]pops.Pop, 0, len(s.pops))
	for _, p := range s.pops {
		out = append(out, *p.DeepCopy())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *store) Subscribe(fn func(string)) func() {
	s.subMu.Lock()
	id := s.nextID
	s.nextID++
	s.subs[id] = fn
	s.subMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.subMu.Lock()
			delete(s.subs, id)
			s.subMu.Unlock()
		})
	}
}

func (s *store) Synced() bool { return s.synced.Load() }

func (s *store) notify(names []string) {
	if len(names) == 0 {
		return
	}
	s.subMu.Lock()
	fns := make([]func(string), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.subMu.Unlock()
	for _, name := range names {
		for _, fn := range fns {
			fn(name)
		}
	}
}

// put stores a copy of p and reports whether anything changed.
func (s *store) put(p *pops.Pop) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.pops[p.Name]; ok && reflect.DeepEqual(old, p) {
		return false
	}
	s.pops[p.Name] = p.DeepCopy()
	return true
}

// remove deletes name and reports whether it was there.
func (s *store) remove(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pops[name]; !ok {
		return false
	}
	delete(s.pops, name)
	return true
}

// replace makes the set exactly list and returns the names that changed,
// sorted.
func (s *store) replace(list []pops.Pop) []string {
	next := make(map[string]*pops.Pop, len(list))
	for i := range list {
		next[list[i].Name] = list[i].DeepCopy()
	}
	s.mu.Lock()
	var changed []string
	for name, old := range s.pops {
		if p, ok := next[name]; !ok || !reflect.DeepEqual(old, p) {
			changed = append(changed, name)
		}
	}
	for name := range next {
		if _, ok := s.pops[name]; !ok {
			changed = append(changed, name)
		}
	}
	s.pops = next
	s.mu.Unlock()
	sort.Strings(changed)
	return changed
}
