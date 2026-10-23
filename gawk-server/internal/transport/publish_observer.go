package transport

import (
	"net/http"
	"slices"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

// PublishObserver is told when a publisher session is accepted (R74,
// docs/76): after the post-upgrade ban re-check, on both the mint and the
// claim path. It MUST NOT block: it runs on the publish handler's goroutine
// before the broadcast is announced.
type PublishObserver interface {
	PublisherLive(r *http.Request, id string)
}

// AddPublishObserver registers o. Copy-on-write, so the publish path reads
// the list without a lock and observers may be added while serving.
func (s *Server) AddPublishObserver(o PublishObserver) {
	s.publishObserversMu.Lock()
	defer s.publishObserversMu.Unlock()
	cur := s.publishObservers.Load()
	var next []PublishObserver
	if cur != nil {
		next = slices.Clone(*cur)
	}
	next = append(next, o)
	s.publishObservers.Store(&next)
}

// publisherLive notifies every registered PublishObserver.
func (s *Server) publisherLive(r *http.Request, id string, _ *hub.Publisher) {
	obs := s.publishObservers.Load()
	if obs == nil {
		return
	}
	for _, o := range *obs {
		o.PublisherLive(r, id)
	}
}
