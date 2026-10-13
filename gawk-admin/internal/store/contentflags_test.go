package store_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// post applies one content-flag POST and fails the test on a store error.
func post(t *testing.T, s *store.Store, body screening.ContentFlag, key string, at time.Time) store.FlagPostResult {
	t.Helper()
	res, err := s.PostContentFlag(t.Context(), store.FlagPost{Body: body, BroadcastKey: key, Actor: "gawk-sampler", Now: at}, nil)
	if err != nil {
		t.Fatalf("post %s/%s %s: %v", body.BroadcastID, body.PublisherEpoch, body.State, err)
	}
	return res
}

// eventsOfType returns the stored events of one type, newest first.
func eventsOfType(t *testing.T, s *store.Store, typ string) []store.Event {
	t.Helper()
	evs, err := s.ListEvents(t.Context(), store.EventQuery{Types: []string{typ}})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// D9: the first tripped POST raises, the ones after update — keeping the
// highest score seen and filling in a broadcast key the raise lacked — and a
// clear for the same session resolves it as cleared. A clear for another
// session, or for a broadcast with no flag, does nothing.
func TestContentFlagRaiseUpdateClear(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	raised := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", t0)
	if raised.Status != screening.FlagStatusRaised {
		t.Fatalf("first tripped POST: %q, want raised", raised.Status)
	}
	f := raised.Flag
	if f.State != store.FlagStateOpen || f.Mode != screening.ModeFlag || f.PeakScore != 0.95 || f.BroadcastKey != "" ||
		!f.RaisedAt.Equal(t0) || !f.LastPostAt.Equal(t0) || f.Body.Window.FlaggedSamples != 4 || !f.Unresolved() {
		t.Fatalf("raised flag = %+v", f)
	}

	higher := flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped)
	higher.Scores.Max, higher.SamplerID = 0.99, "gawk-sampler-1"
	upd := post(t, s, higher, "0123456789ab", t0.Add(5*time.Second))
	if upd.Status != screening.FlagStatusUpdated || upd.Flag.ID != f.ID {
		t.Fatalf("second tripped POST: %+v, want an update of flag %d", upd, f.ID)
	}
	if upd.Flag.PeakScore != 0.99 || upd.Flag.SamplerID != "gawk-sampler-1" || upd.Flag.BroadcastKey != "0123456789ab" ||
		!upd.Flag.LastPostAt.Equal(t0.Add(5*time.Second)) || !upd.Flag.RaisedAt.Equal(t0) {
		t.Fatalf("updated flag = %+v", upd.Flag)
	}

	// A lower window max keeps the peak; a different key does not replace
	// the one already recorded.
	lower := flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped)
	lower.Scores.Max = 0.5
	upd = post(t, s, lower, "ffffffffffff", t0.Add(10*time.Second))
	if upd.Flag.PeakScore != 0.99 || upd.Flag.BroadcastKey != "0123456789ab" || upd.Flag.Body.Scores.Max != 0.5 {
		t.Fatalf("after a lower window: peak %v, key %q, body max %v; want 0.99, the first key and the last body",
			upd.Flag.PeakScore, upd.Flag.BroadcastKey, upd.Flag.Body.Scores.Max)
	}

	got, err := s.GetContentFlag(ctx, f.ID)
	if err != nil || got.PeakScore != 0.99 || got.State != store.FlagStateOpen {
		t.Fatalf("GetContentFlag = %+v, %v", got, err)
	}
	if _, err := s.GetContentFlag(ctx, f.ID+1000); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetContentFlag(unknown) = %v, want ErrNotFound", err)
	}

	if res := post(t, s, flagBody("ABC234", "e-other", screening.ModeFlag, screening.StateCleared), "", t0.Add(15*time.Second)); res.Status != screening.FlagStatusIgnored {
		t.Fatalf("a clear for another session: %q, want ignored", res.Status)
	}
	if res := post(t, s, flagBody("NOFLAG", "e9", screening.ModeFlag, screening.StateCleared), "", t0.Add(15*time.Second)); res.Status != screening.FlagStatusIgnored {
		t.Fatalf("a clear with no flag: %q, want ignored", res.Status)
	}

	cleared := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateCleared), "", t0.Add(20*time.Second))
	if cleared.Status != screening.FlagStatusResolved || cleared.Flag.ID != f.ID {
		t.Fatalf("the clear: %+v", cleared)
	}
	c := cleared.Flag
	if c.State != store.FlagStateResolved || c.Resolution != events.ContentFlagResolutionCleared || c.ResolvedBy != "gawk-sampler" ||
		c.ResolvedAt == nil || !c.ResolvedAt.Equal(t0.Add(20*time.Second)) || c.Unresolved() {
		t.Fatalf("cleared flag = %+v", c)
	}
	resolved := eventsOfType(t, s, store.EventContentFlagResolved)
	if len(resolved) != 1 || resolved[0].ContentFlag().Resolution != events.ContentFlagResolutionCleared || resolved[0].ContentFlag().FlagID != f.ID {
		t.Fatalf("resolved events = %+v", resolved)
	}

	// The episode is over: the next trip in the same session is a NEW flag.
	again := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", t0.Add(time.Minute))
	if again.Status != screening.FlagStatusRaised || again.Flag.ID == f.ID {
		t.Fatalf("a trip after the clear: %+v, want a new raise", again)
	}
	if n := len(eventsOfType(t, s, store.EventContentFlag)); n != 2 {
		t.Fatalf("raised events = %d, want one per raise", n)
	}

	bad := flagBody("ABC234", "e1", screening.ModeFlag, "sideways")
	if _, err := s.PostContentFlag(ctx, store.FlagPost{Body: bad, Now: t0}, nil); err == nil {
		t.Fatal("a POST in an unknown state was applied")
	}
}

