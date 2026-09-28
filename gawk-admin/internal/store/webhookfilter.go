package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// The per-webhook event filter (R49, docs/50 D6, D8).
//
// A webhook's filter is either ABSENT (nil) — every moderation event and no
// activity event, which is exactly what every webhook received before R49 — or
// an exact list of webhook-eligible row types. The four room activity types
// are deliverable only to a webhook that lists them, so the ops pager that
// never asked for a join is never paged by one.

// FilterParticipantRejoined is the one filter entry that is not an event type.
//
// When a room moves to a new home pod (docs/51 D9) its participants reconnect,
// and the bus says so with a pair: participant_left{reason: home_moved} from
// the old home, then participant_joined{rejoin: true} from the new one.
// Nobody arrived and nobody left, so both halves are dropped by default; a
// webhook that lists this token receives them (as the two event types they
// are — there is no `room.participant_rejoined` event).
const FilterParticipantRejoined = "room.participant_rejoined"

// ConfigWebhook is one enabled chart-defined webhook as the enqueue sees it:
// its name and its filter. Config webhooks are not rows, so the recording
// path hands them to the store with every event.
type ConfigWebhook struct {
	Name string
	// Events is the filter; nil means the default.
	Events []string
}

// WebhookFilterTypes is every value a webhook's `events` list may hold: the
// webhook-eligible row types plus FilterParticipantRejoined.
func WebhookFilterTypes() []string {
	return append(WebhookEventTypes(), FilterParticipantRejoined)
}

// ValidateWebhookEvents checks a filter list. nil is valid (the default); an
// empty list is valid and receives nothing; any entry outside
// WebhookFilterTypes is an error naming it.
func ValidateWebhookEvents(list []string) error {
	known := WebhookFilterTypes()
	for _, e := range list {
		if !slices.Contains(known, e) {
			return fmt.Errorf("unknown webhook event %q: must be one of %s", e, strings.Join(known, ", "))
		}
	}
	return nil
}

// WebhookWants reports whether a webhook with this filter receives ev.
//
// It is the ONE decision: the enqueue consults it for both webhook sources,
// so a chart-defined receiver and a UI-created one cannot disagree about what
// "subscribed" means.
func WebhookWants(filter []string, ev Event) bool {
	if !isWebhookEventType(ev.Type) {
		return false
	}
	if filter == nil {
		return slices.Contains(AllEventTypes(), ev.Type)
	}
	if isRejoinHalf(ev) {
		return slices.Contains(filter, FilterParticipantRejoined)
	}
	return slices.Contains(filter, ev.Type)
}

// isRejoinHalf reports whether ev is one half of an adoption pair: a departure
// because the room moved, or an arrival flagged as a reconnection. It reads
// the bus event the row carries verbatim (activityPayload's "event" key).
func isRejoinHalf(ev Event) bool {
	if ev.Type != EventRoomParticipantLeft && ev.Type != EventRoomParticipantJoined {
		return false
	}
	var p struct {
		Event struct {
			Data struct {
				Reason string `json:"reason"`
				Rejoin bool   `json:"rejoin"`
			} `json:"data"`
		} `json:"event"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return false
	}
	if ev.Type == EventRoomParticipantLeft {
		return p.Event.Data.Reason == events.ParticipantLeftHomeMoved
	}
	return p.Event.Data.Rejoin
}
