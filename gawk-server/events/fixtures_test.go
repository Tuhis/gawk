package events

import "time"

// The Go side of every golden vector in testdata/vectors (docs/52 D7): one
// fully-populated event per type, written by hand to mirror the JSON file
// property by property. TestContract holds the two byte-identical in both
// directions, so a struct field renamed, reordered or retagged fails here
// before a consumer's parser notices.
//
// Fictional identifiers throughout, shared with the vectors: the broadcast
// ABC234 keyed 3f9a1c2b4d5e, the room R7K3MX keyed 9c1d2e3f4a5b.

const (
	fixtureBroadcastKey = "3f9a1c2b4d5e"
	fixtureBroadcastID  = "ABC234"
	fixtureRoomKey      = "9c1d2e3f4a5b"
	fixtureRoomCode     = "R7K3MX"
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
		TypeBroadcastKilled: admin(TypeBroadcastKilled, fixtureBroadcastKey, BroadcastKilledData{
			Actor: "juho@example.com", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "terms violation", Enforcement: EnforcementPending,
			Delivery: Delivery{
				Summary:   "a kill of broadcast 3f9a1c2b4d5e was recorded by juho@example.com — NOT enforced yet, the broadcast is still live",
				PortalURL: fixturePortalB,
			},
		}),
		TypeBanCreated: admin(TypeBanCreated, fixtureBroadcastKey, BanCreatedData{
			Actor: "juho@example.com", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "spam & abuse", Enforcement: EnforcementPending,
			Delivery: Delivery{
				Summary:   "a ban of broadcast 3f9a1c2b4d5e was recorded by juho@example.com — NOT enforced yet",
				PortalURL: fixturePortalB,
			},
		}),
		TypeBanExpired: admin(TypeBanExpired, fixtureBroadcastKey, BanExpiredData{
			Actor: "system", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "terms violation", Enforcement: EnforcementPending,
			Delivery: Delivery{
				Summary:   "the ban on broadcast 3f9a1c2b4d5e expired",
				PortalURL: fixturePortalB,
			},
		}),
		TypeBanRemoved: admin(TypeBanRemoved, fixtureBroadcastKey, BanRemovedData{
			Actor: "juho@example.com", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "terms violation", Enforcement: EnforcementPending,
			Delivery: Delivery{
				Summary:   "the ban on broadcast 3f9a1c2b4d5e was lifted by juho@example.com — STILL banned until the reconciler catches up",
				PortalURL: fixturePortalB,
			},
		}),
		TypeContentFlagRaised: admin(TypeContentFlagRaised, fixtureBroadcastKey, ContentFlagRaisedData{
			Actor: "juho@example.com", BroadcastKey: fixtureBroadcastKey, BroadcastID: fixtureBroadcastID,
			Reason: "reported by a viewer",
			Delivery: Delivery{
				Summary:   "a content flag was raised on broadcast 3f9a1c2b4d5e by juho@example.com",
				PortalURL: fixturePortalB,
			},
		}),
		TypeRoomCreated: admin(TypeRoomCreated, fixtureRoomKey, RoomCreatedData{
			Actor: "juho@example.com", RoomKey: fixtureRoomKey, RoomCode: "tuhisroom", Kind: RoomKindStatic,
			Delivery: Delivery{
				Summary:   "a static room was created by juho@example.com",
				PortalURL: fixturePortalR,
			},
		}),
		TypeRoomEnded: admin(TypeRoomEnded, fixtureRoomKey, RoomEndedData{
			Actor: "system", RoomKey: fixtureRoomKey, RoomCode: fixtureRoomCode, Kind: RoomKindDynamic, Reason: RoomClosedGrace,
			Delivery: Delivery{
				Summary:   "a dynamic room was ended by the relay",
				PortalURL: fixturePortalR,
			},
		}),
		TypeRoomSecretRotated: admin(TypeRoomSecretRotated, fixtureRoomKey, RoomSecretRotatedData{
			Actor: "juho@example.com", RoomKey: fixtureRoomKey, RoomCode: "tuhisroom", Kind: RoomKindStatic,
			Delivery: Delivery{
				Summary:   "a static room's attach secret was rotated by juho@example.com",
				PortalURL: fixturePortalR,
			},
		}),

		TypeBroadcastStarted: bus(TypeBroadcastStarted, fixtureBroadcastKey, BroadcastStartedData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Role: RoleOrigin,
			StartedAt: "2026-09-17T12:00:00Z",
		}),
		TypeBroadcastPublisherAway: bus(TypeBroadcastPublisherAway, fixtureBroadcastKey, BroadcastPublisherAwayData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey,
		}),
		TypeBroadcastPublisherBack: bus(TypeBroadcastPublisherBack, fixtureBroadcastKey, BroadcastPublisherBackData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey,
		}),
		TypeBroadcastEnded: bus(TypeBroadcastEnded, fixtureBroadcastKey, BroadcastEndedData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Reason: BroadcastEndedKilled,
		}),
		TypeBroadcastViewers: bus(TypeBroadcastViewers, fixtureBroadcastKey, BroadcastViewersData{
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Role: RoleEdge,
			ViewersLocal: 12, ViewersGlobal: 340,
		}),
		TypeRoomOpened: bus(TypeRoomOpened, fixtureRoomKey, RoomOpenedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, Kind: RoomKindDynamic, DisplayCode: fixtureRoomCode,
			CreatedAt: "2026-09-17T12:00:00Z",
		}),
		TypeRoomClosed: bus(TypeRoomClosed, fixtureRoomKey, RoomClosedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, Reason: RoomClosedCreator,
		}),
		TypeRoomAttached: bus(TypeRoomAttached, fixtureRoomKey, RoomAttachedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey,
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Label: "main stage & lobby",
		}),
		TypeRoomDetached: bus(TypeRoomDetached, fixtureRoomKey, RoomDetachedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey,
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Label: "main stage & lobby",
		}),
		TypeRoomAttachmentUpdated: bus(TypeRoomAttachmentUpdated, fixtureRoomKey, RoomAttachmentUpdatedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey,
			BroadcastID: fixtureBroadcastID, BroadcastKey: fixtureBroadcastKey, Live: true, Viewers: 7,
		}),
		TypeRoomParticipantJoined: bus(TypeRoomParticipantJoined, fixtureRoomKey, RoomParticipantJoinedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, ParticipantID: 3, Nickname: "tuhis",
			ClientKind: ClientKindWebBroadcaster, Streaming: true, Speaking: false, Rejoin: true,
		}),
		TypeRoomParticipantLeft: bus(TypeRoomParticipantLeft, fixtureRoomKey, RoomParticipantLeftData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, ParticipantID: 3, Nickname: "tuhis",
			ClientKind: ClientKindWebBroadcaster,
		}),
		TypeRoomParticipantUpdated: bus(TypeRoomParticipantUpdated, fixtureRoomKey, RoomParticipantUpdatedData{
			RoomCode: fixtureRoomCode, RoomKey: fixtureRoomKey, ParticipantID: 3, Nickname: "tuhis <3",
			ClientKind: ClientKindNative, Streaming: false, Speaking: true,
		}),

		TypeWebhookTest: New(TypeWebhookTest, "0f2c6a3e-8d41-4b9a-b7c2-5e1d3f4a6b8c", SourceAdmin, "", fixtureTime,
			WebhookTestData{Delivery: Delivery{
				Summary:   "test notification from the gawk-admin portal: this webhook is configured correctly",
				PortalURL: "https://admin.example.com/#/broadcasts",
			}}),
	}
}