// D9: a tripped POST from a new publisher session ends the old session's
// unresolved flag (resolved `ended`, by the system, with its event) and
// raises its own, rather than updating a flag about a session that is gone.
func TestANewPublisherSessionEndsTheOldFlag(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	old := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", t0).Flag
	fresh := post(t, s, flagBody("ABC234", "e2", screening.ModeFlag, screening.StateTripped), "", t0.Add(time.Minute))
	if fresh.Status != screening.FlagStatusRaised || fresh.Flag.ID == old.ID || fresh.Flag.PublisherEpoch != "e2" {
		t.Fatalf("the new session's trip: %+v, want a raise of its own", fresh)
	}
	ended, err := s.GetContentFlag(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ended.State != store.FlagStateResolved || ended.Resolution != events.ContentFlagResolutionEnded || ended.ResolvedBy != "system" {
		t.Fatalf("the old session's flag = %+v, want resolved ended by system", ended)
	}
	res := eventsOfType(t, s, store.EventContentFlagResolved)
	if len(res) != 1 || res[0].ContentFlag().Resolution != events.ContentFlagResolutionEnded || res[0].BroadcastID != "ABC234" {
		t.Fatalf("resolved events = %+v", res)
	}
	if !strings.Contains(res[0].PayloadString(store.PayloadSummary), "the broadcast ended") {
		t.Errorf("summary %q does not say the broadcast ended", res[0].PayloadString(store.PayloadSummary))
	}
}

// D9: acknowledging stops reminders without resolving; only an open
// flag-mode flag can be acknowledged, and an observed flag says why not.
func TestAcknowledgeContentFlag(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	f := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", t0).Flag

	ack, err := s.AcknowledgeContentFlag(ctx, f.ID, "op@example.com", t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ack.State != store.FlagStateAcknowledged || ack.AcknowledgedBy != "op@example.com" || ack.AcknowledgedAt == nil ||
		!ack.AcknowledgedAt.Equal(t0.Add(time.Minute)) || !ack.Unresolved() || ack.Resolution != "" {
		t.Fatalf("acknowledged flag = %+v", ack)
	}
	if _, err := s.AcknowledgeContentFlag(ctx, f.ID, "op@example.com", t0.Add(2*time.Minute)); !errors.Is(err, store.ErrFlagNotOpen) {
		t.Fatalf("second acknowledge = %v, want ErrFlagNotOpen", err)
	}
	// Acknowledged: no reminder is due however long it waits.
	if _, ok, err := s.RemindContentFlag(ctx, f.ID, events.ContentFlagSignalLive, time.Minute, t0.Add(time.Hour), nil); err != nil || ok {
		t.Fatalf("remind an acknowledged flag = %v, %v; want not due", ok, err)
	}
	// A tripped POST still updates it: the signal is still recorded.
	if res := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", t0.Add(3*time.Minute)); res.Status != screening.FlagStatusUpdated ||
		res.Flag.State != store.FlagStateAcknowledged {
		t.Fatalf("a trip on an acknowledged flag: %+v", res)
	}
	// And a clear resolves it, so a later episode pages again.
	if res := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateCleared), "", t0.Add(4*time.Minute)); res.Status != screening.FlagStatusResolved {
		t.Fatalf("a clear on an acknowledged flag: %q, want resolved", res.Status)
	}

	obs := post(t, s, flagBody("DEF567", "e2", screening.ModeObserve, screening.StateTripped), "", t0).Flag
	if _, err := s.AcknowledgeContentFlag(ctx, obs.ID, "op@example.com", t0); !errors.Is(err, store.ErrFlagObserved) {
		t.Fatalf("acknowledge an observed flag = %v, want ErrFlagObserved", err)
	}
	if _, err := s.AcknowledgeContentFlag(ctx, obs.ID+1000, "op@example.com", t0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("acknowledge an unknown flag = %v, want ErrNotFound", err)
	}
}

