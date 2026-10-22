package transport

// R74 moderation reach (docs/76 §6.9) on the transport. A home's PoP ban
// forwarder chains on HandleBanAdded through AddBanObserver, so every ban
// record the ban source applies — every source, every resync — reaches it
// on the same callback that actuates the ban locally. The screening-notice
// override a linked PoP sets lives beside sendRelayIdentity (telemetry.go).

import (
	"sync"
	"sync/atomic"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// popModeration is the Server's R74 moderation state.
type popModeration struct {
	// screening is the dynamic screening notice (SetScreeningNotice).
	screening atomic.Bool
	// observers are told of every ban record, copy-on-write under obsMu.
	obsMu     sync.Mutex
	observers atomic.Pointer[[]func(moderation.Record)]
}

// AddBanObserver registers fn for every record HandleBanAdded sees,
// expired and unparseable ones included: the observer filters. fn runs on
// the ban source's callback, before the local kill, and must not block.
// Safe while serving.
func (s *Server) AddBanObserver(fn func(moderation.Record)) {
	if fn == nil {
		return
	}
	m := &s.popMod
	m.obsMu.Lock()
	defer m.obsMu.Unlock()
	var next []func(moderation.Record)
	if cur := m.observers.Load(); cur != nil {
		next = append(next, *cur...)
	}
	next = append(next, fn)
	m.observers.Store(&next)
}

func (s *Server) notifyBanObservers(rec moderation.Record) {
	obs := s.popMod.observers.Load()
	if obs == nil {
		return
	}
	for _, fn := range *obs {
		fn(rec)
	}
}
