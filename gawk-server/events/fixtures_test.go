package events

import "time"

// The Go side of every golden vector in testdata/vectors (docs/52 D7): one
// fully-populated event per type, written by hand to mirror the JSON file
// property by property. TestContract holds the two byte-identical in both
// directions, so a struct field renamed, reordered or retagged fails here
// before a consumer's parser notices.
//
// Fictional identifiers throughout, shared with the vectors: the broadcast
// ABC234 keyed 3f9a1c2b4d5e, the dynamic room r7k3mx keyed 9c1d2e3f4a5b, the
// static room tuhisroom under the same key. The `subject` of every fixture is
// the CLEARTEXT one (docs/52 D9); the key appears in `data` beside it.
//
// The code and the display code differ on purpose, because that is the shape
// a producer actually emits: `roomCode` is normalised (lower case, the CR
// name and the registry key), `displayCode` is what a person sees and types —
// a dynamic code upper-cased, a static slug with its configured casing.

const (
	fixtureBroadcastKey = "3f9a1c2b4d5e"
	fixtureBroadcastID  = "ABC234"
	fixtureRoomKey      = "9c1d2e3f4a5b"
	fixtureRoomCode     = "r7k3mx"
	fixtureRoomDisplay  = "R7K3MX"
	fixtureStaticCode   = "tuhisroom"
	fixtureStaticDisp   = "TuhisRoom"
	fixtureBusID        = "relay-0:4711"
	fixtureAdminID      = "6c1f1a40-2f7e-5a3b-9a1d-0b7d2c1e4f55"
	fixturePortalB      = "https://admin.example.com/#/broadcasts?key=" + fixtureBroadcastKey
	fixturePortalR      = "https://admin.example.com/#/rooms?key=" + fixtureRoomKey
)

var fixtureTime = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func fixtureRelaySource() string { return SourceRelay("relay-0") }

