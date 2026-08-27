package api

import (
	"net/netip"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// The /api/v1 wire shapes (docs/42 §4.7).
//
// They are declared HERE rather than by tagging the store's row structs, on
// purpose: the response contract belongs to the API, not to the database, and
// the portal SPA is written against these names. Every LIST route answers a
// KEYED object ({"bans":[…]}, {"broadcasts":[…]}, {"events":[…],"nextAfterId":…})
// so a cursor or a total can be added later without breaking a client; every
// single-object route answers the object itself, except kill, which §4.7 pins
// as `201 {ban}`.

type banTargetJSON struct {
	Type  moderation.TargetType `json:"type"`
	Value string                `json:"value"`
}

// enforcementJSON reports whether the Kubernetes enforcement object currently
// MATCHES this record. It rides only on a mutation's own response, and only
// when the two disagree — a 202 (see the three handlers). List and read routes
// never carry it: a stored row has no in-flight projection to report on, and
// the reconciler, not this field, is what makes them agree again.
//
// `inSync` rather than `enforced` because the field has to read correctly in
// BOTH directions. A pending create means the ban is recorded and NOT yet
// enforced; a pending unban means the record is lifted and the target is STILL
// enforced. "enforced: false" would be a lie about the second one.
type enforcementJSON struct {
	InSync bool   `json:"inSync"`
	Detail string `json:"detail,omitempty"`
}

// The two sentences a 202 explains itself with. They are constants because the
// SPA renders them verbatim — this is operator-facing copy, not log text.
const (
	// DetailBanPending is a ban row that exists with no CR behind it yet.
	DetailBanPending = "The ban is recorded but NOT enforced yet — its Kubernetes enforcement object could not be written; the reconciler retries within a minute, so do not re-submit."
	// DetailUnbanPending is the mirror: the row says removed, the CR does not.
	DetailUnbanPending = "The ban is lifted in the record but the target is STILL banned — its Kubernetes enforcement object could not be deleted; the reconciler retries within a minute, so do not re-submit."
)

type banJSON struct {
	ID     string         `json:"id"`
	Target banTargetJSON  `json:"target"`
	State  store.BanState `json:"state"`
	// Reason is operator-private context: portal and Postgres, never a relay
	// log above Debug (docs/42 §5).
	Reason    string `json:"reason"`
	CreatedAt string `json:"createdAt"`
	CreatedBy string `json:"createdBy"`
	// ExpiresAt is null for a permanent ban — explicitly null rather than
	// omitted, because "permanent" is a fact the UI renders.
	ExpiresAt         *string `json:"expiresAt"`
	RemovedAt         *string `json:"removedAt,omitempty"`
	RemovedBy         string  `json:"removedBy,omitempty"`
	SourceBroadcastID string  `json:"sourceBroadcastId,omitempty"`
	CRName            string  `json:"crName,omitempty"`
	// Enforcement is set ONLY by renderPendingBan, so a 201/204 and every
	// list/read response stay byte-identical to what they have always been.
	// Its presence is the whole signal: absent means there is nothing to say.
	Enforcement *enforcementJSON `json:"enforcement,omitempty"`
}

func renderBan(b store.Ban) banJSON {
	out := banJSON{
		ID:                b.ID.String(),
		Target:            banTargetJSON{Type: b.Target.Type, Value: b.Target.Value},
		State:             b.State,
		Reason:            b.Reason,
		CreatedAt:         b.CreatedAt.UTC().Format(time.RFC3339),
		CreatedBy:         b.CreatedBy,
		RemovedBy:         b.RemovedBy,
		SourceBroadcastID: b.SourceBroadcastID,
		CRName:            b.CRName,
	}
	if b.ExpiresAt != nil {
		s := b.ExpiresAt.UTC().Format(time.RFC3339)
		out.ExpiresAt = &s
	}
	if b.RemovedAt != nil {
		s := b.RemovedAt.UTC().Format(time.RFC3339)
		out.RemovedAt = &s
	}
	return out
}

// renderPendingBan is the 202 body: the committed row, plus one sentence
// saying which way the Kubernetes side is out of step with it.
//
// The ban itself is here because a client that just created something needs
// its ID regardless of whether enforcement caught up — and because a 202 that
// answered with only a message would leave the operator no handle on the row
// they must NOT re-submit.
func renderPendingBan(b store.Ban, detail string) banJSON {
	out := renderBan(b)
	out.Enforcement = &enforcementJSON{InSync: false, Detail: detail}
	return out
}

func renderBans(bs []store.Ban) []banJSON {
	out := make([]banJSON, 0, len(bs))
	for _, b := range bs {
		out = append(out, renderBan(b))
	}
	return out
}

type podPlacementJSON struct {
	Pod          string `json:"pod"`
	Role         string `json:"role"`
	ViewersLocal int    `json:"viewersLocal"`
}

type linksJSON struct {
	// Watch is <appBaseUrl>/#/view/<id>; Telemetry is
	// <telemetryBaseUrl>/#/broadcast/<key>. Each is OMITTED when its base URL
	// is unconfigured — a dead link is worse than no link (§4.7).
	Watch     string `json:"watch,omitempty"`
	Telemetry string `json:"telemetry,omitempty"`
}

// banStateJSON says whether an active ban already covers a broadcast. `ban` is
// the covering ban when there is one — the ID ban in preference to an IP ban,
// because that is the one a re-kill would collide with.
type banStateJSON struct {
	Banned bool     `json:"banned"`
	Ban    *banJSON `json:"ban,omitempty"`
}

type broadcastJSON struct {
	// ID is raw and joinable: portal-only (D8).
	ID                string             `json:"id"`
	Key               string             `json:"key"`
	PublisherActive   bool               `json:"publisherActive"`
	PublisherRemoteIP string             `json:"publisherRemoteIp,omitempty"`
	StartedAt         string             `json:"startedAt,omitempty"`
	ViewersGlobal     int                `json:"viewersGlobal"`
	Pods              []podPlacementJSON `json:"pods"`
	Links             *linksJSON         `json:"links,omitempty"`
	// BanState is null when it could not be determined — Postgres unreachable
	// — so the UI shows "unknown" rather than a confident "not banned".
	BanState *banStateJSON `json:"banState"`
}

type deliveryJSON struct {
	WebhookName   string              `json:"webhookName"`
	State         store.DeliveryState `json:"state"`
	Attempts      int                 `json:"attempts"`
	LastError     string              `json:"lastError,omitempty"`
	DeliveredAt   *string             `json:"deliveredAt,omitempty"`
	NextAttemptAt *string             `json:"nextAttemptAt,omitempty"`
}

func renderDeliveries(ds []store.Delivery) []deliveryJSON {
	out := make([]deliveryJSON, 0, len(ds))
	for _, d := range ds {
		row := deliveryJSON{
			WebhookName: d.WebhookName,
			State:       d.State,
			Attempts:    d.Attempts,
			LastError:   d.LastError,
		}
		if d.DeliveredAt != nil {
			s := d.DeliveredAt.UTC().Format(time.RFC3339)
			row.DeliveredAt = &s
		}
		if d.NextAttemptAt != nil {
			s := d.NextAttemptAt.UTC().Format(time.RFC3339)
			row.NextAttemptAt = &s
		}
		out = append(out, row)
	}
	return out
}

type eventJSON struct {
	ID         int64  `json:"id"`
	Type       string `json:"type"`
	OccurredAt string `json:"occurredAt"`
	Actor      string `json:"actor"`
	// BroadcastKey is the HMAC'd key; BroadcastID is raw and portal-only (D8).
	BroadcastKey string `json:"broadcastKey,omitempty"`
	BroadcastID  string `json:"broadcastId,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Summary      string `json:"summary,omitempty"`
	// Deliveries is what makes a failed webhook delivery VISIBLE (§4.10) —
	// R40's "a flag must reach a human" posture inherits this pipe.
	Deliveries []deliveryJSON `json:"deliveries"`
}

func renderEvent(e store.Event, deliveries []store.Delivery) eventJSON {
	return eventJSON{
		ID:           e.ID,
		Type:         e.Type,
		OccurredAt:   e.OccurredAt.UTC().Format(time.RFC3339),
		Actor:        e.Actor,
		BroadcastKey: e.BroadcastKey,
		BroadcastID:  e.BroadcastID,
		Reason:       e.PayloadString(store.PayloadReason),
		Summary:      e.PayloadString(store.PayloadSummary),
		Deliveries:   renderDeliveries(deliveries),
	}
}

type relayJSON struct {
	Pod       string         `json:"pod"`
	Reachable bool           `json:"reachable"`
	Version   string         `json:"version,omitempty"`
	Config    map[string]any `json:"config,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// Webhook sources (§4.7). "config" rows are chart-defined: visible, testable,
// never editable here.
const (
	SourceConfig = "config"
	SourceUI     = "ui"
)

// webhookJSON has NO secret field at all — for either source. That is the
// strongest form of "secrets are never returned": there is nowhere to put one.
type webhookJSON struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
}

// linksFor builds the per-broadcast deep links, omitting either when its base
// URL is unconfigured.
func linksFor(appBase, telemetryBase, id, key string) *linksJSON {
	l := linksJSON{}
	if appBase != "" && id != "" {
		l.Watch = appBase + "/#/view/" + id
	}
	if telemetryBase != "" && key != "" {
		l.Telemetry = telemetryBase + "/#/broadcast/" + key
	}
	if l.Watch == "" && l.Telemetry == "" {
		return nil
	}
	return &l
}

// coveringBan finds the active ban that covers a broadcast: its ID ban first,
// then any IP ban whose prefix contains the publisher's address.
//
// The IP arm is a real prefix test, not a string compare: a /24 ban covers a
// publisher the operator never typed, and the portal must say so or the
// operator will ban the same person twice and wonder why nothing changed.
func coveringBan(active []store.Ban, now time.Time, normID, publisherIP string) *store.Ban {
	var ipMatch *store.Ban
	addr, addrOK := parsePeerAddr(publisherIP)
	for i := range active {
		b := active[i]
		if !b.Active(now) {
			continue
		}
		switch b.Target.Type {
		case moderation.TargetBroadcastID:
			if normID != "" && b.Target.Value == normID {
				return &active[i]
			}
		case moderation.TargetIP:
			if !addrOK || ipMatch != nil {
				continue
			}
			p, err := moderation.ParsePrefix(b.Target.Value)
			if err == nil && p.Contains(addr) {
				ipMatch = &active[i]
			}
		}
	}
	return ipMatch
}

// parsePeerAddr turns whatever the relay reported into a comparable address.
// It tolerates a host:port form because addresses reach the relay through
// RemoteAddr, and canonicalizes v4-mapped-v6 so ::ffff:203.0.113.7 is matched
// by a 203.0.113.7/32 ban — the same rule moderation.CanonicalAddr applies on
// the publish path.
func parsePeerAddr(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		return moderation.CanonicalAddr(addr), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return moderation.CanonicalAddr(ap.Addr()), true
	}
	return netip.Addr{}, false
}

// renderBroadcast merges a relayscan aggregate with its ban state.
func renderBroadcast(agg relayscan.Aggregate, cfg linkConfig, banState *banStateJSON) broadcastJSON {
	pods := make([]podPlacementJSON, 0, len(agg.Pods))
	for _, p := range agg.Pods {
		pods = append(pods, podPlacementJSON{Pod: p.Pod, Role: p.Role, ViewersLocal: p.ViewersLocal})
	}
	startedAt := ""
	if !agg.StartedAt.IsZero() {
		startedAt = agg.StartedAt.UTC().Format(time.RFC3339)
	}
	return broadcastJSON{
		ID:                agg.ID,
		Key:               agg.Key,
		PublisherActive:   agg.PublisherActive,
		PublisherRemoteIP: agg.PublisherRemoteIP,
		StartedAt:         startedAt,
		ViewersGlobal:     int(agg.ViewersGlobal),
		Pods:              pods,
		Links:             linksFor(cfg.app, cfg.telemetry, agg.ID, agg.Key),
		BanState:          banState,
	}
}

type linkConfig struct{ app, telemetry string }
