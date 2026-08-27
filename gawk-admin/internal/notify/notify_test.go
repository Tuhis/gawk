package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
)

// killEvent is a realistic broadcast.killed event: an HMAC'd key on the row, a
// RAW joinable ID beside it, and portal-only context in the payload.
func killEvent(rawID string) store.Event {
	return store.Event{
		Type:         store.EventBroadcastKilled,
		OccurredAt:   time.Date(2026, 8, 20, 15, 4, 5, 0, time.UTC),
		Actor:        "juho@example.com",
		BroadcastKey: "3f9a1c2b4d5e",
		BroadcastID:  rawID,
		Payload: json.RawMessage(`{"reason":"terms violation",` +
			`"summary":"broadcast 3f9a1c2b4d5e was terminated by juho@example.com",` +
			`"cooldownSeconds":600,"banId":"11111111-2222-3333-4444-555555555555"}`),
	}
}

func mustCreateWebhook(t *testing.T, st *store.Store, name, url, secret string, isEnabled bool) {
	t.Helper()
	if _, err := st.CreateWebhook(context.Background(), store.Webhook{
		Name: name, URL: url, Secret: secret, Enabled: isEnabled,
		CreatedAt: time.Now(), CreatedBy: "juho@example.com",
	}); err != nil {
		t.Fatalf("create webhook %s: %v", name, err)
	}
}

// TestFanOutAcrossBothSourcesEachWithItsOwnSecret is the core AP7 criterion:
// one event reaches every ENABLED webhook from BOTH sources, each signed with
// its own key, and the parked ones get nothing.
func TestFanOutAcrossBothSourcesEachWithItsOwnSecret(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	rec := newReceiver(t)

	const chartSecret, uiSecret = "chart-defined-secret", "ui-created-secret"
	cfg := config.Config{
		ExternalURL: "https://admin.example.com",
		StaticWebhooks: []config.StaticWebhook{
			{Name: "chart-pager", URL: rec.url("/chart-pager"), SecretEnv: "PAGER_SECRET", Secret: chartSecret},
			{Name: "chart-parked", URL: rec.url("/chart-parked"), SecretEnv: "PARKED_SECRET",
				Secret: "parked-secret", Enabled: enabled(false)},
		},
	}
	mustCreateWebhook(t, st, "ui-slack", rec.url("/ui-slack"), uiSecret, true)
	mustCreateWebhook(t, st, "ui-parked", rec.url("/ui-parked"), "ui-parked-secret", false)

	const rawID = "ZXQ7K2"
	d := newDispatcher(t, st, cfg, nil)
	ev := mustRecord(t, d, killEvent(rawID))

	rows := deliveriesFor(t, st, ev.ID)
	if len(rows) != 2 {
		t.Fatalf("queued %d deliveries, want 2 (the enabled webhook from each source); got %v", len(rows), rows)
	}
	for _, name := range []string{"chart-pager", "ui-slack"} {
		if _, ok := rows[name]; !ok {
			t.Fatalf("no delivery queued for %s; got %v", name, rows)
		}
	}

	n, err := d.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("claimed %d deliveries, want 2", n)
	}

	byPath := rec.byPath()
	if len(byPath) != 2 {
		t.Fatalf("receiver saw %d distinct paths, want 2 (a disabled webhook must receive nothing): %v", len(byPath), byPath)
	}
	for _, parked := range []string{"/chart-parked", "/ui-parked"} {
		if got := byPath[parked]; len(got) != 0 {
			t.Fatalf("disabled webhook at %s received %d deliveries", parked, len(got))
		}
	}

	secrets := map[string]string{"/chart-pager": chartSecret, "/ui-slack": uiSecret}
	names := map[string]string{"/chart-pager": "chart-pager", "/ui-slack": "ui-slack"}
	for path, secret := range secrets {
		got := byPath[path]
		if len(got) != 1 {
			t.Fatalf("%s received %d deliveries, want exactly 1", path, len(got))
		}
		c := got[0]
		if c.contentType != ContentType {
			t.Errorf("%s: Content-Type = %q, want %q", path, c.contentType, ContentType)
		}
		if c.event != store.EventBroadcastKilled {
			t.Errorf("%s: %s = %q, want %q", path, HeaderEvent, c.event, store.EventBroadcastKilled)
		}
		if want := DeliveryID(rows[names[path]].ID); c.delivery != want {
			t.Errorf("%s: %s = %q, want %q (derived from the delivery row so retries repeat it)",
				path, HeaderDelivery, c.delivery, want)
		}
		if !verifyIndependently(t, secret, c.timestamp, c.signature, c.body) {
			t.Errorf("%s: an independent HMAC over timestamp+\".\"+body did not verify", path)
		}
		// Each webhook signs with ITS OWN secret (D9): the other's must fail.
		for otherPath, otherSecret := range secrets {
			if otherPath == path {
				continue
			}
			if verifyIndependently(t, otherSecret, c.timestamp, c.signature, c.body) {
				t.Errorf("%s verified under %s's secret: the webhooks are not signed independently", path, otherPath)
			}
		}
		// D8, end to end on the wire.
		if strings.Contains(string(c.body), rawID) {
			t.Errorf("%s: the raw broadcast ID reached the wire: %s", path, c.body)
		}
		payload := c.payloadOf(t)
		if payload["broadcastKey"] != "3f9a1c2b4d5e" {
			t.Errorf("%s: broadcastKey = %v, want the HMAC'd key", path, payload["broadcastKey"])
		}
		if s, _ := payload["summary"].(string); strings.TrimSpace(s) == "" {
			t.Errorf("%s: no summary in the payload", path)
		}
	}

	for name, row := range deliveriesFor(t, st, ev.ID) {
		if row.State != store.DeliveryDelivered {
			t.Errorf("%s: state = %q, want delivered", name, row.State)
		}
		if row.Attempts != 1 {
			t.Errorf("%s: attempts = %d, want 1", name, row.Attempts)
		}
		if row.DeliveredAt == nil {
			t.Errorf("%s: delivered_at is null on a delivered row", name)
		}
		if row.NextAttemptAt != nil {
			t.Errorf("%s: next_attempt_at is still set on a delivered row", name)
		}
		if row.LastError != "" {
			t.Errorf("%s: last_error = %q on a delivered row", name, row.LastError)
		}
	}
}

