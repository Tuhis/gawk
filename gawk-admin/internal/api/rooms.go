package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

func metaTime(t time.Time) *metav1.Time {
	m := metav1.NewTime(t)
	return &m
}

// Rooms is the Room CR surface the room routes need (R42, docs/44 D20).
// Implemented by *kube.RoomClient; nil in Options means rooms are OFF and none
// of the routes exist.
//
// Every write goes straight to the API server: unlike bans there is no
// Postgres row ahead of the CR, so a mutation either landed fleet-wide (every
// relay pod's informer sees the CR) or failed outright — there is no 202 here.
type Rooms interface {
	List(ctx context.Context) ([]kube.RoomObject, error)
	CreateStatic(ctx context.Context, req kube.StaticRoom) (secret string, err error)
	RotateSecret(ctx context.Context, name string) (secret string, err error)
	DeleteExisting(ctx context.Context, name string) (kube.RoomObject, error)
}

// Room error codes, in the {"error":{"code","message"}} envelope.
const (
	// CodeRoomExists: a room with that code already exists (409).
	CodeRoomExists = "room_exists"
	// CodeRoomNotStatic: secret rotation on a dynamic room (409).
	CodeRoomNotStatic = "room_not_static"
	// CodeRoomNotDynamic: "end" on a static room, which never ends (409).
	CodeRoomNotDynamic = "room_not_dynamic"
)

// maxRoomBroadcastsCap bounds the per-room override: the fleet's own
// -max-room-broadcasts defaults to 4 (docs/44 §4.10) and a room asking for a
// thousand tiles is a typo, not an intent.
const maxRoomBroadcastsCap = 64

type createRoomRequest struct {
	Code          string `json:"code"`
	DisplayName   string `json:"displayName,omitempty"`
	MaxBroadcasts int    `json:"maxBroadcasts,omitempty"`
	// WithAttachSecret mints an attach secret (docs/44 D8), returned ONCE in
	// the 201 body and never readable again.
	WithAttachSecret bool `json:"withAttachSecret,omitempty"`
}

