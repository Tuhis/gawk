package eventbus

import (
	"encoding/json"
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// The delta types — broadcast.viewers and room.attachment_updated — are the
// owner's explicit ask and the reason this file exists: a thousand-viewer
// broadcast must not be a thousand messages a second, and a dashboard still
// wants a five-second-fresh number (docs/51 D3).
//
// Two rules, both of them what "at most one per key per interval, and only on
// change" means:
//
//   - Unchanged data is never published. A quiet room produces nothing at all,
//     so an idle fleet costs an idle stream.
//   - A change inside the interval is held, not dropped: the LATEST value is
//     published when the interval elapses. The consumer's view is at most one
//     interval stale and never wrong.
//
// Every other type is a discrete transition and goes straight through.
type coalescer struct {
	interval time.Duration
	state    map[string]*coalesced
	isDeltaT map[string]bool
}

type coalesced struct {
	last    []byte    // the data last published, for the change test
	pending *Event    // held because the interval had not elapsed
	sentAt  time.Time // when the last publish happened
}

func newCoalescer(interval time.Duration) *coalescer {
	deltas := map[string]bool{
		// The two coalesced types. Named here rather than read from the
		// contract package because "is this high-rate?" is a property of how
		// the RELAY publishes a type, not of the type's schema.
		events.TypeBroadcastViewers:      true,
		events.TypeRoomAttachmentUpdated: true,
	}
	return &coalescer{
		interval: interval,
		state:    make(map[string]*coalesced),
		isDeltaT: deltas,
	}
}

func (c *coalescer) isDelta(typ string) bool { return c.isDeltaT[typ] }

// offer takes a delta event and reports the one to publish now, if any.
func (c *coalescer) offer(ev Event, now time.Time) (Event, bool) {
	k := ev.Type + "|" + ev.Key
	body, err := json.Marshal(ev.Data)
	if err != nil {
		// Unencodable data will fail in send() too, with a counted reason;
		// let it through rather than swallowing it here.
		return ev, true
	}
	st := c.state[k]
	if st == nil {
		st = &coalesced{}
		c.state[k] = st
	}
	if st.last != nil && string(st.last) == string(body) {
		// Back to what was last published. Any held value is now a value that
		// never was: publishing it at the next flush would report a count the
		// thing no longer has, and leave it wrong until something else moves.
		// A viewer count that ticks 5 → 6 → 5 inside one interval is exactly
		// this, and it is the common shape of a fluctuating count.
		st.pending = nil
		return Event{}, false // nothing changed
	}
	if now.Sub(st.sentAt) >= c.interval {
		st.last, st.pending, st.sentAt = body, nil, now
		return ev, true
	}
	held := ev
	st.pending = &held
	return Event{}, false
}

// flush returns the held events whose interval has elapsed. The publisher
// calls it on a ticker, which is what bounds the staleness of a value that
// changed just after a publish.
func (c *coalescer) flush(now time.Time) []Event {
	var out []Event
	for k, st := range c.state {
		if st.pending == nil {
			// Forget keys that have gone quiet for a while, so a fleet that
			// has churned through broadcasts does not keep their state.
			if now.Sub(st.sentAt) > 10*c.interval {
				delete(c.state, k)
			}
			continue
		}
		if now.Sub(st.sentAt) < c.interval {
			continue
		}
		ev := *st.pending
		if body, err := json.Marshal(ev.Data); err == nil {
			st.last = body
		}
		st.pending, st.sentAt = nil, now
		out = append(out, ev)
	}
	return out
}