// TestRecordQueuesExactlyOnce: one Record call is one transaction — the event
// and exactly one delivery row per enabled webhook, delivered exactly once.
// (The retried-enqueue-after-a-crash case this test used to pin no longer
// exists as a code path: the crash window between the event append and the
// queue write is what Record's single transaction closed.)
func TestRecordQueuesExactlyOnce(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	rec := newReceiver(t)
	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "pager", URL: rec.url("/pager"), SecretEnv: "S", Secret: "secret"},
	}}
	d := newDispatcher(t, st, cfg, nil)

	ev := mustRecord(t, d, killEvent("ZXQ7K2"))
	if rows := deliveriesFor(t, st, ev.ID); len(rows) != 1 {
		t.Fatalf("one Record produced %d delivery rows, want 1", len(rows))
	}
	if _, err := d.DispatchOnce(ctx); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("receiver saw %d deliveries, want 1", got)
	}
}

// TestZeroWebhooksStillRecordsTheEvent: "zero configured webhooks is fine"
// (§4.10) — the event is in Postgres and the portal feed either way.
func TestZeroWebhooksStillRecordsTheEvent(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	d := newDispatcher(t, st, config.Config{ExternalURL: "https://admin.example.com"}, nil)

	ev := mustRecord(t, d, killEvent("ZXQ7K2"))
	if rows := deliveriesFor(t, st, ev.ID); len(rows) != 0 {
		t.Fatalf("queued %d deliveries with no webhooks configured", len(rows))
	}
	n, err := d.DispatchOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("DispatchOnce = %d, %v; want 0, nil", n, err)
	}

	events, err := st.ListEvents(ctx, 0, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].ID != ev.ID {
		t.Fatalf("the event did not land in the feed: %+v", events)
	}
}

