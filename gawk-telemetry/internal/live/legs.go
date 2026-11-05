package live

// R36 TU5: a striped viewer is one participant with N legs.
//
// Since R30 a live-edge viewer can carry its deltas on up to four extra leg
// sessions, each an ordinary relay subscriber, possibly on another pod. A leg
// dials before — and is never sent — the telemetry hello, so it has no
// sessionId; the one thing it shares with its viewer's primary session is the
// relay's stripeOwner key, an HMAC of the viewer's ?owner= token under the
// fleet-shared stats key. That is the join, and the only one: a leg whose
// owner matches no primary is counted in BroadcastView.StripeLegs and attached
// to nobody, rather than guessed onto a viewer.
//
// Legs live in their own map, never among the sessions, so no count or peer
// baseline that walks the sessions can meet one.

import (
	"sort"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
)

// observeLegsLocked folds the stripe legs of one pod's broadcast record. The
// record is where legs are: the scraper writes no per-subscriber record for a
// leg, and the broadcast record keeps every subscriberDetails entry.
func (b *broadcastState) observeLegsLocked(o relayscrape.Observation, now time.Time) {
	for _, sub := range o.Broadcast.SubscriberDetails {
		if !sub.StripeLeg {
			continue
		}
		id := o.Pod + "\x00" + sub.Key
		l, ok := b.legs[id]
		if !ok {
			l = &legState{}
			b.legs[id] = l
		} else if l.lastRelay.Before(now) {
			// Rotated once per round, so a pod answering twice in one round
			// cannot make a leg's window its own counter minus itself.
			l.prev, l.have = l.cur, true
		}
		l.pod, l.role, l.issuer = o.Pod, o.Role, o.Issuer()
		l.cur, l.lastRelay = sub, now
	}
}

// legViewsLocked renders the broadcast's legs: the views attributed to each
// session that owns some, and how many legs the relay holds in all.
//
// A leg attaches to the session whose primary entry carries the same owner
// key in its latest scrape AND was served by the same relay (docs/78 D8). A
// PoP's observations carry no owner key, so in practice a PoP's legs are
// counted and attached to nobody; the relay check keeps it that way if one
// ever did. Owner tokens are minted per connection attempt, so a reconnect's
// primary carries a new key and the dead attempt's legs match nobody; two
// sessions sharing one key is not expected, and if it happens the session
// the relay saw most recently takes the legs, the other none rather than a
// copy — one leg is never two viewers' delivery.
func (b *broadcastState) legViewsLocked(now time.Time) (map[*sessionState][]LegView, int) {
	type owner struct{ issuer, key string }
	holder := map[owner]*sessionState{}
	for _, s := range b.sessions {
		if s.stripeOwner == "" {
			continue
		}
		k := owner{s.view.Issuer, s.stripeOwner}
		if h, ok := holder[k]; !ok || s.lastRelay.After(h.lastRelay) ||
			(s.lastRelay.Equal(h.lastRelay) && s.view.SessionID < h.view.SessionID) {
			holder[k] = s
		}
	}

	live := make([]*legState, 0, len(b.legs))
	for _, l := range b.legs {
		// Gone quiet but not yet swept: not a leg the relay holds now.
		if now.Sub(l.lastRelay) > RelayStaleAfter {
			continue
		}
		live = append(live, l)
	}
	// Deterministic, so a card does not depend on map order or on which pod
	// answered first.
	sort.Slice(live, func(i, j int) bool {
		a, c := live[i], live[j]
		if a.cur.StripeMember != c.cur.StripeMember {
			return a.cur.StripeMember < c.cur.StripeMember
		}
		if a.pod != c.pod {
			return a.pod < c.pod
		}
		return a.cur.Key < c.cur.Key
	})

	out := map[*sessionState][]LegView{}
	for _, l := range live {
		if l.cur.StripeOwner == "" {
			continue
		}
		s := holder[owner{l.issuer, l.cur.StripeOwner}]
		if s == nil {
			continue
		}
		v := LegView{
			Member: l.cur.StripeMember, N: l.cur.StripeN,
			Pod: l.pod, Role: l.role,
			QueueDepth: l.cur.QueueDepth,
			RelayAgeMs: ageMs(l.lastRelay, now),
		}
		if d, ok := deltaOf(l.cur.Dropped, l.prev.Dropped, l.have); ok {
			v.DatagramsDropped = &d
		}
		out[s] = append(out[s], v)
	}
	return out, len(live)
}
