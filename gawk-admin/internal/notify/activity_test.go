package notify

// R49 RA4 (docs/50 D6–D8): room activity reaches exactly the webhooks that
// asked for it, as the bus event itself, and the ops pager that never asked
// hears nothing.

import (
	"encoding/json"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// busEvent is a golden vector turned into what the consumer hands the
// ingester, with its data adjusted and a fresh bus id.
func busEvent(t *testing.T, typ, id string, mutate func(data map[string]any)) eventbus.Event {
	t.Helper()
	raw, err := events.Vector(typ)
	if err != nil {
		t.Fatal(err)
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatal(err)
	}
	data, _ := full["data"].(map[string]any)
	if mutate != nil {
		mutate(data)
	}
	full["id"] = id
	body, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	at, _ := time.Parse(time.RFC3339, full["time"].(string))
	subject, _ := full["subject"].(string)
	return eventbus.Event{ID: id, Source: full["source"].(string), Type: typ, Subject: subject, Time: at, Data: data, Raw: body}
}

func TestActivityWebhooksAreOptInAndDeliverTheBusEvent(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	rec := newReceiver(t)

	cfg := config.Config{
		ExternalURL: "https://admin.example.com",
		StaticWebhooks: []config.StaticWebhook{
			// The ops pager: no filter, the way every webhook was configured
			// before R49.
			{Name: "ops-pager", URL: rec.url("/pager"), SecretEnv: "S", Secret: "c2VjcmV0"},
			// The bot, chart-defined: joins and detaches only.
			{Name: "bot", URL: rec.url("/bot"), SecretEnv: "B", Secret: "Ym90",
				Events: []string{store.EventRoomParticipantJoined, store.EventRoomDetached}},
		},
	}
	// A UI-created webhook that listed the adoption pair too.
	if _, err := st.CreateWebhook(ctx, store.Webhook{
		Name: "ui-bot", URL: rec.url("/ui-bot"), Secret: "dWk=", Enabled: true,
		CreatedAt: time.Now(), CreatedBy: "op",
		Events: []string{store.EventRoomParticipantJoined, store.FilterParticipantRejoined},
	}); err != nil {
		t.Fatal(err)
	}
	d := newDispatcher(t, st, cfg, nil)
	ing := &eventbus.StoreIngester{Store: st, ConfigWebhooks: d.ConfigWebhooks()}

	ingest := func(ev eventbus.Event) {
		t.Helper()
		if _, err := ing.Ingest(ctx, ev); err != nil {
			t.Fatalf("ingest %s: %v", ev.Type, err)
		}
	}
	// A fourth person joins; a stream detaches; a participant's rename (never
	// webhook-eligible); and a re-home's pair.
	ingest(busEvent(t, events.TypeRoomParticipantJoined, "relay-0:1", func(d map[string]any) {
		d["rejoin"] = false
		d["nickname"] = "fourth"
	}))
	ingest(busEvent(t, events.TypeRoomDetached, "relay-0:2", nil))
	ingest(busEvent(t, events.TypeRoomParticipantUpdated, "relay-0:3", nil))
	ingest(busEvent(t, events.TypeRoomParticipantLeft, "relay-0:4", func(d map[string]any) {
		d["reason"] = events.ParticipantLeftHomeMoved
	}))
	ingest(busEvent(t, events.TypeRoomParticipantJoined, "relay-1:1", func(d map[string]any) {
		d["rejoin"] = true
	}))
	// And a moderation event, which the pager has always had.
	mustRecord(t, d, killEvent("ZXQ7K2"))

	for {
		n, err := d.DispatchOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}

	types := func(path string) []string {
		var out []string
		for _, c := range rec.byPath()[path] {
			out = append(out, c.eventType(t))
		}
		return out
	}
	if got := types("/pager"); len(got) != 1 || got[0] != events.TypeBroadcastKilled {
		t.Fatalf("the unfiltered pager received %v, want only the kill", got)
	}
	if got := types("/bot"); len(got) != 2 || !contains(got, events.TypeRoomParticipantJoined) || !contains(got, events.TypeRoomDetached) {
		t.Fatalf("the bot received %v, want one join (not the rejoin) and one detach", got)
	}
	if got := types("/ui-bot"); len(got) != 3 {
		t.Fatalf("the UI bot received %v, want the join, the rejoin and the home_moved leave", got)
	}

	for _, c := range rec.byPath()["/bot"] {
		body := c.payloadOf(t)
		typ := c.eventType(t)
		data, _ := body["data"].(map[string]any)
		// The bus event itself: its id and source, not a portal-minted one.
		if !strings.HasPrefix(body["id"].(string), "relay-0:") || body["source"] != "/gawk/relay/relay-0" || c.id != body["id"] {
			t.Errorf("%s: envelope is not the bus event's: id=%v source=%v header=%s", typ, body["id"], body["source"], c.id)
		}
		if body["subject"] != "r7k3mx" {
			t.Errorf("%s: subject = %v, want the room code", typ, body["subject"])
		}
		if data["summary"] == "" || data["portalUrl"] != "https://admin.example.com/#/rooms?key=9c1d2e3f4a5b" {
			t.Errorf("%s: delivery-added properties = %v / %v", typ, data["summary"], data["portalUrl"])
		}
		switch typ {
		case events.TypeRoomParticipantJoined:
			if data["nickname"] != "fourth" {
				t.Errorf("join carries nickname %v", data["nickname"])
			}
		case events.TypeRoomDetached:
			if data["label"] == nil {
				t.Errorf("detach carries no label: %v", data)
			}
		}
		validateData(t, typ, data)
		assertNoIP(t, typ, c.body)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// assertNoIP is docs/52 D7's half of D8 that still stands: no delivery body
// carries an IP address anywhere.
func assertNoIP(t *testing.T, typ string, body []byte) {
	t.Helper()
	for _, m := range regexp.MustCompile(`[0-9a-fA-F:.]{7,}`).FindAllString(string(body), -1) {
		if _, err := netip.ParseAddr(m); err == nil {
			t.Errorf("%s delivery carries an IP address %q: %s", typ, m, body)
		}
	}
}

// An activity row whose stored event does not match its type is a producer
// bug: it fails terminally rather than being delivered as a guess.
func TestAnActivityRowWithAMismatchedEventIsNotDelivered(t *testing.T) {
	good := busEvent(t, events.TypeRoomDetached, "relay-0:9", nil)
	payload, _ := json.Marshal(map[string]any{"event": json.RawMessage(good.Raw), "summary": "x"})
	_, err := buildEvent(store.Event{ID: 1, Type: store.EventRoomAttached, Category: store.CategoryActivity, Payload: payload}, "")
	if err == nil {
		t.Fatal("a room.attached row carrying a room.detached event was rendered")
	}
	_, err = buildEvent(store.Event{ID: 2, Type: store.EventRoomAttached, Category: store.CategoryActivity, Payload: json.RawMessage(`{}`)}, "")
	if err == nil {
		t.Fatal("an activity row with no bus event was rendered")
	}
}