// D10: a dismissal resolves the flag as dismissed and mutes the broadcast —
// flag-mode POSTs are muted until the mute ends, observe-mode ones are not —
// and only an unresolved flag-mode flag can be dismissed.
func TestDismissContentFlagMutesTheBroadcast(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	f := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", t0).Flag
	until := t0.Add(30 * time.Minute)

	out, err := s.DismissContentFlag(ctx, f.ID, "op@example.com", t0.Add(time.Minute), until, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != store.FlagStateResolved || out.Resolution != events.ContentFlagResolutionDismissed || out.ResolvedBy != "op@example.com" {
		t.Fatalf("dismissed flag = %+v", out)
	}
	res := eventsOfType(t, s, store.EventContentFlagResolved)
	if len(res) != 1 || res[0].ContentFlag().Resolution != events.ContentFlagResolutionDismissed ||
		!strings.Contains(res[0].PayloadString(store.PayloadSummary), "dismissed by op@example.com") {
		t.Fatalf("resolved events = %+v", res)
	}

	mutes, err := s.ActiveMutes(ctx, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := mutes["ABC234"]; !ok || !got.Equal(until) || len(mutes) != 1 {
		t.Fatalf("active mutes = %v, want ABC234 until %v", mutes, until)
	}

	if r := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", t0.Add(5*time.Minute)); r.Status != screening.FlagStatusMuted || r.Flag.ID != 0 {
		t.Fatalf("a trip under the mute: %+v, want muted with no flag", r)
	}
	if r := post(t, s, flagBody("ABC234", "e1", screening.ModeObserve, screening.StateTripped), "", t0.Add(5*time.Minute)); r.Status != screening.FlagStatusRaised {
		t.Fatalf("an observe-mode trip under the mute: %q, want raised (observe ignores mutes)", r.Status)
	}
	if n := len(eventsOfType(t, s, store.EventContentFlag)); n != 1 {
		t.Fatalf("raised events = %d, want only the original page", n)
	}

	if _, err := s.DismissContentFlag(ctx, f.ID, "op@example.com", t0, until, nil); !errors.Is(err, store.ErrFlagResolved) {
		t.Fatalf("dismiss a resolved flag = %v, want ErrFlagResolved", err)
	}
	obs, err := s.UnresolvedContentFlags(ctx, screening.ModeObserve)
	if err != nil || len(obs) != 1 {
		t.Fatalf("observed flags = %+v, %v", obs, err)
	}
	if _, err := s.DismissContentFlag(ctx, obs[0].ID, "op@example.com", t0, until, nil); !errors.Is(err, store.ErrFlagObserved) {
		t.Fatalf("dismiss an observed flag = %v, want ErrFlagObserved", err)
	}
	if _, err := s.DismissContentFlag(ctx, f.ID+1000, "op@example.com", t0, until, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dismiss an unknown flag = %v, want ErrNotFound", err)
	}

	// Past the mute the broadcast pages again, and a second dismissal moves
	// the one mute row rather than adding another.
	if mutes, _ := s.ActiveMutes(ctx, until.Add(time.Second)); len(mutes) != 0 {
		t.Fatalf("mutes after the end = %v, want none", mutes)
	}
	second := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", until.Add(time.Second))
	if second.Status != screening.FlagStatusRaised {
		t.Fatalf("a trip after the mute: %q, want raised", second.Status)
	}
	later := until.Add(time.Hour)
	if _, err := s.DismissContentFlag(ctx, second.Flag.ID, "lead@example.com", until.Add(time.Minute), later, nil); err != nil {
		t.Fatal(err)
	}
	var rows int
	var by string
	if err := s.Pool().QueryRow(ctx, `SELECT count(*), max(muted_by) FROM screening_mutes WHERE broadcast_id = 'ABC234'`).Scan(&rows, &by); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || by != "lead@example.com" {
		t.Fatalf("mute rows = %d by %q, want one row moved to the second dismissal", rows, by)
	}
}

// D9: an operator's kill resolves the broadcast's flag-mode flag as killed
// and leaves its observed flag to the session-end sweep; a second kill, or a
// resolve that lost the race, resolves nothing and is not an error.
func TestResolveFlagsForKill(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	flag := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "", t0).Flag
	obs := post(t, s, flagBody("ABC234", "e1", screening.ModeObserve, screening.StateTripped), "", t0).Flag
	post(t, s, flagBody("OTHER2", "e2", screening.ModeFlag, screening.StateTripped), "", t0)

	killed, err := s.ResolveFlagsForKill(ctx, "ABC234", "op@example.com", t0.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(killed) != 1 || killed[0].ID != flag.ID || killed[0].Resolution != events.ContentFlagResolutionKilled || killed[0].ResolvedBy != "op@example.com" {
		t.Fatalf("killed = %+v, want the one flag-mode flag resolved killed", killed)
	}
	if got, _ := s.GetContentFlag(ctx, obs.ID); got.State != store.FlagStateOpen {
		t.Fatalf("the observed flag = %+v, want it left open", got)
	}
	unresolved, err := s.UnresolvedContentFlags(ctx, screening.ModeFlag)
	if err != nil || len(unresolved) != 1 || unresolved[0].BroadcastID != "OTHER2" {
		t.Fatalf("unresolved flag-mode flags = %+v, %v; want only the other broadcast's", unresolved, err)
	}
	if again, err := s.ResolveFlagsForKill(ctx, "ABC234", "op@example.com", t0.Add(2*time.Minute), nil); err != nil || len(again) != 0 {
		t.Fatalf("a second kill = %+v, %v; want nothing", again, err)
	}
	if _, ok, err := s.ResolveContentFlag(ctx, flag, events.ContentFlagResolutionEnded, "", "system", t0.Add(3*time.Minute), nil); err != nil || ok {
		t.Fatalf("resolve an already-resolved flag = %v, %v; want not ok, no error", ok, err)
	}
	// An automatic resolution carries its reason through to the event.
	if _, ok, err := s.ResolveContentFlag(ctx, unresolved[0], events.ContentFlagResolutionAutoKilled, store.FlagReasonDeadline,
		store.ActorContentFlag, t0.Add(4*time.Minute), nil); err != nil || !ok {
		t.Fatalf("auto-kill resolve = %v, %v", ok, err)
	}
	ev := eventsOfType(t, s, store.EventContentFlagResolved)[0]
	if p := ev.ContentFlag(); p.Resolution != events.ContentFlagResolutionAutoKilled || p.Reason != store.FlagReasonDeadline ||
		ev.Actor != store.ActorContentFlag || !strings.Contains(ev.PayloadString(store.PayloadSummary), "killed automatically (deadline)") {
		t.Fatalf("auto-kill event = %+v (%+v)", ev, p)
	}
	// An observed flag resolves silently: it paged nobody.
	before := len(eventsOfType(t, s, store.EventContentFlagResolved))
	if _, ok, err := s.ResolveContentFlag(ctx, obs, events.ContentFlagResolutionEnded, "", "system", t0.Add(5*time.Minute), nil); err != nil || !ok {
		t.Fatalf("resolve the observed flag = %v, %v", ok, err)
	}
	if after := len(eventsOfType(t, s, store.EventContentFlagResolved)); after != before {
		t.Fatalf("resolving an observed flag wrote %d resolved event(s)", after-before)
	}
}

// D9: a reminder is due only once the last page is repage old, the counter
// and the event say which reminder it is, and the condition holds across
// calls so two sweeps cannot both send one.
func TestRemindContentFlagIsDueOnlyAfterRepage(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	f := post(t, s, flagBody("ABC234", "e1", screening.ModeFlag, screening.StateTripped), "0123456789ab", t0).Flag
	repage := 10 * time.Minute
	remind := func(at time.Time, signal string) (store.ContentFlag, bool) {
		t.Helper()
		out, ok, err := s.RemindContentFlag(ctx, f.ID, signal, repage, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out, ok
	}
	if _, ok := remind(t0.Add(9*time.Minute), events.ContentFlagSignalLive); ok {
		t.Fatal("reminded before repage")
	}
	first, ok := remind(t0.Add(10*time.Minute), events.ContentFlagSignalLive)
	if !ok || first.Reminders != 1 || first.LastRemindedAt == nil || !first.LastRemindedAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("first reminder = %+v, %v", first, ok)
	}
	if _, ok := remind(t0.Add(11*time.Minute), events.ContentFlagSignalLive); ok {
		t.Fatal("a second reminder a minute after the first")
	}
	second, ok := remind(t0.Add(20*time.Minute), events.ContentFlagSignalUnknown)
	if !ok || second.Reminders != 2 {
		t.Fatalf("second reminder = %+v, %v", second, ok)
	}
	evs := eventsOfType(t, s, store.EventContentFlagReminded)
	if len(evs) != 2 {
		t.Fatalf("reminded events = %d, want 2", len(evs))
	}
	p := evs[0].ContentFlag()
	if p.Reminder != 2 || p.Signal != events.ContentFlagSignalUnknown || p.RaisedAt != t0.Format(time.RFC3339) || p.FlagID != f.ID {
		t.Fatalf("newest reminder payload = %+v", p)
	}
	if evs[0].BroadcastKey != "0123456789ab" || evs[0].Actor != "system" {
		t.Errorf("reminder row: key %q, actor %q", evs[0].BroadcastKey, evs[0].Actor)
	}
	if want := "content flag on broadcast ABC234 is still unacknowledged after 20m0s (reminder 2; the sampler has gone quiet)"; evs[0].PayloadString(store.PayloadSummary) != want {
		t.Errorf("summary = %q, want %q", evs[0].PayloadString(store.PayloadSummary), want)
	}
	if _, ok, err := s.RemindContentFlag(ctx, f.ID+1000, events.ContentFlagSignalLive, repage, t0.Add(time.Hour), nil); err != nil || ok {
		t.Fatalf("remind an unknown flag = %v, %v", ok, err)
	}
}

// The list reads unresolved flag-mode flags by default, filters by state and
// mode, pages newest first and clamps the limit; the sweep's read returns
// every unresolved flag of a mode, or of both.
func TestListAndUnresolvedContentFlags(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	open := post(t, s, flagBody("AAA234", "e1", screening.ModeFlag, screening.StateTripped), "", t0).Flag
	done := post(t, s, flagBody("BBB234", "e2", screening.ModeFlag, screening.StateTripped), "", t0).Flag
	post(t, s, flagBody("BBB234", "e2", screening.ModeFlag, screening.StateCleared), "", t0.Add(time.Minute))
	obs := post(t, s, flagBody("CCC234", "e3", screening.ModeObserve, screening.StateTripped), "", t0).Flag

	ids := func(fs []store.ContentFlag) []int64 {
		out := []int64{}
		for _, f := range fs {
			out = append(out, f.ID)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		q    store.FlagQuery
		want []int64
	}{
		{"default", store.FlagQuery{}, []int64{open.ID}},
		{"resolved", store.FlagQuery{State: store.FlagFilterResolved}, []int64{done.ID}},
		{"all flag-mode", store.FlagQuery{State: store.FlagFilterAll}, []int64{done.ID, open.ID}},
		{"observe", store.FlagQuery{Mode: screening.ModeObserve}, []int64{obs.ID}},
		{"everything", store.FlagQuery{State: store.FlagFilterAll, Mode: store.FlagFilterAll}, []int64{obs.ID, done.ID, open.ID}},
		{"one", store.FlagQuery{State: store.FlagFilterAll, Mode: store.FlagFilterAll, Limit: 1}, []int64{obs.ID}},
		{"over the cap", store.FlagQuery{State: store.FlagFilterAll, Mode: store.FlagFilterAll, Limit: store.MaxFlagLimit * 10}, []int64{obs.ID, done.ID, open.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListContentFlags(ctx, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ids(got), tc.want) {
				t.Fatalf("ids = %v, want %v", ids(got), tc.want)
			}
		})
	}
	for mode, want := range map[string][]int64{"": {open.ID, obs.ID}, screening.ModeFlag: {open.ID}, screening.ModeObserve: {obs.ID}} {
		got, err := s.UnresolvedContentFlags(ctx, mode)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ids(got), want) {
			t.Errorf("UnresolvedContentFlags(%q) = %v, want %v", mode, ids(got), want)
		}
	}
}

// D12: a UI-created webhook shadowed by a chart-defined one of the same name
// is not a route, even when the UI row alone would be: only the chart's
// filter counts for that name.
func TestFlagRouteHonoursTheConfigWinsRule(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	if _, err := s.CreateWebhook(ctx, store.Webhook{Name: "pager", URL: "https://pager.example", Secret: "aw==", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	shadow := []store.ConfigWebhook{{Name: "pager", Events: []string{store.EventRoomAttached}}}
	if route, err := s.FlagRoute(ctx, shadow); err != nil || route {
		t.Fatalf("a UI row shadowed by a filtered chart webhook: %v, %v; want no route", route, err)
	}
	if route, err := s.FlagRoute(ctx, []store.ConfigWebhook{{Name: "other", Events: []string{store.EventRoomAttached}}}); err != nil || !route {
		t.Fatalf("an unshadowed unfiltered UI row: %v, %v; want a route", route, err)
	}
}

// D9: the signal is cleared once the sampler said so, live while a tripped
// POST is at most staleAfter old, unknown after.
func TestContentFlagSignal(t *testing.T) {
	stale := 2 * time.Minute
	f := store.ContentFlag{State: store.FlagStateOpen, LastPostAt: t0}
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{t0, events.ContentFlagSignalLive},
		{t0.Add(stale), events.ContentFlagSignalLive},
		{t0.Add(stale + time.Second), events.ContentFlagSignalUnknown},
	} {
		if got := f.Signal(tc.at, stale); got != tc.want {
			t.Errorf("Signal at +%v = %q, want %q", tc.at.Sub(t0), got, tc.want)
		}
	}
	f.State, f.Resolution = store.FlagStateResolved, events.ContentFlagResolutionCleared
	if got := f.Signal(t0, stale); got != "cleared" {
		t.Errorf("a cleared flag's signal = %q, want cleared", got)
	}
	if f.Unresolved() {
		t.Error("a resolved flag reads as unresolved")
	}
	if !(store.ContentFlag{State: store.FlagStateAcknowledged}).Unresolved() {
		t.Error("an acknowledged flag reads as resolved")
	}
}

// The flag payload is read through closed vocabularies and shape checks: no
// free text, no address and no out-of-range number rides a field out to a
// webhook, whatever a producer wrote (the EnforcementState rule).
func TestEventContentFlagIsAClosedVocabulary(t *testing.T) {
	good := `{"flagId":7,"samplerId":"gawk-sampler-0",
		"window":{"start":"2026-10-13T14:00:00+02:00","end":"2026-10-13T12:00:25Z","samples":6,"flaggedSamples":4},
		"scores":{"max":0.95,"mean":0.8},"labels":["FEMALE_GENITALIA_EXPOSED","not a label"],
		"resolution":"auto_killed","reason":"policy","raisedAt":"2026-10-13T12:00:00Z","reminder":2,"signal":"live"}`
	ev := store.Event{Type: store.EventContentFlag, Payload: json.RawMessage(good)}
	p := ev.ContentFlag()
	if p.FlagID != 7 || p.SamplerID != "gawk-sampler-0" || p.Resolution != events.ContentFlagResolutionAutoKilled ||
		p.Reason != store.FlagReasonPolicy || p.RaisedAt != "2026-10-13T12:00:00Z" || p.Reminder != 2 || p.Signal != events.ContentFlagSignalLive {
		t.Fatalf("a well-formed payload = %+v", p)
	}
	if p.Window == nil || p.Window.Start != "2026-10-13T12:00:00Z" || p.Window.Samples != 6 || p.Window.FlaggedSamples != 4 {
		t.Fatalf("window = %+v, want the start normalised to UTC", p.Window)
	}
	if p.Scores == nil || p.Scores.Max != 0.95 || !slices.Equal(p.Labels, []string{"FEMALE_GENITALIA_EXPOSED"}) {
		t.Fatalf("scores %+v, labels %v", p.Scores, p.Labels)
	}

	for _, tc := range []struct {
		name    string
		payload string
		check   func(store.ContentFlagPayload) bool
	}{
		{"no payload", ``, func(p store.ContentFlagPayload) bool { return p.FlagID == 0 && p.Window == nil }},
		{"not JSON", `nonsense`, func(p store.ContentFlagPayload) bool { return p.FlagID == 0 && p.SamplerID == "" }},
		{"a negative flag ID", `{"flagId":-3}`, func(p store.ContentFlagPayload) bool { return p.FlagID == 0 }},
		{"an IPv4 sampler", `{"samplerId":"10.0.0.7"}`, func(p store.ContentFlagPayload) bool { return p.SamplerID == "" }},
		{"an IPv6 sampler", `{"samplerId":"fd00::7"}`, func(p store.ContentFlagPayload) bool { return p.SamplerID == "" }},
		{"a sampler with a space", `{"samplerId":"gawk sampler"}`, func(p store.ContentFlagPayload) bool { return p.SamplerID == "" }},
		{"a non-ASCII sampler", `{"samplerId":"gawk-sämpler"}`, func(p store.ContentFlagPayload) bool { return p.SamplerID == "" }},
		{"an over-long sampler", `{"samplerId":"` + strings.Repeat("s", screening.MaxSamplerIDLen+1) + `"}`, func(p store.ContentFlagPayload) bool { return p.SamplerID == "" }},
		{"more flagged than taken", `{"window":{"samples":2,"flaggedSamples":3}}`, func(p store.ContentFlagPayload) bool { return p.Window == nil }},
		{"a negative window", `{"window":{"samples":-1,"flaggedSamples":0}}`, func(p store.ContentFlagPayload) bool { return p.Window == nil }},
		{"free-text window times", `{"window":{"start":"yesterday","end":"ABC234","samples":1,"flaggedSamples":1}}`,
			func(p store.ContentFlagPayload) bool {
				return p.Window != nil && p.Window.Start == "" && p.Window.End == ""
			}},
		{"a score over one", `{"scores":{"max":1.5,"mean":0.5}}`, func(p store.ContentFlagPayload) bool { return p.Scores == nil }},
		{"a negative score", `{"scores":{"max":0.5,"mean":-0.1}}`, func(p store.ContentFlagPayload) bool { return p.Scores == nil }},
		{"an unknown resolution", `{"resolution":"banned forever"}`, func(p store.ContentFlagPayload) bool { return p.Resolution == "" }},
		{"an unknown reason", `{"reason":"ABC234 looked bad"}`, func(p store.ContentFlagPayload) bool { return p.Reason == "" }},
		{"a free-text raisedAt", `{"raisedAt":"just now"}`, func(p store.ContentFlagPayload) bool { return p.RaisedAt == "" }},
		{"a zero reminder", `{"reminder":0}`, func(p store.ContentFlagPayload) bool { return p.Reminder == 0 }},
		{"a fractional reminder", `{"reminder":1.5}`, func(p store.ContentFlagPayload) bool { return p.Reminder == 0 }},
		{"an unknown signal", `{"signal":"on fire"}`, func(p store.ContentFlagPayload) bool { return p.Signal == "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := store.Event{Type: store.EventContentFlag}
			if tc.payload != "" {
				ev.Payload = json.RawMessage(tc.payload)
			}
			if p := ev.ContentFlag(); !tc.check(p) {
				t.Fatalf("ContentFlag() = %+v", p)
			}
		})
	}
}

// room.screening_changed's value is one of two words or nothing.
func TestEventRoomScreeningIsAClosedVocabulary(t *testing.T) {
	for payload, want := range map[string]string{
		`{"screening":"exempt"}`:         events.RoomScreeningExempt,
		`{"screening":"screened"}`:       events.RoomScreeningScreened,
		`{"screening":"EXEMPT"}`:         "",
		`{"screening":"exempt: ABC234"}`: "",
		`{"screening":true}`:             "",
		`{"room":"tuhisroom"}`:           "",
		`nonsense`:                       "",
	} {
		ev := store.Event{Type: store.EventRoomScreeningChanged, Payload: json.RawMessage(payload)}
		if got := ev.RoomScreening(); got != want {
			t.Errorf("RoomScreening(%s) = %q, want %q", payload, got, want)
		}
	}
}

// The R40 sentences name the broadcast, say how a flag ended and who did it,
// and fall back to "a broadcast" / "an operator" rather than print nothing.
func TestSummarizeContentFlagSentences(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{store.SummarizeContentFlagRaised("ABC234", 4, 6, 0.951),
			"content flag raised on broadcast ABC234: 4 of 6 samples flagged, peak score 0.95 — join and judge it"},
		{store.SummarizeContentFlagObserved("", 4, 6, 0.9),
			"observe mode would have flagged a broadcast: 4 of 6 samples, peak score 0.90"},
		{store.SummarizeContentFlagReminded("ABC234", 1, 10*time.Minute+20*time.Second, events.ContentFlagSignalLive),
			"content flag on broadcast ABC234 is still unacknowledged after 10m0s (reminder 1; the signal is still live)"},
		{store.SummarizeContentFlagResolved("ABC234", events.ContentFlagResolutionCleared, "", "gawk-sampler"),
			"content flag on broadcast ABC234 resolved: the signal cleared"},
		{store.SummarizeContentFlagResolved("ABC234", events.ContentFlagResolutionDismissed, "", ""),
			"content flag on broadcast ABC234 was dismissed by an operator"},
		{store.SummarizeContentFlagResolved("ABC234", events.ContentFlagResolutionKilled, "", "op@example.com"),
			"content flag on broadcast ABC234 resolved: the broadcast was killed by op@example.com"},
		{store.SummarizeContentFlagResolved("ABC234", events.ContentFlagResolutionAutoKilled, store.FlagReasonPolicy, store.ActorContentFlag),
			"content flag on broadcast ABC234 resolved: killed automatically (policy)"},
		{store.SummarizeContentFlagResolved("ABC234", events.ContentFlagResolutionAutoKilled, "", store.ActorContentFlag),
			"content flag on broadcast ABC234 resolved: killed automatically"},
		{store.SummarizeContentFlagResolved("", events.ContentFlagResolutionEnded, "", "system"),
			"content flag on a broadcast resolved: the broadcast ended"},
		{store.SummarizeContentFlagResolved("ABC234", "something new", "", "system"),
			"content flag on broadcast ABC234 resolved"},
		{store.SummarizeRoomScreening("static", "TuhisTestLab", "op@example.com", true),
			"static room TuhisTestLab was exempted from content screening by op@example.com"},
		{store.SummarizeRoomScreening("", "TuhisTestLab", "", false),
			"room TuhisTestLab is screened again, by an operator"},
		{store.SummarizeRoomScreening("dynamic", "", "op@example.com", false),
			"a room is screened again, by op@example.com"},
	} {
		if tc.got != tc.want {
			t.Errorf("got  %q\nwant %q", tc.got, tc.want)
		}
	}
}

// §3.1: the samples table's size is readable, and grows with what is stored.
func TestScreeningSamplesBytes(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	empty, err := s.ScreeningSamplesBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sent := map[int]float64{}
	for p := 1; p <= 400; p++ {
		sent[p] = 0.9
	}
	if _, err := s.IngestSamples(ctx, batch("e-size", 400, t0, sent)); err != nil {
		t.Fatal(err)
	}
	full, err := s.ScreeningSamplesBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if full <= empty || full <= 0 {
		t.Fatalf("samples table size: %d empty, %d after 400 samples; want it to grow", empty, full)
	}
}
