package roomsrv

import (
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The R50 bus hooks (docs/51 D4). Every room transition already passes through
// broadcastLocked on its way to the participants, so that is where the bus is
// fed: one funnel, no second list of transitions to keep in step with the
// first. Mint, attach, end and the re-home release are the facts that are not
// participant-visible events and are published from their own call sites.
//
// HOME POD ONLY, by construction rather than by a check: a proxy pod has no
// room in its registry, so none of this runs there and a consumer sees each
// fact once.
//
// Everything here runs UNDER the registry lock. That is safe only because
// Publish is a non-blocking channel send — the same property the media path
// relies on. Nothing added here may grow a blocking call.

// emitLocked publishes one bus event. Nil-safe at every level: no hook, no
// obfuscator, no bus.
func (r *Registry) emitLocked(typ, code string, data any) {
	if r.opts.OnEvent == nil {
		return
	}
	r.opts.OnEvent(eventbus.Event{
		Type: typ,
		Key:  r.opts.Obfuscate(code),
		Time: r.opts.Now(),
		Data: data,
	})
}

// clientKind converts a wire.RoomClient* byte to the contract's name. An
// unknown byte maps to the viewer kind rather than failing: a producer must
// not drop an event because a newer client kind appeared, and the schema's
// vocabulary is closed.
func clientKind(kind uint8) string {
	switch kind {
	case wire.RoomClientWebBroadcaster:
		return events.ClientKindWebBroadcaster
	case wire.RoomClientNative:
		return events.ClientKindNative
	default:
		return events.ClientKindWebViewer
	}
}

// busEventLocked maps one wire room event onto its bus type. An event kind
// with no bus counterpart (RoomEnding, CommandRejected) publishes nothing:
// the room's end is published by busRoomClosedLocked, with the reason
// vocabulary a consumer can act on rather than the wire byte.
func (r *Registry) busEventLocked(rm *room, ev wire.RoomEvent) {
	if r.opts.OnEvent == nil {
		return
	}
	key := r.opts.Obfuscate(rm.code)
	switch ev.Kind {
	case wire.RoomEventParticipantJoined:
		r.emitLocked(events.TypeRoomParticipantJoined, rm.code, events.RoomParticipantJoinedData{
			RoomCode: rm.code, RoomKey: key,
			ParticipantID: int(ev.Participant.ID),
			Nickname:      ev.Participant.Nickname,
			ClientKind:    clientKind(ev.Participant.Kind),
			Streaming:     ev.Participant.Flags&wire.RoomParticipantFlagStreaming != 0,
			Speaking:      ev.Participant.Flags&wire.RoomParticipantFlagSpeaking != 0,
			// A join into a room this pod adopted is a re-announcement, not a
			// new person: the publisher knows, so it says so rather than
			// leaving every consumer to infer it from a left/join pair
			// (docs/51 D9).
			Rejoin: rm.adopted,
		})
	case wire.RoomEventParticipantUpdated:
		r.emitLocked(events.TypeRoomParticipantUpdated, rm.code, events.RoomParticipantUpdatedData{
			RoomCode: rm.code, RoomKey: key,
			ParticipantID: int(ev.Participant.ID),
			Nickname:      ev.Participant.Nickname,
			ClientKind:    clientKind(ev.Participant.Kind),
			Streaming:     ev.Participant.Flags&wire.RoomParticipantFlagStreaming != 0,
			Speaking:      ev.Participant.Flags&wire.RoomParticipantFlagSpeaking != 0,
		})
	case wire.RoomEventParticipantLeft:
		// Published by the leave path, for the same reason an attach is
		// published by attachLocked: the wire event carries only the id, and
		// the record that knows the nickname, the client kind and WHY the
		// session ended is out of the roster by the time this runs.
	case wire.RoomEventAttachmentAdded:
		// Published by attachLocked, the funnel every attach goes through —
		// including the ones with no participants to notify.
	case wire.RoomEventAttachmentRemoved:
		r.emitLocked(events.TypeRoomDetached, rm.code, events.RoomDetachedData{
			RoomCode: rm.code, RoomKey: key,
			BroadcastID:  ev.Attachment.BroadcastID,
			BroadcastKey: r.opts.Obfuscate(ev.Attachment.BroadcastID),
		})
	case wire.RoomEventAttachmentUpdated:
		// The high-rate one: fed by the registry's own refresh, coalesced in
		// the publisher to at most one per key per interval and only on
		// change (docs/51 D3).
		r.emitLocked(events.TypeRoomAttachmentUpdated, rm.code, events.RoomAttachmentUpdatedData{
			RoomCode: rm.code, RoomKey: key,
			BroadcastID:  ev.Attachment.BroadcastID,
			BroadcastKey: r.opts.Obfuscate(ev.Attachment.BroadcastID),
			Live:         ev.Attachment.Live,
			Viewers:      int(ev.Attachment.ViewerCount),
		})
	}
}

// busParticipantLeftLocked publishes one departure, with the reason the room
// or the session recorded.
//
// The room's reason outranks the session's: "the room ended" explains a
// departure better than "its control queue overflowed", and during a re-home
// it is the only one that is true — nobody left.
func (r *Registry) busParticipantLeftLocked(rm *room, p *Participant) {
	reason := events.ParticipantLeft
	switch {
	case rm.leaveReason != "":
		reason = rm.leaveReason
	case p.leaveReason != "":
		reason = p.leaveReason
	}
	r.emitLocked(events.TypeRoomParticipantLeft, rm.code, events.RoomParticipantLeftData{
		RoomCode:      rm.code,
		RoomKey:       r.opts.Obfuscate(rm.code),
		ParticipantID: int(p.id),
		Nickname:      p.nick,
		ClientKind:    clientKind(p.kind),
		Reason:        reason,
	})
}

// busHomeChangedLocked publishes a room's arrival on THIS pod after an
// adoption: the room moved, it did not open. previousPod is whoever the Room
// record last named as home, or empty when the lease was already released.
func (r *Registry) busHomeChangedLocked(rm *room, previousPod string) {
	r.emitLocked(events.TypeRoomHomeChanged, rm.code, events.RoomHomeChangedData{
		RoomCode:    rm.code,
		RoomKey:     r.opts.Obfuscate(rm.code),
		Kind:        rm.kind,
		PreviousPod: previousPod,
	})
}

// busRoomOpenedLocked publishes a room's birth. Both kinds: a static room's
// first homing is when it starts existing on the fleet, and a consumer that
// wants only dynamic rooms has `kind` to filter on.
func (r *Registry) busRoomOpenedLocked(rm *room) {
	r.emitLocked(events.TypeRoomOpened, rm.code, events.RoomOpenedData{
		RoomCode:    rm.code,
		RoomKey:     r.opts.Obfuscate(rm.code),
		Kind:        rm.kind,
		DisplayCode: rm.display,
		CreatedAt:   rm.createdAt.UTC().Truncate(time.Second).Format(time.RFC3339),
	})
}

// busAttachedLocked publishes one stream arriving in a room. An adoption
// re-announces the room's attachments on the new home, as it re-announces its
// participants: the consumer's picture of a re-homed room is rebuilt from
// events rather than assumed to have survived the move.
func (r *Registry) busAttachedLocked(rm *room, a *attachment) {
	r.emitLocked(events.TypeRoomAttached, rm.code, events.RoomAttachedData{
		RoomCode:     rm.code,
		RoomKey:      r.opts.Obfuscate(rm.code),
		BroadcastID:  a.id,
		BroadcastKey: r.opts.Obfuscate(a.id),
		Label:        a.label,
	})
}

// busRoomClosedLocked publishes a room's end, translating the wire reason byte
// into the vocabulary the schema declares.
//
// A room being re-homed is NOT closed: the old home releases it, the new one
// adopts it, and publishing room.closed there would tell every consumer a
// live room had ended (docs/51 D9).
func (r *Registry) busRoomClosedLocked(rm *room, reason uint8) {
	if rm.releasing {
		return
	}
	r.emitLocked(events.TypeRoomClosed, rm.code, events.RoomClosedData{
		RoomCode: rm.code,
		RoomKey:  r.opts.Obfuscate(rm.code),
		Reason:   closeReason(reason),
	})
}

func closeReason(reason uint8) string {
	switch reason {
	case wire.RoomEndReasonEmpty:
		return events.RoomClosedGrace
	case wire.RoomEndReasonCreator:
		return events.RoomClosedCreator
	default:
		return events.RoomClosedOperator
	}
}

// ReleaseHome is what a pod calls when it loses a room's home lease: the room
// is moving, not ending.
//
// It publishes nothing itself. It marks the room, and the ordinary leave path
// then reports each departure as `home_moved` when that session actually
// goes — one funnel, real timing, no events invented for people who may
// already be gone. The same mark suppresses the `room.closed` that the
// `EndRoom` behind it would otherwise produce: a re-homed room did not close.
//
// The honest limit: a pod losing a room because it is being DELETED may never
// run this at all, so these departures are best-effort. The signal a consumer
// should actually follow is `room.home_changed`, published by the pod that
// ADOPTS the room — the one participant in a re-home that is certain to be
// alive (docs/51 D9).
func (r *Registry) ReleaseHome(code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rm := r.rooms[code]
	if rm == nil {
		return
	}
	rm.releasing = true
	rm.leaveReason = events.ParticipantLeftHomeMoved
}
