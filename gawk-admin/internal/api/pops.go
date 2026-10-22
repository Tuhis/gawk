package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// Pops is the Pop CR surface the PoP routes need (R74, docs/76 §7.2).
// Implemented by *kube.PopClient; nil in Options means PoPs are OFF and none
// of the routes exist.
//
// Like rooms, every write goes straight to the API server: a Pop's CR is its
// whole truth, so there is no Postgres row ahead of it.
type Pops interface {
	Create(ctx context.Context, req kube.NewPop) (kube.PopObject, error)
	List(ctx context.Context) ([]kube.PopObject, error)
	Get(ctx context.Context, name string) (kube.PopObject, error)
	PatchSpec(ctx context.Context, name string, edit func(*pops.PopSpec) error) (before, after kube.PopObject, err error)
	Repair(ctx context.Context, name string, pairing pops.Pairing) (kube.PopObject, error)
	Revoke(ctx context.Context, name string) (kube.PopObject, error)
	Delete(ctx context.Context, name string, check func(kube.PopObject) error) (kube.PopObject, error)
}

// PoP error codes, in the {"error":{"code","message"}} envelope.
const (
	// CodePopExists: a PoP with that name already exists (409).
	CodePopExists = "pop_exists"
	// CodePopRevoked: a Re-pair of a revoked PoP, whose links are all
	// refused, so a new pairing string would pair nothing (409).
	CodePopRevoked = "pop_revoked"
	// CodeWallCodeTaken: the wall's room code is already a room (409). The
	// PoP is not created.
	CodeWallCodeTaken = "wall_code_taken"
)

// PoP state values, docs/76 §7.2's card states.
const (
	PopStateWaiting = "waiting-for-pairing"
	PopStateLinked  = "linked"
	PopStateOffline = "offline"
	PopStateRevoked = "revoked"
	PopStateEnded   = "ended"
	// PopStateInvalid is a CR that could not be decoded: listed by name so
	// it can be deleted, never acted on otherwise.
	PopStateInvalid = "invalid"
)

// Validation bounds. They catch typos, not intents: the budget is the
// party's uplink, and nobody's is 10 Gbps.
const (
	maxPopDisplayName  = 64
	maxPopMbps         = 10000
	maxPopStreams      = 64
	maxPopBroadcasts   = 1000
	maxWallParticipant = 1000
)

type popBudgetJSON struct {
	MaxMbps    int `json:"maxMbps"`
	MaxStreams int `json:"maxStreams"`
}

type popLimitsJSON struct {
	MaxBroadcasts      int `json:"maxBroadcasts"`
	MaxDownlinkStreams int `json:"maxDownlinkStreams"`
}

type popGuestsJSON struct {
	AutoSwitch bool `json:"autoSwitch"`
}

type popWallJSON struct {
	// RoomCode is the wall's static room code: raw and joinable, like any
	// room code on this API.
	RoomCode string `json:"roomCode"`
}

type popNamedJSON struct {
	Enabled bool `json:"enabled"`
	// Host is the random name gawk-admin assigned (status.tls.host);
	// CertificateNotAfter is the issued chain's expiry.
	Host                string `json:"host,omitempty"`
	CertificateNotAfter string `json:"certificateNotAfter,omitempty"`
}

