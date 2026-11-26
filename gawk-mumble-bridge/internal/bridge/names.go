package bridge

import (
	"strings"
	"sync"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
)

// nameRegistry is the pod's view of which Mumble names are taken.
//
// Murmur refuses a name already in use (UsernameInUse) only when the second
// login comes from a DIFFERENT address. From the same address it treats the
// second login as the same person reconnecting and disconnects the first as
// a ghost ("You connected to the server from another device"; measured on
// murmur 1.5.939 and 1.6.870). Every bridged connection of a pod shares the
// pod's address, so two gawk users called Alice on one pod would kick each
// other. The registry prevents that: a name this pod holds, or that any live
// connection of this pod sees on the server, is skipped before dialling.
//
// R88 (docs/92 D12): with gawk-mmproxy, murmur sees each user at the
// browser's address, so the ghost rule keys on the person, across pods. A
// pod with no live connection would dial blind, so in proxy mode with Ice
// the registry also consults the Ice mirror, the one fleet-wide user list a
// pod keeps. It is set only then: without the proxy, name selection is
// exactly R79's (G7).
type nameRegistry struct {
	mu      sync.Mutex
	held    map[string]int
	clients map[*mumble.Client]struct{}
	// onServer, when set, reports a name (lowercased) the Ice mirror lists.
	onServer func(key string) bool
}

func newNameRegistry() *nameRegistry {
	return &nameRegistry{held: map[string]int{}, clients: map[*mumble.Client]struct{}{}}
}

// claim reserves name (case-insensitively, murmur's comparison) and returns
// its release, or false when the name is taken.
func (r *nameRegistry) claim(name string) (func(), bool) {
	key := strings.ToLower(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held[key] > 0 {
		return nil, false
	}
	for c := range r.clients {
		for _, u := range c.Users() {
			if strings.ToLower(u.Name) == key {
				return nil, false
			}
		}
	}
	if r.onServer != nil && r.onServer(key) {
		return nil, false
	}
	r.held[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.held[key]--; r.held[key] <= 0 {
				delete(r.held, key)
			}
			r.mu.Unlock()
		})
	}, true
}

// attach adds a live connection whose user list the registry consults.
func (r *nameRegistry) attach(c *mumble.Client) {
	r.mu.Lock()
	r.clients[c] = struct{}{}
	r.mu.Unlock()
	go func() {
		<-c.Done()
		r.mu.Lock()
		delete(r.clients, c)
		r.mu.Unlock()
	}()
}