// TestRetryScheduleAndTerminalFailure walks the §4.10 ladder on a fake clock:
// +5 s, +30 s, +2 m, +10 m, then failed — five attempts in total.
//
// It asserts the schedule is HONOURED, not merely recorded: at each rung the
// delivery is claimed only once the clock has reached its due time.
func TestRetryScheduleAndTerminalFailure(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	rec := newReceiver(t)
	rec.setStatus(http.StatusServiceUnavailable, "receiver is on fire\n\nplease hold")

	clk := newClock(time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC))
	// The store shares the clock so a row's first due time is the fake now,
	// not wall-clock now.
	st.Now = clk.Now

	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "pager", URL: rec.url("/pager"), SecretEnv: "S", Secret: "secret"},
	}}
	d := newDispatcher(t, st, cfg, func(o *Options) { o.Now = clk.Now })

	ev := mustRecord(t, d, killEvent("ZXQ7K2"))

	wantDelays := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	for attempt, delay := range wantDelays {
		n, err := d.DispatchOnce(ctx)
		if err != nil {
			t.Fatalf("attempt %d: DispatchOnce: %v", attempt+1, err)
		}
		if n != 1 {
			t.Fatalf("attempt %d: claimed %d deliveries, want 1", attempt+1, n)
		}
		row := deliveriesFor(t, st, ev.ID)["pager"]
		if row.State != store.DeliveryPending {
			t.Fatalf("attempt %d: state = %q, want pending (the budget is not spent yet)", attempt+1, row.State)
		}
		if row.Attempts != attempt+1 {
			t.Fatalf("attempt %d: attempts = %d", attempt+1, row.Attempts)
		}
		if row.NextAttemptAt == nil {
			t.Fatalf("attempt %d: no next_attempt_at on a pending row", attempt+1)
		}
		if want := clk.Now().Add(delay); !row.NextAttemptAt.Equal(want) {
			t.Fatalf("attempt %d: next attempt at %s, want %s (+%s)", attempt+1, row.NextAttemptAt, want, delay)
		}
		if !strings.Contains(row.LastError, "503") {
			t.Errorf("attempt %d: last_error = %q, want the receiver's status in it", attempt+1, row.LastError)
		}
		if strings.ContainsAny(row.LastError, "\n\r") {
			t.Errorf("attempt %d: last_error carries raw newlines from the receiver: %q", attempt+1, row.LastError)
		}

		// One second short of due: nothing may be claimed.
		clk.Advance(delay - time.Second)
		if n, err := d.DispatchOnce(ctx); err != nil || n != 0 {
			t.Fatalf("attempt %d: a delivery was claimed %s before its due time (n=%d, err=%v)", attempt+1, time.Second, n, err)
		}
		clk.Advance(time.Second)
	}

	// The fifth attempt spends the budget.
	if n, err := d.DispatchOnce(ctx); err != nil || n != 1 {
		t.Fatalf("final attempt: DispatchOnce = %d, %v", n, err)
	}
	row := deliveriesFor(t, st, ev.ID)["pager"]
	if row.State != store.DeliveryFailed {
		t.Fatalf("state = %q after %d attempts, want failed", row.State, MaxAttempts)
	}
	if row.Attempts != MaxAttempts {
		t.Fatalf("attempts = %d, want %d", row.Attempts, MaxAttempts)
	}
	if row.NextAttemptAt != nil {
		t.Fatalf("a failed delivery is still scheduled for %s", row.NextAttemptAt)
	}
	if row.LastError == "" {
		t.Fatal("a failed delivery carries no last_error: the operator cannot see WHY the page never arrived")
	}
	if got := rec.count(); got != MaxAttempts {
		t.Fatalf("receiver saw %d requests, want %d", got, MaxAttempts)
	}

	// Terminal means terminal: no further clock advance revives it.
	clk.Advance(24 * time.Hour)
	if n, err := d.DispatchOnce(ctx); err != nil || n != 0 {
		t.Fatalf("a failed delivery was claimed again (n=%d, err=%v)", n, err)
	}
}