// fixtures returns the Go form of every vector, keyed by type.
func fixtures() map[string]Event {
	admin := func(typ, subject string, data any) Event {
		return New(typ, fixtureAdminID, SourceAdmin, subject, fixtureTime, data)
	}
	bus := func(typ, subject string, data any) Event {
		return New(typ, fixtureBusID, fixtureRelaySource(), subject, fixtureTime, data)
	}
	return map[string]Event{
		TypeBroadcastKilled: admin(TypeBroadcastKilled, fixtureBroadcastID, BroadcastKilledData{
			Actor: "juho@example.com", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "terms violation", Enforcement: EnforcementPending,
			Delivery: Delivery{
				Summary:   "a kill of broadcast ABC234 was recorded by juho@example.com — NOT enforced yet, the broadcast is still live",
				PortalURL: fixturePortalB,
			},
		}),
		TypeBanCreated: admin(TypeBanCreated, fixtureBroadcastID, BanCreatedData{
			Actor: "juho@example.com", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "spam & abuse", Enforcement: EnforcementPending,
			Delivery: Delivery{
				Summary:   "a ban on broadcast ABC234 was recorded by juho@example.com — NOT enforced yet",
				PortalURL: fixturePortalB,
			},
		}),
		TypeBanExpired: admin(TypeBanExpired, fixtureBroadcastID, BanExpiredData{
			Actor: "system", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "terms violation", Enforcement: EnforcementPending,
			Delivery: Delivery{
				Summary:   "a ban on broadcast ABC234 expired in the record — the target is STILL banned",
				PortalURL: fixturePortalB,
			},
		}),
		TypeBanRemoved: admin(TypeBanRemoved, fixtureBroadcastID, BanRemovedData{
			Actor: "juho@example.com", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "terms violation", Enforcement: EnforcementPending,
			Delivery: Delivery{
				Summary:   "a ban on broadcast ABC234 was lifted in the record by juho@example.com — the target is STILL banned",
				PortalURL: fixturePortalB,
			},
		}),
		TypeContentFlagRaised: admin(TypeContentFlagRaised, fixtureBroadcastID, ContentFlagRaisedData{
			Actor: "juho@example.com", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "reported by a viewer",
			Delivery: Delivery{
				Summary:   "a content flag was raised on broadcast ABC234 by juho@example.com",
				PortalURL: fixturePortalB,
			},
		}),
		TypeRoomCreated: admin(TypeRoomCreated, fixtureStaticCode, RoomCreatedData{
			Actor: "juho@example.com", RoomKey: fixtureRoomKey, RoomCode: fixtureStaticCode, DisplayCode: fixtureStaticDisp, Kind: RoomKindStatic,
			Delivery: Delivery{
				Summary:   "static room TuhisRoom was created by juho@example.com",
				PortalURL: fixturePortalR,
			},
		}),
		TypeRoomEnded: admin(TypeRoomEnded, fixtureRoomCode, RoomEndedData{
			Actor: "system", RoomKey: fixtureRoomKey, RoomCode: fixtureRoomCode, DisplayCode: fixtureRoomDisplay, Kind: RoomKindDynamic, Reason: RoomClosedGrace,
			Delivery: Delivery{
				Summary:   "dynamic room R7K3MX ended",
				PortalURL: fixturePortalR,
			},
		}),
		TypeRoomSecretRotated: admin(TypeRoomSecretRotated, fixtureStaticCode, RoomSecretRotatedData{
			Actor: "juho@example.com", RoomKey: fixtureRoomKey, RoomCode: fixtureStaticCode, DisplayCode: fixtureStaticDisp, Kind: RoomKindStatic,
			Delivery: Delivery{
				Summary:   "the attach secret of static room TuhisRoom was rotated by juho@example.com",
				PortalURL: fixturePortalR,
			},
		}),

		TypeBroadcastStarted: bus(TypeBroadcastStarted, fixtureBroadcastID, BroadcastStartedData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Role: RoleOrigin,
			StartedAt: "2026-09-17T12:00:00Z",
		}),
		TypeBroadcastPublisherAway: bus(TypeBroadcastPublisherAway, fixtureBroadcastID, BroadcastPublisherAwayData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey,
		}),
		TypeBroadcastPublisherBack: bus(TypeBroadcastPublisherBack, fixtureBroadcastID, BroadcastPublisherBackData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey,
		}),
		TypeBroadcastEnded: bus(TypeBroadcastEnded, fixtureBroadcastID, BroadcastEndedData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Reason: BroadcastEndedKilled,
		}),
		TypeBroadcastViewers: bus(TypeBroadcastViewers, fixtureBroadcastID, BroadcastViewersData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Role: RoleEdge,
			ViewersLocal: 12, ViewersGlobal: 340,
		}),
		TypeRoomOpened: bus(TypeRoomOpened, fixtureRoomCode, RoomOpenedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, DisplayCode: fixtureRoomDisplay, Kind: RoomKindDynamic,
			CreatedAt: "2026-09-17T12:00:00Z",
		}),
		TypeRoomClosed: bus(TypeRoomClosed, fixtureRoomCode, RoomClosedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, DisplayCode: fixtureRoomDisplay, Kind: RoomKindDynamic,
			Reason: RoomClosedCreator,
		}),
		TypeRoomAttached: bus(TypeRoomAttached, fixtureRoomCode, RoomAttachedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, DisplayCode: fixtureRoomDisplay,
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Label: "main stage & lobby",
		}),
		TypeRoomDetached: bus(TypeRoomDetached, fixtureRoomCode, RoomDetachedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, DisplayCode: fixtureRoomDisplay,
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Label: "main stage & lobby",
		}),
		TypeRoomAttachmentUpdated: bus(TypeRoomAttachmentUpdated, fixtureRoomCode, RoomAttachmentUpdatedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, DisplayCode: fixtureRoomDisplay,
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Live: true, Viewers: 7,
		}),
		TypeRoomHomeChanged: bus(TypeRoomHomeChanged, fixtureRoomCode, RoomHomeChangedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, DisplayCode: fixtureRoomDisplay, Kind: RoomKindDynamic,
			PreviousPod: "relay-1",
		}),
		TypeRoomParticipantJoined: bus(TypeRoomParticipantJoined, fixtureRoomCode, RoomParticipantJoinedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, DisplayCode: fixtureRoomDisplay, ParticipantID: 3, Nickname: "tuhis",
			ClientKind: ClientKindWebBroadcaster, Streaming: true, Speaking: false, Rejoin: true,
		}),
		TypeRoomParticipantLeft: bus(TypeRoomParticipantLeft, fixtureRoomCode, RoomParticipantLeftData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, DisplayCode: fixtureRoomDisplay, ParticipantID: 3, Nickname: "tuhis",
			ClientKind: ClientKindWebBroadcaster, Reason: ParticipantLeftHomeMoved,
		}),
		TypeRoomParticipantUpdated: bus(TypeRoomParticipantUpdated, fixtureRoomCode, RoomParticipantUpdatedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, DisplayCode: fixtureRoomDisplay, ParticipantID: 3, Nickname: "tuhis <3",
			ClientKind: ClientKindNative, Streaming: false, Speaking: true,
		}),

		TypeWebhookTest: New(TypeWebhookTest, "0f2c6a3e-8d41-4b9a-b7c2-5e1d3f4a6b8c", SourceAdmin, "", fixtureTime,
			WebhookTestData{Delivery: Delivery{
				Summary:   "test notification from the gawk-admin portal: this webhook is configured correctly",
				PortalURL: "https://admin.example.com/#/broadcasts",
			}}),
	}
}