type roomJSON struct {
	// Name is the CR name — the normalized code. Raw and joinable, like a
	// broadcast ID on /broadcasts: portal-only (docs/44 D16, docs/42 D8).
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Code is the display form: the slug as configured, or the upper-cased
	// dynamic code.
	Code          string `json:"code"`
	DisplayName   string `json:"displayName,omitempty"`
	MaxBroadcasts int    `json:"maxBroadcasts,omitempty"`
	Attachments   int    `json:"attachments"`
	// HomeHolder is the pod holding the room's lease; empty when no pod has
	// homed the room yet (a static room nobody has joined).
	HomeHolder string `json:"homeHolder,omitempty"`
	// Key is the fleet's HMAC'd handle (status.key) — the one form the
	// portal's `?key=` filter and a webhook agree on. Empty until homed.
	Key        string `json:"key,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
	EmptySince string `json:"emptySince,omitempty"`
	// HasAttachSecret says whether a static room is gated. The secret itself
	// has no field here: there is nowhere to put one (§4.7's rule for
	// webhook secrets, applied again).
	HasAttachSecret bool `json:"hasAttachSecret"`
	// Managed is true for a room the portal created; a `kubectl apply`'d one
	// is shown, deletable by an explicit request, but marked as not ours.
	Managed bool `json:"managed"`
}

// roomWithSecretJSON is the 201 (create) and 200 (rotate) body: the room plus
// the ONE-TIME secret. This is the only shape that ever carries it.
type roomWithSecretJSON struct {
	Room         roomJSON `json:"room"`
	AttachSecret string   `json:"attachSecret,omitempty"`
}

func renderRoom(obj kube.RoomObject) roomJSON {
	r := obj.Room
	out := roomJSON{
		Name:            obj.Name,
		Kind:            r.Spec.Kind,
		Code:            rooms.DisplayCode(&r),
		DisplayName:     r.Spec.DisplayName,
		MaxBroadcasts:   r.Spec.MaxBroadcasts,
		Attachments:     len(r.Status.Attachments),
		Key:             r.Status.Key,
		HasAttachSecret: r.Spec.AttachSecretRef != nil,
		Managed:         obj.Managed,
	}
	if r.Status.Lease != nil {
		out.HomeHolder = r.Status.Lease.Holder
	}
	if r.Status.CreatedAt != nil {
		out.CreatedAt = r.Status.CreatedAt.UTC().Format(time.RFC3339)
	} else if !r.CreationTimestamp.IsZero() {
		out.CreatedAt = r.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	if r.Status.EmptySince != nil {
		out.EmptySince = r.Status.EmptySince.UTC().Format(time.RFC3339)
	}
	return out
}

// handleListRooms lists both kinds from the CR list, newest first.
//
// A CR that could not be decoded is reported by name with its kind blank
// rather than dropped: an operator who cannot see a stuck object cannot fix
// it, and the delete route works by name alone.
func (a *API) handleListRooms(w http.ResponseWriter, r *http.Request) {
	list, err := a.opts.Rooms.List(r.Context())
	if err != nil {
		a.failRoom(w, r, "list rooms", err)
		return
	}
	out := make([]roomJSON, 0, len(list))
	for _, obj := range list {
		if obj.Err != nil {
			a.log.Warn("room CR could not be decoded; listing it by name only", "crName", obj.Name, "err", obj.Err)
			out = append(out, roomJSON{Name: obj.Name, Code: obj.Name})
			continue
		}
		out = append(out, renderRoom(obj))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			// Static rooms first: they are the ones an operator manages;
			// dynamic ones come and go.
			return out[i].Kind == rooms.KindStatic
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

// handleCreateRoom creates a static room (docs/44 D2, D4).
//
// A slug with the DYNAMIC-code shape — exactly six characters of the broadcast
// alphabet — is refused even though the relay would resolve it by name: the
// join box accepts six characters and resolves rooms first, so such a slug
// would be reachable by a typed code, which is precisely what "static rooms
// are link-only" (D2) rules out. It is a 400 the operator can fix by adding a
// character.
func (a *API) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	var req createRoomRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	code, err := rooms.NormalizeCode(req.Code)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			fmt.Sprintf("code must be %d-%d characters of [A-Za-z0-9-], not starting or ending with '-'",
				rooms.MinCodeLen, rooms.MaxCodeLen))
		return
	}
	if rooms.IsDynamicShape(code) {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"a static room code must not look like a six-character dynamic code: the join box would resolve it as one (docs/44 D2)")
		return
	}
	if req.MaxBroadcasts < 0 || req.MaxBroadcasts > maxRoomBroadcastsCap {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			fmt.Sprintf("maxBroadcasts must be 0 (the fleet default) or 1..%d", maxRoomBroadcastsCap))
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if len(displayName) > 128 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "displayName must be at most 128 characters")
		return
	}

	secret, err := a.opts.Rooms.CreateStatic(r.Context(), kube.StaticRoom{
		Code:             strings.TrimSpace(req.Code),
		DisplayName:      displayName,
		MaxBroadcasts:    req.MaxBroadcasts,
		WithAttachSecret: req.WithAttachSecret,
	})
	if err != nil {
		a.failRoom(w, r, "create the room", err)
		return
	}
	created := kube.RoomObject{Name: code, Managed: true}
	created.Room.Spec = rooms.RoomSpec{
		Kind: rooms.KindStatic, DisplayCode: strings.TrimSpace(req.Code),
		DisplayName: displayName, MaxBroadcasts: req.MaxBroadcasts,
	}
	if secret != "" {
		created.Room.Spec.AttachSecretRef = &rooms.SecretKeyRef{Name: kube.RoomSecretName(code), Key: kube.RoomSecretKey}
	}
	created.Room.Status.CreatedAt = metaTime(a.now())

	a.recordRoom(r.Context(), store.EventRoomCreated, id.Actor(), created)
	a.kick()
	// The code is a joinable secret (docs/44 D16); no key exists until a pod
	// homes the room, so the log names nothing but the kind.
	a.log.Info("static room created", "actor", id.Actor(), "withAttachSecret", secret != "")
	writeJSON(w, http.StatusCreated, roomWithSecretJSON{Room: renderRoom(created), AttachSecret: secret})
}

// handleRotateRoomSecret mints a fresh attach secret for a static room and
// returns it once.
func (a *API) handleRotateRoomSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	name, ok := roomName(w, r)
	if !ok {
		return
	}
	secret, err := a.opts.Rooms.RotateSecret(r.Context(), name)
	if err != nil {
		a.failRoom(w, r, "rotate the attach secret", err)
		return
	}
	obj, err := a.lookupRoom(r.Context(), name)
	if err != nil {
		// Rotated, but the read-back failed: the secret is still the answer.
		a.log.Warn("re-reading a room after rotating its secret failed", "err", err)
		obj = kube.RoomObject{Name: name, Managed: true}
		obj.Room.Spec = rooms.RoomSpec{Kind: rooms.KindStatic,
			AttachSecretRef: &rooms.SecretKeyRef{Name: kube.RoomSecretName(name), Key: kube.RoomSecretKey}}
	}
	a.recordRoom(r.Context(), store.EventRoomSecretRotated, id.Actor(), obj)
	a.log.Info("room attach secret rotated", "actor", id.Actor(), "roomKey", obj.Room.Status.Key)
	writeJSON(w, http.StatusOK, roomWithSecretJSON{Room: renderRoom(obj), AttachSecret: secret})
}

// handleDeleteRoom deletes a room of either kind. For a dynamic room this is
// "end room" (docs/44 D20): the CR deletion is what every relay pod's informer
// acts on, closing the sessions with 4007. handleEndRoom is the same action
// restricted to dynamic rooms, for a client that means "end" and would rather
// be told than delete a static room by mistake.
func (a *API) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	a.deleteRoom(w, r, false)
}

func (a *API) handleEndRoom(w http.ResponseWriter, r *http.Request) {
	a.deleteRoom(w, r, true)
}

func (a *API) deleteRoom(w http.ResponseWriter, r *http.Request, dynamicOnly bool) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	name, ok := roomName(w, r)
	if !ok {
		return
	}
	if dynamicOnly {
		obj, err := a.lookupRoom(r.Context(), name)
		if err != nil {
			a.failRoom(w, r, "look up the room", err)
			return
		}
		if obj.Err == nil && obj.Room.Spec.Kind != rooms.KindDynamic {
			writeError(w, http.StatusConflict, CodeRoomNotDynamic,
				"a static room never ends; delete it instead (docs/44 D7)")
			return
		}
	}
	// The event is recorded BEFORE the CR goes, so the reconciler's room
	// sweep — which sees the deletion on its next pass — finds the operator's
	// record and does not add a "system" one (store.RoomEndedSince).
	obj, err := a.opts.Rooms.DeleteExisting(r.Context(), name)
	if err != nil {
		a.failRoom(w, r, "delete the room", err)
		return
	}
	a.recordRoom(r.Context(), store.EventRoomEnded, id.Actor(), obj)
	a.kick()
	a.log.Info("room ended", "actor", id.Actor(), "kind", obj.Room.Spec.Kind, "roomKey", obj.Room.Status.Key)
	w.WriteHeader(http.StatusNoContent)
}

// lookupRoom finds one room in the list. The Rooms seam has no Get on purpose:
// one read path, and the list is small.
func (a *API) lookupRoom(ctx context.Context, name string) (kube.RoomObject, error) {
	list, err := a.opts.Rooms.List(ctx)
	if err != nil {
		return kube.RoomObject{}, err
	}
	for _, obj := range list {
		if obj.Name == name {
			return obj, nil
		}
	}
	return kube.RoomObject{}, kube.ErrRoomNotFound
}

// roomName reads and normalizes the path's room code; a malformed one is a
// 404, since no such CR can exist.
func roomName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name, err := rooms.NormalizeCode(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such room")
		return "", false
	}
	return name, true
}

// failRoom maps the kube sentinels onto their statuses; anything else is the
// generic exit (503 only when Postgres is the problem, which here it never is
// — a Kubernetes API failure is a 500 with the log line).
func (a *API) failRoom(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, kube.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such room")
	case errors.Is(err, kube.ErrRoomExists):
		writeError(w, http.StatusConflict, CodeRoomExists, "a room with that code already exists")
	case errors.Is(err, kube.ErrRoomNotStatic):
		writeError(w, http.StatusConflict, CodeRoomNotStatic,
			"only a static room has an attach secret; a dynamic room is gated by its creator token (docs/44 D8)")
	case errors.Is(err, kube.ErrRoomNotDynamic):
		writeError(w, http.StatusConflict, CodeRoomNotDynamic, "a static room never ends; delete it instead")
	case errors.Is(err, rooms.ErrInvalidCode):
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	default:
		a.fail(w, r, what, err)
	}
}

// recordRoom persists a room event: the raw code under the portal-only key,
// the HMAC'd key (when a pod has homed the room) under the one internal/notify
// forwards, and the kind-only summary (docs/44 D16).
func (a *API) recordRoom(ctx context.Context, eventType, actor string, obj kube.RoomObject) {
	kind := obj.Room.Spec.Kind
	payload := map[string]any{
		store.PayloadSummary:  store.SummarizeRoom(eventType, kind, actor),
		store.PayloadRoom:     obj.Name,
		store.PayloadRoomKind: kind,
		"displayCode":         rooms.DisplayCode(&obj.Room),
	}
	if key := obj.Room.Status.Key; key != "" {
		payload[store.PayloadRoomKey] = key
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{}`)
	}
	a.record(ctx, store.Event{
		Type:       eventType,
		OccurredAt: a.now(),
		Actor:      actor,
		Payload:    raw,
	})
}
