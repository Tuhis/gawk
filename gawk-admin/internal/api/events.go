package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
)

// handleListEvents serves the audit/notification feed, newest first, with
// cursor pagination by event ID (§4.7).
//
// Every event carries its webhook delivery state, because "a failed delivery
// must be SEEN" (§4.10) — the portal's events view is the only place an
// operator learns that the page they were counting on never arrived, and R40's
// DSA posture inherits this pipe.
func (a *API) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var afterID int64
	if raw := q.Get("afterId"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "afterId must be a non-negative integer")
			return
		}
		afterID = parsed
	}
	limit := store.DefaultEventLimit
	if raw := q.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "limit must be a positive integer")
			return
		}
		limit = parsed
	}
	// Clamp HERE, against the store's own rule, before anything pages by it.
	// An oversized limit is not an error — it is answered with the biggest
	// page there is — but the cursor below has to be computed against the
	// number of rows that were actually asked for, or a full page looks short
	// and the feed reports an end it has not reached.
	limit = store.ClampEventLimit(limit)

	types, ok := eventTypesParam(w, q["type"])
	if !ok {
		return
	}

	// R50: the feed carries two categories now. An unfiltered request answers
	// with BOTH, because a cursor that silently hid half the rows would page
	// past events the caller can see in the UI; the SPA sends the filter.
	category := q.Get("category")
	switch category {
	case "", store.CategoryModeration, store.CategoryActivity:
	default:
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"category must be moderation or activity")
		return
	}

	events, err := a.opts.Store.ListEvents(r.Context(), store.EventQuery{
		AfterID: afterID, Limit: limit, Types: types, Category: category,
	})
	if err != nil {
		a.fail(w, r, "list events", err)
		return
	}

	ids := make([]int64, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	deliveries, err := a.opts.Store.ListDeliveriesForEvents(r.Context(), ids)
	if err != nil {
		// The feed is still worth serving without delivery state; losing it
		// would hide the events themselves, which is strictly worse.
		a.log.Warn("loading webhook delivery state for the events feed failed", "err", err)
		deliveries = map[int64][]store.Delivery{}
	}

	out := make([]eventJSON, 0, len(events))
	for _, e := range events {
		out = append(out, renderEvent(e, deliveries[e.ID]))
	}

	// nextAfterId is present only when the page came back full — a short page
	// is the end of the feed, and handing out a cursor there would make the UI
	// fetch an empty page to discover it.
	body := eventsPageJSON{Events: out}
	if len(events) == limit && len(events) > 0 {
		last := events[len(events)-1].ID
		body.NextAfterID = &last
	}
	writeJSON(w, http.StatusOK, body)
}

// eventTypesParam reads the `type` filter: repeated (`?type=a&type=b`) or
// comma-separated (`?type=a,b`), the two forms every client library produces
// for one enum-valued query parameter.
//
// An unknown name is a 400 rather than an empty page, and that is the whole
// reason the filter validates at all: `type=ban.create` (singular, a typo a
// bot author makes once) would otherwise answer 200 with no events, which
// reads as "nothing has happened" — the same reassuring lie a null banState
// exists to prevent on the broadcast view.
func eventTypesParam(w http.ResponseWriter, raw []string) ([]string, bool) {
	out := []string{}
	seen := map[string]struct{}{}
	for _, v := range raw {
		for _, t := range strings.Split(v, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if !store.IsEventType(t) {
				writeError(w, http.StatusBadRequest, CodeBadRequest,
					"type must be one of "+strings.Join(store.AllEventTypes(), ", "))
				return nil, false
			}
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out, true
}