type popPairingJSON struct {
	IssuedAt  string `json:"issuedAt,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	// Paired reports a credential minted from the CURRENT pairing string;
	// PairedAt is when. The credential itself is never readable anywhere.
	Paired   bool   `json:"paired"`
	PairedAt string `json:"pairedAt,omitempty"`
}

// popLinkJSON is status.link plus the lease holder: what the relays last
// wrote. It may carry the PoP's public and LAN addresses.
type popLinkJSON struct {
	Connected   bool     `json:"connected"`
	Since       string   `json:"since,omitempty"`
	Version     string   `json:"version,omitempty"`
	Proto       int      `json:"proto,omitempty"`
	PublicAddrs []string `json:"publicAddrs,omitempty"`
	LanAddr     string   `json:"lanAddr,omitempty"`
	// Pod is the link-home pod (status.lease.holder).
	Pod string `json:"pod,omitempty"`
}

// popLiveJSON is the link-home pod's live view (/internal/admin/pops). It is
// absent when no reachable pod holds the PoP's link.
type popLiveJSON struct {
	Pod             string        `json:"pod"`
	Live            int           `json:"live"`
	Forwarded       int           `json:"forwarded"`
	OutsideViewers  int           `json:"outsideViewers"`
	UplinkMbps      float64       `json:"uplinkMbps"`
	UplinkStreams   int           `json:"uplinkStreams"`
	Uplink          popBudgetJSON `json:"uplink"`
	Downlink        popBudgetJSON `json:"downlink"`
	DownlinkStreams int           `json:"downlinkStreams"`
	GuestsConnected int           `json:"guestsConnected"`
	OffersSent      int           `json:"offersSent"`
}

type popBroadcastJSON struct {
	// ID is the raw, joinable broadcast ID (docs/42 D8).
	ID               string               `json:"id"`
	Label            string               `json:"label,omitempty"`
	PublisherLanAddr string               `json:"publisherLanAddr,omitempty"`
	LocalViewers     int                  `json:"localViewers"`
	OutsideViewers   int                  `json:"outsideViewers"`
	Forwarded        bool                 `json:"forwarded"`
	Wall             string               `json:"wall,omitempty"`
	Links            *attachmentLinksJSON `json:"links,omitempty"`
}

type popHealthJSON struct {
	Key    string `json:"key"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type popJSON struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// State is one of the PopState* values; StateSince dates it when known.
	State      string `json:"state"`
	StateSince string `json:"stateSince,omitempty"`
	NotAfter   string `json:"notAfter,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
	Managed    bool   `json:"managed"`
	Revoked    bool   `json:"revoked"`

	Pairing popPairingJSON `json:"pairing"`
	// Uplink and Limits are effective (defaults applied); Downlink is the
	// spec, 0 meaning unlimited at the PoP.
	Uplink       popBudgetJSON `json:"uplink"`
	Downlink     popBudgetJSON `json:"downlink"`
	Limits       popLimitsJSON `json:"limits"`
	Guests       popGuestsJSON `json:"guests"`
	Wall         *popWallJSON  `json:"wall,omitempty"`
	NamedAddress popNamedJSON  `json:"namedAddress"`

	Link *popLinkJSON `json:"link,omitempty"`
	Live *popLiveJSON `json:"live,omitempty"`
	// Broadcasts and Health are the DETAIL projection (GET /pops/{name});
	// a list row omits both.
	Broadcasts *[]popBroadcastJSON `json:"broadcasts,omitempty"`
	Health     *[]popHealthJSON    `json:"health,omitempty"`
}

type popsPageJSON struct {
	Pops []popJSON `json:"pops"`
}

// popWithPairingJSON is the 201 (create) and 200 (repair) body: the PoP plus
// the ONE-TIME pairing string. This is the only shape that ever carries it;
// the CR stores its SHA-256 alone (docs/76 item 7).
type popWithPairingJSON struct {
	Pop           popJSON `json:"pop"`
	PairingString string  `json:"pairingString"`
	ExpiresAt     string  `json:"expiresAt"`
}

type popBudgetRequest struct {
	MaxMbps    int `json:"maxMbps,omitempty"`
	MaxStreams int `json:"maxStreams,omitempty"`
}

type popLimitsRequest struct {
	MaxBroadcasts      int `json:"maxBroadcasts,omitempty"`
	MaxDownlinkStreams int `json:"maxDownlinkStreams,omitempty"`
}

type popGuestsRequest struct {
	// AutoSwitch nil is the default, on.
	AutoSwitch *bool `json:"autoSwitch,omitempty"`
}

type popWallRequest struct {
	// RoomCode defaults to the PoP name.
	RoomCode string `json:"roomCode,omitempty"`
	// MaxParticipants is the wall room's optional participant cap; 0
	// leaves the fleet's -max-room-participants.
	MaxParticipants int `json:"maxParticipants,omitempty"`
}

type createPopRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// NotAfter is an RFC 3339 end date; links are refused after it.
	NotAfter     string            `json:"notAfter,omitempty"`
	Uplink       *popBudgetRequest `json:"uplink,omitempty"`
	Downlink     *popBudgetRequest `json:"downlink,omitempty"`
	Limits       *popLimitsRequest `json:"limits,omitempty"`
	Guests       *popGuestsRequest `json:"guests,omitempty"`
	Wall         *popWallRequest   `json:"wall,omitempty"`
	NamedAddress bool              `json:"namedAddress,omitempty"`
}

// popPatchRequest edits a PoP. An absent field is left alone; notAfter ""
// clears the end date. The pairing and the wall are not editable here: a
// Re-pair is its own route because it ends the credential, and the wall is
// created with the PoP.
type popPatchRequest struct {
	DisplayName  *string           `json:"displayName,omitempty"`
	NotAfter     *string           `json:"notAfter,omitempty"`
	Uplink       *popBudgetRequest `json:"uplink,omitempty"`
	Downlink     *popBudgetRequest `json:"downlink,omitempty"`
	Limits       *popLimitsRequest `json:"limits,omitempty"`
	Guests       *popGuestsRequest `json:"guests,omitempty"`
	NamedAddress *bool             `json:"namedAddress,omitempty"`
}

func rfc3339(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func timeString(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// renderPop merges a Pop CR with the fleet's live view of it. detail adds the
// broadcasts and the health lines.
func (a *API) renderPop(obj kube.PopObject, live *relayscan.PopAggregate, detail bool) popJSON {
	if obj.Err != nil {
		return popJSON{Name: obj.Name, State: PopStateInvalid, Managed: obj.Managed}
	}
	p := &obj.Pop
	up, lim := p.EffectiveUplink(), p.EffectiveLimits()
	out := popJSON{
		Name:        obj.Name,
		DisplayName: p.Spec.DisplayName,
		NotAfter:    rfc3339(p.Spec.NotAfter),
		CreatedAt:   rfc3339(&p.CreationTimestamp),
		Managed:     obj.Managed,
		Revoked:     p.Spec.Revoked,
		Pairing: popPairingJSON{
			IssuedAt:  rfc3339(p.Spec.Pairing.IssuedAt),
			ExpiresAt: rfc3339(p.Spec.Pairing.ExpiresAt),
			Paired:    p.CredentialCurrent(),
		},
		Uplink:       popBudgetJSON{MaxMbps: up.MaxMbps, MaxStreams: up.MaxStreams},
		Downlink:     popBudgetJSON{MaxMbps: p.Spec.Downlink.MaxMbps, MaxStreams: p.Spec.Downlink.MaxStreams},
		Limits:       popLimitsJSON{MaxBroadcasts: lim.MaxBroadcasts, MaxDownlinkStreams: lim.MaxDownlinkStreams},
		Guests:       popGuestsJSON{AutoSwitch: p.AutoSwitch()},
		NamedAddress: popNamedJSON{Enabled: p.Spec.NamedAddress.Enabled},
	}
	if out.Pairing.Paired && p.Status.Credential != nil {
		out.Pairing.PairedAt = rfc3339(p.Status.Credential.PairedAt)
	}
	if p.Spec.Wall != nil {
		out.Wall = &popWallJSON{RoomCode: p.Spec.Wall.RoomCode}
	}
	if tls := p.Status.TLS; tls != nil {
		out.NamedAddress.Host = tls.Host
		out.NamedAddress.CertificateNotAfter = rfc3339(tls.NotAfter)
	}
	if l := p.Status.Link; l != nil {
		out.Link = &popLinkJSON{Connected: l.Connected, Since: rfc3339(l.Since), Version: l.Version, Proto: l.Proto,
			PublicAddrs: l.PublicAddrs, LanAddr: l.LanAddr}
		if p.Status.Lease != nil {
			out.Link.Pod = p.Status.Lease.Holder
		}
	}
	if live != nil {
		s := live.PopState
		out.Live = &popLiveJSON{
			Pod: live.Pod, Live: s.Live, Forwarded: s.Forwarded, OutsideViewers: s.OutsideViewers,
			UplinkMbps: s.UplinkMbps, UplinkStreams: s.UplinkStreams,
			Uplink:          popBudgetJSON{MaxMbps: s.Uplink.MaxMbps, MaxStreams: s.Uplink.MaxStreams},
			Downlink:        popBudgetJSON{MaxMbps: s.Downlink.MaxMbps, MaxStreams: s.Downlink.MaxStreams},
			DownlinkStreams: s.DownlinkStreams, GuestsConnected: s.GuestsConnected, OffersSent: s.OffersSent,
		}
		// The live view is fresher than status.link, which the relays
		// write only on link changes.
		if out.Link == nil {
			out.Link = &popLinkJSON{}
		}
		out.Link.Connected = s.Linked
		out.Link.Pod = live.Pod
		if s.Since != nil {
			out.Link.Since = timeString(s.Since)
		}
		if s.Version != "" {
			out.Link.Version, out.Link.Proto = s.Version, s.Proto
		}
		if len(s.PublicAddrs) > 0 {
			out.Link.PublicAddrs = s.PublicAddrs
		}
		if s.LanAddr != "" {
			out.Link.LanAddr = s.LanAddr
		}
		if s.NamedHost != "" && out.NamedAddress.Host == "" {
			out.NamedAddress.Host = s.NamedHost
		}
	}
	out.State, out.StateSince = popState(p, out.Link, a.now())
	if detail {
		bs := []popBroadcastJSON{}
		hs := []popHealthJSON{}
		if live != nil {
			for _, b := range live.Broadcasts {
				row := popBroadcastJSON{ID: b.ID, Label: b.Label, PublisherLanAddr: b.PublisherLanAddr,
					LocalViewers: b.LocalViewers, OutsideViewers: b.OutsideViewers, Forwarded: b.Forwarded, Wall: b.Wall}
				if base := a.opts.Config.AppBaseURL; base != "" {
					row.Links = &attachmentLinksJSON{Watch: base + "/#/view/" + b.ID}
				}
				bs = append(bs, row)
			}
			for _, h := range live.Health {
				hs = append(hs, popHealthJSON{Key: h.Key, OK: h.OK, Detail: h.Detail})
			}
		}
		out.Broadcasts, out.Health = &bs, &hs
	}
	return out
}

// popState is the card's state (docs/76 §7.2). Revoked and Ended are the
// operator's decisions and win; a PoP with no credential from its current
// string is waiting for pairing, whatever its link once was, because that
// credential is refused (item 9).
func popState(p *pops.Pop, link *popLinkJSON, now time.Time) (string, string) {
	switch {
	case p.Spec.Revoked:
		return PopStateRevoked, ""
	case p.Spec.NotAfter != nil && now.After(p.Spec.NotAfter.Time):
		return PopStateEnded, rfc3339(p.Spec.NotAfter)
	case !p.CredentialCurrent():
		return PopStateWaiting, rfc3339(p.Spec.Pairing.IssuedAt)
	case link != nil && link.Connected:
		return PopStateLinked, link.Since
	case link != nil:
		return PopStateOffline, link.Since
	default:
		return PopStateOffline, ""
	}
}

// livePops indexes the fleet's PoP view by name. A failed scan is an empty
// view, not an error: the CR list is still the truth of what exists.
func (a *API) livePops(ctx context.Context) map[string]relayscan.PopAggregate {
	out := map[string]relayscan.PopAggregate{}
	if a.opts.Fleet == nil {
		return out
	}
	snap, err := a.opts.Fleet.Snapshot(ctx)
	if err != nil {
		a.log.Warn("relay enumeration failed; rendering PoPs from their CRs alone", "err", err)
		return out
	}
	for _, p := range snap.Pops {
		out[p.Name] = p
	}
	return out
}

func (a *API) handleListPops(w http.ResponseWriter, r *http.Request) {
	list, err := a.opts.Pops.List(r.Context())
	if err != nil {
		a.failPop(w, r, "list PoPs", err)
		return
	}
	live := a.livePops(r.Context())
	out := make([]popJSON, 0, len(list))
	for _, obj := range list {
		var l *relayscan.PopAggregate
		if agg, ok := live[obj.Name]; ok {
			l = &agg
		}
		out = append(out, a.renderPop(obj, l, false))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, popsPageJSON{Pops: out})
}

func (a *API) handleGetPop(w http.ResponseWriter, r *http.Request) {
	name, ok := popName(w, r)
	if !ok {
		return
	}
	obj, err := a.opts.Pops.Get(r.Context(), name)
	if err != nil {
		a.failPop(w, r, "look up the PoP", err)
		return
	}
	writeJSON(w, http.StatusOK, a.renderPop(obj, a.livePop(r.Context(), name), true))
}

func (a *API) livePop(ctx context.Context, name string) *relayscan.PopAggregate {
	if agg, ok := a.livePops(ctx)[name]; ok {
		return &agg
	}
	return nil
}

// newPairing mints a pairing string and the spec.pairing that records only
// its hash.
func (a *API) newPairing() (string, pops.Pairing, error) {
	token, err := pops.NewPairingToken()
	if err != nil {
		return "", pops.Pairing{}, err
	}
	now := a.now().UTC().Truncate(time.Second)
	ttl := a.opts.Config.PopPairingTTL
	if ttl <= 0 {
		ttl = pops.DefaultPairingTTL
	}
	issued, expires := metav1.NewTime(now), metav1.NewTime(now.Add(ttl))
	return pops.FormatPairing(a.opts.Config.PopHomeEndpoint, token),
		pops.Pairing{TokenSHA256: pops.TokenSHA256(token), IssuedAt: &issued, ExpiresAt: &expires}, nil
}

// badPop is a 400 the operator can fix.
type badPop string

func (e badPop) Error() string { return string(e) }

func checkBudget(what string, b *popBudgetRequest) error {
	if b == nil {
		return nil
	}
	if b.MaxMbps < 0 || b.MaxMbps > maxPopMbps {
		return badPop(fmt.Sprintf("%s.maxMbps must be 0 (the default) or 1..%d", what, maxPopMbps))
	}
	if b.MaxStreams < 0 || b.MaxStreams > maxPopStreams {
		return badPop(fmt.Sprintf("%s.maxStreams must be 0 (the default) or 1..%d", what, maxPopStreams))
	}
	return nil
}

func checkLimits(l *popLimitsRequest) error {
	if l == nil {
		return nil
	}
	if l.MaxBroadcasts < 0 || l.MaxBroadcasts > maxPopBroadcasts {
		return badPop(fmt.Sprintf("limits.maxBroadcasts must be 0 (the default, %d) or 1..%d", pops.DefaultMaxBroadcasts, maxPopBroadcasts))
	}
	if l.MaxDownlinkStreams < 0 || l.MaxDownlinkStreams > maxPopStreams {
		return badPop(fmt.Sprintf("limits.maxDownlinkStreams must be 0 (the default, %d) or 1..%d", pops.DefaultMaxDownlinkStreams, maxPopStreams))
	}
	return nil
}

func (a *API) parseNotAfter(v string) (*metav1.Time, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return nil, badPop("notAfter must be an RFC 3339 time, e.g. 2026-11-09T00:00:00Z")
	}
	if !t.After(a.now()) {
		return nil, badPop("notAfter must be in the future")
	}
	m := metav1.NewTime(t.UTC())
	return &m, nil
}

func cleanDisplayName(v, fallback string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		v = fallback
	}
	if len(v) > maxPopDisplayName {
		return "", badPop(fmt.Sprintf("displayName must be at most %d characters", maxPopDisplayName))
	}
	return v, nil
}

// applyEdits validates and applies the fields create and edit share.
func (a *API) applyEdits(spec *pops.PopSpec, up, down *popBudgetRequest, lim *popLimitsRequest, guests *popGuestsRequest, named *bool) error {
	for _, c := range []error{checkBudget("uplink", up), checkBudget("downlink", down), checkLimits(lim)} {
		if c != nil {
			return c
		}
	}
	if up != nil {
		spec.Uplink = pops.Budget{MaxMbps: up.MaxMbps, MaxStreams: up.MaxStreams}
	}
	if down != nil {
		spec.Downlink = pops.Budget{MaxMbps: down.MaxMbps, MaxStreams: down.MaxStreams}
	}
	if lim != nil {
		spec.Limits = pops.Limits{MaxBroadcasts: lim.MaxBroadcasts, MaxDownlinkStreams: lim.MaxDownlinkStreams}
	}
	if guests != nil {
		if guests.AutoSwitch == nil {
			spec.Guests.AutoSwitch = nil
		} else {
			v := *guests.AutoSwitch
			spec.Guests.AutoSwitch = &v
		}
	}
	if named != nil {
		if *named && !a.opts.Config.PopNamedAddress() {
			return badPop("a named address needs -pop-dns-zone and -pop-cert-issuer on this deployment")
		}
		spec.NamedAddress.Enabled = *named
	}
	return nil
}

// wallBroadcasts is the wall room's spec.maxBroadcasts: the PoP's
// limits.maxBroadcasts (default 40), clamped to the room API's cap.
func wallBroadcasts(p *pops.Pop) int {
	n := p.EffectiveLimits().MaxBroadcasts
	if n > maxRoomBroadcastsCap {
		n = maxRoomBroadcastsCap
	}
	return n
}

// handleCreatePop creates a Pop CR and shows its pairing string once
// (docs/76 item 7). With a wall it also creates the wall's static room
// (item 33), owned by the Pop so deleting the PoP removes it; a wall code
// that is already a room fails the whole create and the Pop is removed again.
func (a *API) handleCreatePop(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	var req createPopRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := pops.ValidateName(name); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			fmt.Sprintf("name must be a DNS label: at most %d characters of [a-z0-9-], not starting or ending with '-'", pops.MaxNameLen))
		return
	}
	spec := pops.PopSpec{}
	var err error
	if spec.DisplayName, err = cleanDisplayName(req.DisplayName, name); err != nil {
		a.failPop(w, r, "create the PoP", err)
		return
	}
	if strings.TrimSpace(req.NotAfter) != "" {
		if spec.NotAfter, err = a.parseNotAfter(req.NotAfter); err != nil {
			a.failPop(w, r, "create the PoP", err)
			return
		}
	}
	named := req.NamedAddress
	if err := a.applyEdits(&spec, req.Uplink, req.Downlink, req.Limits, req.Guests, &named); err != nil {
		a.failPop(w, r, "create the PoP", err)
		return
	}
	var wall *popWallRequest
	if req.Wall != nil {
		wall = req.Wall
		code := strings.TrimSpace(wall.RoomCode)
		if code == "" {
			code = name
		}
		if a.opts.Rooms == nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "a party wall is a room: it needs rooms enabled on this deployment")
			return
		}
		if err := pops.ValidateWallCode(code); err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest,
				fmt.Sprintf("the wall code must be %d-%d characters of [A-Za-z0-9-] and must not look like a six-character broadcast code; a long or code-shaped PoP name needs an explicit wall.roomCode",
					rooms.MinCodeLen, rooms.MaxCodeLen))
			return
		}
		if wall.MaxParticipants < 0 || wall.MaxParticipants > maxWallParticipant {
			writeError(w, http.StatusBadRequest, CodeBadRequest,
				fmt.Sprintf("wall.maxParticipants must be 0 (the fleet default) or 1..%d", maxWallParticipant))
			return
		}
		wall.RoomCode = code
		spec.Wall = &pops.Wall{RoomCode: code}
	}

	pairing, record, err := a.newPairing()
	if err != nil {
		a.fail(w, r, "mint a pairing string", err)
		return
	}
	spec.Pairing = record
	obj, err := a.opts.Pops.Create(r.Context(), kube.NewPop{Name: name, Spec: spec, CreatedBy: id.Subject})
	if err != nil {
		a.failPop(w, r, "create the PoP", err)
		return
	}
	if wall != nil {
		if err := a.createWall(r.Context(), obj, wall, id.Subject); err != nil {
			// Roll the PoP back, bound to the object just created: a PoP
			// whose wall never existed would be a half-made party.
			if _, derr := a.opts.Pops.Delete(context.WithoutCancel(r.Context()), name, func(o kube.PopObject) error {
				if o.Pop.UID != obj.Pop.UID {
					return kube.ErrPopNotFound
				}
				return nil
			}); derr != nil {
				a.log.Warn("removing a PoP whose wall room failed also failed", "pop", name, "err", derr)
			}
			if errors.Is(err, kube.ErrRoomExists) {
				writeError(w, http.StatusConflict, CodeWallCodeTaken, "the wall code is already a room; choose another wall.roomCode")
				return
			}
			a.failPop(w, r, "create the wall room", err)
			return
		}
	}
	a.kick()
	// The pairing string is a credential until redeemed: it is in the
	// response and nowhere else, never in a log line or an event.
	a.log.Info("PoP created", "actor", id.Actor(), "pop", name, "wall", wall != nil)
	writeJSON(w, http.StatusCreated, popWithPairingJSON{
		Pop: a.renderPop(obj, nil, false), PairingString: pairing, ExpiresAt: rfc3339(record.ExpiresAt)})
}

func (a *API) createWall(ctx context.Context, obj kube.PopObject, wall *popWallRequest, creator string) error {
	owner, ok := kube.PopOwnerReference(obj.Pop)
	req := kube.StaticRoom{
		Code:            wall.RoomCode,
		DisplayName:     obj.Pop.Spec.DisplayName,
		MaxBroadcasts:   wallBroadcasts(&obj.Pop),
		MaxParticipants: wall.MaxParticipants,
		Pop:             obj.Name,
		CreatedBy:       creator,
	}
	if ok {
		req.Owner = &owner
	}
	_, err := a.opts.Rooms.CreateStatic(ctx, req)
	return err
}

func (a *API) handlePatchPop(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	name, ok := popName(w, r)
	if !ok {
		return
	}
	var req popPatchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	var notAfter *metav1.Time
	if req.NotAfter != nil && strings.TrimSpace(*req.NotAfter) != "" {
		var err error
		if notAfter, err = a.parseNotAfter(*req.NotAfter); err != nil {
			a.failPop(w, r, "edit the PoP", err)
			return
		}
	}
	_, after, err := a.opts.Pops.PatchSpec(r.Context(), name, func(spec *pops.PopSpec) error {
		if req.DisplayName != nil {
			v, err := cleanDisplayName(*req.DisplayName, name)
			if err != nil {
				return err
			}
			spec.DisplayName = v
		}
		if req.NotAfter != nil {
			spec.NotAfter = notAfter
		}
		return a.applyEdits(spec, req.Uplink, req.Downlink, req.Limits, req.Guests, req.NamedAddress)
	})
	if err != nil {
		a.failPop(w, r, "edit the PoP", err)
		return
	}
	a.kick()
	a.log.Info("PoP edited", "actor", id.Actor(), "pop", name)
	writeJSON(w, http.StatusOK, a.renderPop(after, a.livePop(r.Context(), name), false))
}

// handleRepairPop writes a new pairing (docs/76 item 9): the credential
// paired under the old string is refused at once, and the new string is
// shown once.
func (a *API) handleRepairPop(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	name, ok := popName(w, r)
	if !ok {
		return
	}
	pairing, record, err := a.newPairing()
	if err != nil {
		a.fail(w, r, "mint a pairing string", err)
		return
	}
	obj, err := a.opts.Pops.Repair(r.Context(), name, record)
	if err != nil {
		a.failPop(w, r, "re-pair the PoP", err)
		return
	}
	a.kick()
	a.log.Info("PoP re-paired", "actor", id.Actor(), "pop", name)
	writeJSON(w, http.StatusOK, popWithPairingJSON{
		Pop: a.renderPop(obj, nil, false), PairingString: pairing, ExpiresAt: rfc3339(record.ExpiresAt)})
}

func (a *API) handleRevokePop(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	name, ok := popName(w, r)
	if !ok {
		return
	}
	obj, err := a.opts.Pops.Revoke(r.Context(), name)
	if err != nil {
		a.failPop(w, r, "revoke the PoP", err)
		return
	}
	a.kick()
	a.log.Info("PoP revoked", "actor", id.Actor(), "pop", name)
	writeJSON(w, http.StatusOK, a.renderPop(obj, nil, false))
}

// handleDeletePop deletes the Pop CR. Its wall room carries an
// ownerReference to it and is garbage-collected with it.
func (a *API) handleDeletePop(w http.ResponseWriter, r *http.Request) {
	id, ok := a.caller(w, r)
	if !ok {
		return
	}
	name, ok := popName(w, r)
	if !ok {
		return
	}
	if _, err := a.opts.Pops.Delete(r.Context(), name, nil); err != nil {
		a.failPop(w, r, "delete the PoP", err)
		return
	}
	a.kick()
	a.log.Info("PoP deleted", "actor", id.Actor(), "pop", name)
	w.WriteHeader(http.StatusNoContent)
}

// popName reads the path's PoP name; a malformed one is a 404, since no such
// CR can exist.
func popName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if pops.ValidateName(name) != nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such PoP")
		return "", false
	}
	return name, true
}

func (a *API) failPop(w http.ResponseWriter, r *http.Request, what string, err error) {
	var bad badPop
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, CodeBadRequest, bad.Error())
	case errors.Is(err, kube.ErrPopNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such PoP")
	case errors.Is(err, kube.ErrPopExists):
		writeError(w, http.StatusConflict, CodePopExists, "a PoP with that name already exists")
	case errors.Is(err, kube.ErrPopRevoked):
		writeError(w, http.StatusConflict, CodePopRevoked, "the PoP is revoked: a new pairing string would pair nothing")
	case errors.Is(err, pops.ErrBadName):
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	case errors.Is(err, rooms.ErrInvalidCode):
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	default:
		a.fail(w, r, what, err)
	}
}