// TestEventsViewRendersDeliveryState proves the rows this package writes make
// the portal's events view truthful — "a failed delivery must be SEEN"
// (§4.10). The API side already exists; this asserts the two halves agree.
func TestEventsViewRendersDeliveryState(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	good := newReceiver(t)
	bad := newReceiver(t)
	bad.setStatus(http.StatusInternalServerError, "nope")

	cfg := config.Config{
		ExternalURL:  "https://admin.example.com",
		OperatorRole: "operator",
		StaticWebhooks: []config.StaticWebhook{
			{Name: "chart-pager", URL: good.url("/pager"), SecretEnv: "S", Secret: "chart-secret"},
		},
	}
	mustCreateWebhook(t, st, "ui-broken", bad.url("/broken"), "ui-secret", true)

	d := newDispatcher(t, st, cfg, nil)
	_ = mustRecord(t, d, killEvent("ZXQ7K2"))
	if _, err := d.DispatchOnce(ctx); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}

	a, err := api.New(api.Options{Store: st, Config: cfg})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(a.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/events")
	if err != nil {
		t.Fatalf("GET /api/v1/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/events = %d", resp.StatusCode)
	}
	var body struct {
		Events []struct {
			ID         int64  `json:"id"`
			Type       string `json:"type"`
			Summary    string `json:"summary"`
			Deliveries []struct {
				WebhookName   string `json:"webhookName"`
				State         string `json:"state"`
				Attempts      int    `json:"attempts"`
				LastError     string `json:"lastError"`
				DeliveredAt   string `json:"deliveredAt"`
				NextAttemptAt string `json:"nextAttemptAt"`
			} `json:"deliveries"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(body.Events) != 1 {
		t.Fatalf("events view returned %d events, want 1", len(body.Events))
	}
	rendered := map[string]struct {
		state, lastErr, deliveredAt string
		attempts                    int
	}{}
	for _, dlv := range body.Events[0].Deliveries {
		rendered[dlv.WebhookName] = struct {
			state, lastErr, deliveredAt string
			attempts                    int
		}{dlv.State, dlv.LastError, dlv.DeliveredAt, dlv.Attempts}
	}
	if len(rendered) != 2 {
		t.Fatalf("events view shows %d deliveries, want one per enabled webhook: %v", len(rendered), rendered)
	}
	if got := rendered["chart-pager"]; got.state != string(store.DeliveryDelivered) || got.attempts != 1 || got.deliveredAt == "" {
		t.Errorf("chart-pager rendered as %+v, want a delivered row with a timestamp", got)
	}
	if got := rendered["ui-broken"]; got.state != string(store.DeliveryPending) || got.attempts != 1 || got.lastErr == "" {
		t.Errorf("ui-broken rendered as %+v, want a pending row carrying its last error", got)
	}
}

// TestDeletedOrDisabledWebhookEndsDeliveryTerminally: a queued page whose
// webhook has since gone away must not sit pending forever, and must say why.
func TestDeletedOrDisabledWebhookEndsDeliveryTerminally(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	rec := newReceiver(t)

	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "chart-pager", URL: rec.url("/chart-pager"), SecretEnv: "S", Secret: "chart-secret"},
	}}
	mustCreateWebhook(t, st, "ui-slack", rec.url("/ui-slack"), "ui-secret", true)

	d := newDispatcher(t, st, cfg, nil)
	ev := mustRecord(t, d, killEvent("ZXQ7K2"))

	// The UI webhook is deleted, and the chart-defined one is parked, between
	// the enqueue and the dispatch.
	hooks, err := st.ListWebhooks(ctx)
	if err != nil || len(hooks) != 1 {
		t.Fatalf("list webhooks: %v (%d rows)", err, len(hooks))
	}
	if err := st.DeleteWebhook(ctx, hooks[0].ID); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}
	cfg.StaticWebhooks[0].Enabled = enabled(false)
	d = newDispatcher(t, st, cfg, nil)

	if _, err := d.DispatchOnce(ctx); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("receiver saw %d deliveries for webhooks that are gone", got)
	}
	for name, row := range deliveriesFor(t, st, ev.ID) {
		if row.State != store.DeliveryFailed {
			t.Errorf("%s: state = %q, want failed (retrying cannot bring a deleted webhook back)", name, row.State)
		}
		if row.LastError == "" {
			t.Errorf("%s: no last_error explaining the terminal state", name)
		}
	}
}

// TestTestWebhookForBothSources: POST /webhooks/{name}/test delivers a
// synthetic signed event for a chart-defined webhook and a UI-created one
// alike, and writes nothing to the audit trail.
func TestTestWebhookForBothSources(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	rec := newReceiver(t)

	const chartSecret, uiSecret = "chart-secret", "ui-secret"
	cfg := config.Config{
		ExternalURL: "https://admin.example.com",
		StaticWebhooks: []config.StaticWebhook{
			{Name: "chart-pager", URL: rec.url("/chart-pager"), SecretEnv: "S", Secret: chartSecret},
		},
	}
	mustCreateWebhook(t, st, "ui-slack", rec.url("/ui-slack"), uiSecret, true)
	d := newDispatcher(t, st, cfg, nil)

	for name, secret := range map[string]string{"chart-pager": chartSecret, "ui-slack": uiSecret} {
		result, err := d.TestWebhook(ctx, name)
		if err != nil {
			t.Fatalf("%s: TestWebhook: %v", name, err)
		}
		if !result.OK || result.Status != http.StatusOK {
			t.Fatalf("%s: result = %+v, want ok with 200", name, result)
		}
		if result.DeliveryID == "" {
			t.Errorf("%s: no delivery id in the result", name)
		}
		got := rec.byPath()["/"+name]
		if len(got) != 1 {
			t.Fatalf("%s: receiver saw %d test deliveries, want 1", name, len(got))
		}
		c := got[0]
		if c.event != EventTest {
			t.Errorf("%s: %s = %q, want %q", name, HeaderEvent, c.event, EventTest)
		}
		if c.delivery != result.DeliveryID {
			t.Errorf("%s: header delivery id %q != reported %q", name, c.delivery, result.DeliveryID)
		}
		if !verifyIndependently(t, secret, c.timestamp, c.signature, c.body) {
			t.Errorf("%s: the test delivery is not signed with that webhook's secret", name)
		}
		payload := c.payloadOf(t)
		if payload["type"] != EventTest {
			t.Errorf("%s: payload type = %v, want %q", name, payload["type"], EventTest)
		}
		if s, _ := payload["summary"].(string); strings.TrimSpace(s) == "" {
			t.Errorf("%s: the test payload carries no summary", name)
		}
	}

	// A test send is not a moderation action: nothing lands in the audit
	// trail or the delivery queue.
	events, err := st.ListEvents(ctx, 0, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("a test send wrote %d moderation events", len(events))
	}
}

// TestTestWebhookReportsAReceiverRejection: the receiver saying no is a
// SUCCESSFUL test with a failing outcome, not an API error — the portal shows
// the status the operator needs to debug their endpoint.
func TestTestWebhookReportsAReceiverRejection(t *testing.T) {
	rec := newReceiver(t)
	rec.setStatus(http.StatusForbidden, "bad signature")
	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "chart-pager", URL: rec.url("/chart-pager"), SecretEnv: "S", Secret: "chart-secret"},
	}}
	d := newDispatcher(t, deadStore(t), cfg, nil)

	result, err := d.TestWebhook(t.Context(), "chart-pager")
	if err != nil {
		t.Fatalf("TestWebhook returned an error for a reachable receiver: %v", err)
	}
	if result.OK {
		t.Fatal("result.OK is true for a 403")
	}
	if result.Status != http.StatusForbidden {
		t.Errorf("result.Status = %d, want 403", result.Status)
	}
	if !strings.Contains(result.Error, "bad signature") {
		t.Errorf("result.Error = %q, want the receiver's explanation", result.Error)
	}
}

// TestTestWebhookOnADisabledWebhook: a parked webhook is immutable-from-the-UI
// or paused, not untestable — proving a channel works before enabling it is
// exactly when the button earns its place.
func TestTestWebhookOnADisabledWebhook(t *testing.T) {
	rec := newReceiver(t)
	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "chart-parked", URL: rec.url("/chart-parked"), SecretEnv: "S",
			Secret: "chart-secret", Enabled: enabled(false)},
	}}
	d := newDispatcher(t, deadStore(t), cfg, nil)

	result, err := d.TestWebhook(t.Context(), "chart-parked")
	if err != nil {
		t.Fatalf("TestWebhook on a disabled chart webhook: %v", err)
	}
	if !result.OK {
		t.Fatalf("result = %+v, want a successful test send", result)
	}
}

func TestTestWebhookUnknownName(t *testing.T) {
	st := newStore(t)
	d := newDispatcher(t, st, config.Config{}, nil)
	if _, err := d.TestWebhook(t.Context(), "nobody"); err == nil {
		t.Fatal("TestWebhook on an unknown name returned no error")
	}
}

// TestCrossOriginRedirectIsRefused: a webhook URL that 307s to another host
// must not hand that host X-Gawk-Signature — a valid MAC under the operator's
// key for a host the operator never configured.
func TestCrossOriginRedirectIsRefused(t *testing.T) {
	elsewhere := newReceiver(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.url("/stolen"), http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "chart-pager", URL: redirector.URL + "/pager", SecretEnv: "S", Secret: "chart-secret"},
	}}
	d := newDispatcher(t, deadStore(t), cfg, nil)

	result, err := d.TestWebhook(t.Context(), "chart-pager")
	if err != nil {
		t.Fatalf("TestWebhook: %v", err)
	}
	if result.OK {
		t.Fatal("the delivery followed a cross-origin redirect")
	}
	if !strings.Contains(result.Error, "different origin") {
		t.Errorf("result.Error = %q, want it to name the refused redirect", result.Error)
	}
	if got := elsewhere.count(); got != 0 {
		t.Fatalf("the other origin received %d requests carrying a signature", got)
	}
}

// TestSameOriginRedirectIsFollowed: the refusal is about the ORIGIN, not about
// redirects — a receiver canonicalizing its own path must keep working.
func TestSameOriginRedirectIsFollowed(t *testing.T) {
	var captured []byte
	var sawSignature string
	mux := http.NewServeMux()
	mux.HandleFunc("/pager", func(w http.ResponseWriter, r *http.Request) {
		// 307 preserves the method and body; a 301/302 would turn the POST
		// into a bodyless GET, which the receiver would rightly reject.
		http.Redirect(w, r, "/pager/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/pager/", func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		sawSignature = r.Header.Get(HeaderSignature)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "chart-pager", URL: srv.URL + "/pager", SecretEnv: "S", Secret: "chart-secret"},
	}}
	d := newDispatcher(t, deadStore(t), cfg, nil)

	result, err := d.TestWebhook(t.Context(), "chart-pager")
	if err != nil {
		t.Fatalf("TestWebhook: %v", err)
	}
	if !result.OK {
		t.Fatalf("a same-origin redirect was not followed: %+v", result)
	}
	if len(captured) == 0 || sawSignature == "" {
		t.Fatalf("the redirected request lost its body or signature (body %d bytes, signature %q)", len(captured), sawSignature)
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{"https://hooks.example.com/a", "https://hooks.example.com/b", true},
		{"https://hooks.example.com/a", "https://HOOKS.example.com/b", true},
		{"http://hooks.example.com/a", "https://hooks.example.com/a", true},  // an upgrade is the same trust boundary
		{"https://hooks.example.com/a", "http://hooks.example.com/a", false}, // a downgrade is not
		{"https://hooks.example.com/a", "https://evil.example.com/a", false},
		{"https://hooks.example.com/a", "https://hooks.example.com:8443/a", false},
	}
	for _, tc := range cases {
		from, err := url.Parse(tc.from)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.from, err)
		}
		to, err := url.Parse(tc.to)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.to, err)
		}
		if got := sameOrigin(from, to); got != tc.want {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestRetryDelayLadder(t *testing.T) {
	want := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	for attempts, d := range want {
		got, ok := retryDelay(attempts + 1)
		if !ok || got != d {
			t.Errorf("retryDelay(%d) = %s, %v; want %s, true", attempts+1, got, ok, d)
		}
	}
	if _, ok := retryDelay(MaxAttempts); ok {
		t.Errorf("retryDelay(%d) still schedules a retry; the budget is %d attempts", MaxAttempts, MaxAttempts)
	}
	if _, ok := retryDelay(0); !ok {
		t.Error("retryDelay(0) refuses to schedule: a row that never recorded an attempt would go straight to failed")
	}
}

func TestReadErrorBody(t *testing.T) {
	got := readErrorBody(strings.NewReader("invalid signature\n\r\tfor\x00 delivery"))
	if strings.ContainsAny(got, "\n\r\t\x00") {
		t.Fatalf("readErrorBody kept control characters: %q", got)
	}
	if got != "invalid signature for delivery" {
		t.Fatalf("readErrorBody = %q", got)
	}
	long := strings.Repeat("x", maxErrorBody*4)
	if n := len(readErrorBody(strings.NewReader(long))); n > maxErrorBody {
		t.Fatalf("readErrorBody returned %d bytes, want at most %d", n, maxErrorBody)
	}
}

// TestSignedTimestampOnTheWire is the on-the-wire half of the timestamp
// criterion: the header a real receiver reads is the one inside the MAC.
func TestSignedTimestampOnTheWire(t *testing.T) {
	rec := newReceiver(t)
	const secret = "chart-secret"
	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "chart-pager", URL: rec.url("/chart-pager"), SecretEnv: "S", Secret: secret},
	}}
	d := newDispatcher(t, deadStore(t), cfg, nil)
	if _, err := d.TestWebhook(t.Context(), "chart-pager"); err != nil {
		t.Fatalf("TestWebhook: %v", err)
	}
	got := rec.captures()
	if len(got) != 1 {
		t.Fatalf("receiver saw %d requests", len(got))
	}
	c := got[0]
	ts, err := strconv.ParseInt(c.timestamp, 10, 64)
	if err != nil {
		t.Fatalf("%s = %q, want unix seconds: %v", HeaderTimestamp, c.timestamp, err)
	}
	if delta := time.Since(time.Unix(ts, 0)); delta > time.Minute || delta < -time.Minute {
		t.Errorf("%s is %s away from now; a receiver's ±300 s replay window would reject it", HeaderTimestamp, delta)
	}
	if !verifyIndependently(t, secret, c.timestamp, c.signature, c.body) {
		t.Fatal("the signature does not verify over the header timestamp")
	}
	if verifyIndependently(t, secret, strconv.FormatInt(ts+1, 10), c.signature, c.body) {
		t.Fatal("re-dating the delivery kept the signature valid: the timestamp is not in the signed material")
	}
}

// TestRunDeliversOnKickAndStopsWithItsContext exercises the leader loop itself:
// Enqueue kicks it, it delivers without waiting for a tick, and it stops the
// moment its context ends — which is how a leadership handover is signalled
// (kube.Election.OnLeading's context is cancelled on losing the Lease).
func TestRunDeliversOnKickAndStopsWithItsContext(t *testing.T) {
	st := newStore(t)
	rec := newReceiver(t)
	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "pager", URL: rec.url("/pager"), SecretEnv: "S", Secret: "secret"},
	}}
	// A poll interval far longer than the test: anything delivered here was
	// delivered because of the Kick, not because a tick came round.
	d := newDispatcher(t, st, cfg, func(o *Options) { o.PollInterval = time.Hour })

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		d.Run(ctx)
	}()

	_ = mustRecord(t, d, killEvent("ZXQ7K2"))

	deadline := time.After(10 * time.Second)
	for rec.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("the running dispatcher never delivered after a Kick")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return when its context was cancelled: a lost Lease would leave a second dispatcher running")
	}
}

// TestTransportErrorDoesNotLeakTheWebhookURLPath: a webhook URL is frequently
// a CREDENTIAL — a Slack incoming-webhook path, an ntfy topic — and net/http
// puts the whole URL into every *url.Error. That string becomes last_error and
// the "webhook delivery failed" log line, both of which travel further than
// the OIDC-gated portal the URL is otherwise only shown in.
//
// internal/config already keeps webhook URLs out of the startup log (LogAttrs
// logs names only); this is the same rule on the error path.
func TestTransportErrorDoesNotLeakTheWebhookURLPath(t *testing.T) {
	// A port nothing listens on: the send fails at dial, which is exactly the
	// case whose error carries the URL.
	const secretPath = "/services/T00000000/B00000000/xxxxSECRETxxxx"
	cfg := config.Config{StaticWebhooks: []config.StaticWebhook{
		{Name: "chart-pager", URL: "http://127.0.0.1:1" + secretPath, SecretEnv: "S", Secret: "chart-secret"},
	}}
	d := newDispatcher(t, deadStore(t), cfg, func(o *Options) { o.RequestTimeout = 2 * time.Second })

	result, err := d.TestWebhook(t.Context(), "chart-pager")
	if err != nil {
		t.Fatalf("TestWebhook: %v", err)
	}
	if result.OK {
		t.Fatal("a dial to a closed port reported success")
	}
	if strings.Contains(result.Error, "xxxxSECRETxxxx") || strings.Contains(result.Error, secretPath) {
		t.Fatalf("the error carries the webhook URL's secret path: %q", result.Error)
	}
	// The origin is still there: enough to debug DNS or TLS.
	if !strings.Contains(result.Error, "127.0.0.1:1") {
		t.Errorf("the error names no origin at all, which makes it undebuggable: %q", result.Error)
	}
}
