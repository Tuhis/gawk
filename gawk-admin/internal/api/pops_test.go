package api_test

// R74 (docs/76 §6.2, §6.10, §7.2): the PoP routes. They run over the real
// kube.PopClient on client-go's dynamic fake, so what "only the hash is
// stored" means is checked against the object the API server would hold.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

const popNS = "gawk"

var popNow = time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)

// popEnv is a PoP-enabled harness: the real PopClient over the dynamic fake,
// a fixed clock, and the rooms double recording what a wall asked for.
type popEnv struct {
	*harness
	dc    *dynamicfake.FakeDynamicClient
	pops  *kube.PopClient
	rooms *wallRooms
	now   time.Time
}

// wallRooms is fakeRooms plus a record of every StaticRoom request, so a
// test can see the wall's spec.pop, caps and ownerReference.
type wallRooms struct {
	*fakeRooms
	mu   sync.Mutex
	reqs []kube.StaticRoom
}

func (w *wallRooms) CreateStatic(ctx context.Context, req kube.StaticRoom) (string, error) {
	w.mu.Lock()
	w.reqs = append(w.reqs, req)
	w.mu.Unlock()
	return w.fakeRooms.CreateStatic(ctx, req)
}

func newPopDynamic(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := pops.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	dc := dynamicfake.NewSimpleDynamicClient(scheme, objs...)
	// The fake assigns no UID or creation time; an API server always does.
	n := 0
	dc.PrependReactor("create", "pops", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		n++
		u.SetUID(types.UID("uid-" + u.GetName() + "-" + string(rune('0'+n))))
		u.SetCreationTimestamp(metav1.NewTime(popNow))
		return false, nil, nil
	})
	return dc
}

func withPops(pc api.Pops) harnessOption {
	return func(o *api.Options, _ *harness) {
		o.Pops = pc
		o.Config.Pops = true
		o.Config.PopHomeEndpoint = "api.gawk.ioio.fi:4433"
		o.Config.PopPairingTTL = 72 * time.Hour
		o.Config.PopDNSTTL = time.Minute
	}
}

// withPopsEnabled registers the PoP routes with a client no handler reaches,
// for the tests that only ask whether a route exists and what guards it.
func withPopsEnabled() harnessOption {
	return func(o *api.Options, h *harness) {
		withPops(kube.NewPopClientFor(newPopDynamic(h.t), popNS))(o, h)
	}
}

func newPopEnv(t *testing.T, opts ...harnessOption) *popEnv {
	t.Helper()
	dc := newPopDynamic(t)
	e := &popEnv{dc: dc, pops: kube.NewPopClientFor(dc, popNS), rooms: &wallRooms{fakeRooms: newFakeRooms()}, now: popNow}
	all := append([]harnessOption{
		withPops(e.pops),
		withRooms(e.rooms.fakeRooms, &memoryRecorder{}),
		func(o *api.Options, _ *harness) {
			o.Rooms = e.rooms
			o.Clock = func() time.Time { return e.now }
		},
	}, opts...)
	e.harness = newHarnessWithoutPostgres(t, all...)
	return e
}

func (e *popEnv) stored(name string) *unstructured.Unstructured {
	e.t.Helper()
	u, err := e.dc.Resource(pops.GroupVersionResource).Namespace(popNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("get %s: %v", name, err)
	}
	return u
}

func (e *popEnv) storedPop(name string) pops.Pop {
	e.t.Helper()
	var p pops.Pop
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(e.stored(name).Object, &p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// setStatus writes status the way a relay would, through the dynamic fake.
func (e *popEnv) setStatus(name string, st pops.PopStatus) {
	e.t.Helper()
	u := e.stored(name)
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&st)
	if err != nil {
		e.t.Fatal(err)
	}
	u.Object["status"] = raw
	if _, err := e.dc.Resource(pops.GroupVersionResource).Namespace(popNS).Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

// paired marks a Pop paired with its current string, as a relay redeeming it
// would.
func (e *popEnv) paired(name string, link *pops.Link) {
	e.t.Helper()
	p := e.storedPop(name)
	at := metav1.NewTime(e.now)
	e.setStatus(name, pops.PopStatus{
		Credential: &pops.Credential{SHA256: strings.Repeat("c", 64), PairingTokenSHA256: p.Spec.Pairing.TokenSHA256, PairedAt: &at},
		Link:       link,
	})
}

type wirePop struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	State       string `json:"state"`
	StateSince  string `json:"stateSince"`
	NotAfter    string `json:"notAfter"`
	Revoked     bool   `json:"revoked"`
	Pairing     struct {
		ExpiresAt string `json:"expiresAt"`
		Paired    bool   `json:"paired"`
	} `json:"pairing"`
	Uplink struct {
		MaxMbps    int `json:"maxMbps"`
		MaxStreams int `json:"maxStreams"`
	} `json:"uplink"`
	Limits struct {
		MaxBroadcasts int `json:"maxBroadcasts"`
	} `json:"limits"`
	Guests struct {
		AutoSwitch bool `json:"autoSwitch"`
	} `json:"guests"`
	Wall *struct {
		RoomCode string `json:"roomCode"`
	} `json:"wall"`
	Link *struct {
		Connected   bool     `json:"connected"`
		LanAddr     string   `json:"lanAddr"`
		PublicAddrs []string `json:"publicAddrs"`
		Pod         string   `json:"pod"`
	} `json:"link"`
	Live *struct {
		Pod            string `json:"pod"`
		Live           int    `json:"live"`
		Forwarded      int    `json:"forwarded"`
		OutsideViewers int    `json:"outsideViewers"`
		OffersSent     int    `json:"offersSent"`
	} `json:"live"`
	Broadcasts *[]struct {
		ID        string `json:"id"`
		Forwarded bool   `json:"forwarded"`
		Links     *struct {
			Watch string `json:"watch"`
		} `json:"links"`
	} `json:"broadcasts"`
	Health *[]struct {
		Key string `json:"key"`
		OK  bool   `json:"ok"`
	} `json:"health"`
}

type wirePopWithPairing struct {
	Pop           wirePop `json:"pop"`
	PairingString string  `json:"pairingString"`
	ExpiresAt     string  `json:"expiresAt"`
}

// PP6: the pairing string is shown once and only its hash is stored.
func TestCreatePopShowsThePairingStringOnceAndStoresOnlyItsHash(t *testing.T) {
	e := newPopEnv(t)
	var out wirePopWithPairing
	e.decode(http.MethodPost, "/api/v1/pops", map[string]any{
		"name": "assembly-2026", "displayName": "Assembly 2026",
		"uplink": map[string]any{"maxMbps": 30, "maxStreams": 4},
	}, http.StatusCreated, &out)

	endpoint, token, err := pops.ParsePairing(out.PairingString)
	if err != nil || endpoint != "api.gawk.ioio.fi:4433" {
		t.Fatalf("pairing string %q: endpoint %q, err %v", out.PairingString, endpoint, err)
	}
	if out.ExpiresAt != popNow.Add(72*time.Hour).Format(time.RFC3339) {
		t.Fatalf("expiresAt = %q, want now + -pop-pairing-ttl", out.ExpiresAt)
	}
	if out.Pop.State != api.PopStateWaiting || out.Pop.Pairing.Paired || out.Pop.Uplink.MaxMbps != 30 {
		t.Fatalf("created pop = %+v", out.Pop)
	}

	stored := e.storedPop("assembly-2026")
	if stored.Spec.Pairing.TokenSHA256 != pops.TokenSHA256(token) {
		t.Fatalf("stored hash %q, want sha256 of the shown token", stored.Spec.Pairing.TokenSHA256)
	}
	raw, _ := json.Marshal(e.stored("assembly-2026").Object)
	bare := strings.ReplaceAll(token, "-", "")
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), bare) {
		t.Fatalf("the pairing token is in the stored CR: %s", raw)
	}
	if strings.Contains(e.logText(), bare) || strings.Contains(e.logText(), token) {
		t.Fatalf("the pairing token was logged: %s", e.logText())
	}

	// Shown once: no read ever carries it again.
	for _, path := range []string{"/api/v1/pops", "/api/v1/pops/assembly-2026"} {
		status, body := e.raw(http.MethodGet, path, nil)
		if status != http.StatusOK || strings.Contains(body, bare) || strings.Contains(body, token) || strings.Contains(body, "pairingString") {
			t.Fatalf("GET %s = %d, carries the pairing string: %s", path, status, body)
		}
	}

	// The name is the reservation.
	if code := e.errorCode(http.MethodPost, "/api/v1/pops", map[string]any{"name": "assembly-2026"}, http.StatusConflict); code != api.CodePopExists {
		t.Fatalf("second create = %q, want %q", code, api.CodePopExists)
	}
}

func TestRepairShowsANewStringAndKillsTheOldCredential(t *testing.T) {
	e := newPopEnv(t)
	var first, second, third wirePopWithPairing
	e.decode(http.MethodPost, "/api/v1/pops", map[string]any{"name": "lanparty"}, http.StatusCreated, &first)
	e.paired("lanparty", &pops.Link{Connected: true})
	var before wirePop
	e.decode(http.MethodGet, "/api/v1/pops/lanparty", nil, http.StatusOK, &before)
	if before.State != api.PopStateLinked || !before.Pairing.Paired {
		t.Fatalf("paired pop = %+v, want linked", before)
	}

	// Two Re-pairs in the same second still differ (docs/76 item 9).
	e.decode(http.MethodPost, "/api/v1/pops/lanparty/repair", nil, http.StatusOK, &second)
	e.decode(http.MethodPost, "/api/v1/pops/lanparty/repair", nil, http.StatusOK, &third)
	if second.PairingString == first.PairingString || third.PairingString == second.PairingString {
		t.Fatalf("re-pair reused a string: %q %q %q", first.PairingString, second.PairingString, third.PairingString)
	}
	_, tok, _ := pops.ParsePairing(third.PairingString)
	p := e.storedPop("lanparty")
	if p.Spec.Pairing.TokenSHA256 != pops.TokenSHA256(tok) {
		t.Fatal("re-pair did not store the new string's hash")
	}
	// The relays' status is untouched and the old credential is no longer
	// current, so the card says the PoP needs pairing again.
	if p.Status.Credential == nil || p.CredentialCurrent() {
		t.Fatalf("credential after re-pair = %+v, want kept but not current", p.Status.Credential)
	}
	if third.Pop.State != api.PopStateWaiting {
		t.Fatalf("state after re-pair = %q, want %q", third.Pop.State, api.PopStateWaiting)
	}

	e.decode(http.MethodPost, "/api/v1/pops/lanparty/revoke", nil, http.StatusOK, nil)
	if code := e.errorCode(http.MethodPost, "/api/v1/pops/lanparty/repair", nil, http.StatusConflict); code != api.CodePopRevoked {
		t.Fatalf("re-pair of a revoked pop = %q, want %q", code, api.CodePopRevoked)
	}
}

func TestCreatePopRefusesWhatCouldNeverWork(t *testing.T) {
	e := newPopEnv(t)
	for name, body := range map[string]map[string]any{
		"a name that is no DNS label":    {"name": "Assembly_2026"},
		"a name over 63":                 {"name": strings.Repeat("a", 64)},
		"an end date in the past":        {"name": "p1", "notAfter": "2026-10-20T00:00:00Z"},
		"an end date that is not a time": {"name": "p1", "notAfter": "next friday"},
		"a negative budget":              {"name": "p1", "uplink": map[string]any{"maxMbps": -1}},
		"a silly stream count":           {"name": "p1", "uplink": map[string]any{"maxStreams": 1000}},
		"a named address unconfigured":   {"name": "p1", "namedAddress": true},
		"a dynamic-shaped wall code":     {"name": "p1", "wall": map[string]any{"roomCode": "abcdef"}},
		"a wall code too long":           {"name": strings.Repeat("a", 40), "wall": map[string]any{}},
		"a display name over 64":         {"name": "p1", "displayName": strings.Repeat("x", 65)},
	} {
		if code := e.errorCode(http.MethodPost, "/api/v1/pops", body, http.StatusBadRequest); code != api.CodeBadRequest {
			t.Errorf("%s: code %q", name, code)
		}
	}
	if list, _ := e.pops.List(context.Background()); len(list) != 0 {
		t.Fatalf("a refused create left %d PoPs behind", len(list))
	}
}

func TestANamedAddressNeedsTheNamedTier(t *testing.T) {
	e := newPopEnv(t, withConfig(func(c *config.Config) {
		c.PopDNSZone, c.PopCertIssuer = "pop.gawk.ioio.fi", "ClusterIssuer/le"
	}))
	e.decode(http.MethodPost, "/api/v1/pops", map[string]any{"name": "named", "namedAddress": true}, http.StatusCreated, nil)
	if !e.storedPop("named").Spec.NamedAddress.Enabled {
		t.Fatal("namedAddress not stored")
	}
	var me struct {
		Features struct {
			Pops            bool `json:"pops"`
			PopNamedAddress bool `json:"popNamedAddress"`
		} `json:"features"`
	}
	e.decode(http.MethodGet, "/api/v1/me", nil, http.StatusOK, &me)
	if !me.Features.Pops || !me.Features.PopNamedAddress {
		t.Fatalf("me.features = %+v", me.Features)
	}
}

// PP11: the wall is a static room with spec.pop, the copied broadcast cap,
// no attach secret, owned by the Pop.
func TestCreatePopWithAWallCreatesItsRoomOwnedByThePop(t *testing.T) {
	e := newPopEnv(t)
	var out wirePopWithPairing
	e.decode(http.MethodPost, "/api/v1/pops", map[string]any{
		"name": "assembly-2026", "displayName": "Assembly 2026",
		"limits": map[string]any{"maxBroadcasts": 100},
		"wall":   map[string]any{"maxParticipants": 300},
	}, http.StatusCreated, &out)
	if out.Pop.Wall == nil || out.Pop.Wall.RoomCode != "assembly-2026" {
		t.Fatalf("wall = %+v, want the name as the default code", out.Pop.Wall)
	}
	if len(e.rooms.reqs) != 1 {
		t.Fatalf("room creates = %d", len(e.rooms.reqs))
	}
	req := e.rooms.reqs[0]
	uid := e.storedPop("assembly-2026").UID
	if req.Pop != "assembly-2026" || req.Code != "assembly-2026" || req.WithAttachSecret || req.MaxParticipants != 300 ||
		req.MaxBroadcasts != 64 || req.Owner == nil || req.Owner.UID != uid || req.Owner.Kind != pops.Kind || req.DisplayName != "Assembly 2026" {
		t.Fatalf("wall room request = %+v (owner %+v), want spec.pop, 64 (clamped), no secret, owned by uid %q", req, req.Owner, uid)
	}

	// The default 40 is copied as is.
	e.decode(http.MethodPost, "/api/v1/pops", map[string]any{"name": "lanparty", "wall": map[string]any{"roomCode": "LAN-Wall"}}, http.StatusCreated, nil)
	if got := e.rooms.reqs[1]; got.MaxBroadcasts != pops.DefaultMaxBroadcasts || got.Code != "LAN-Wall" {
		t.Fatalf("second wall = %+v", got)
	}
}

func TestAWallCodeThatIsTakenFailsTheCreateAndLeavesNoPop(t *testing.T) {
	e := newPopEnv(t)
	if _, err := e.rooms.fakeRooms.CreateStatic(context.Background(), kube.StaticRoom{Code: "taken"}); err != nil {
		t.Fatal(err)
	}
	if code := e.errorCode(http.MethodPost, "/api/v1/pops", map[string]any{"name": "p1", "wall": map[string]any{"roomCode": "taken"}},
		http.StatusConflict); code != api.CodeWallCodeTaken {
		t.Fatalf("code = %q, want %q", code, api.CodeWallCodeTaken)
	}
	if _, err := e.pops.Get(context.Background(), "p1"); err != kube.ErrPopNotFound {
		t.Fatalf("the PoP outlived its failed wall: %v", err)
	}
}

func TestAWallNeedsRooms(t *testing.T) {
	e := newPopEnv(t, func(o *api.Options, _ *harness) { o.Rooms = nil })
	if code := e.errorCode(http.MethodPost, "/api/v1/pops", map[string]any{"name": "p1", "wall": map[string]any{}}, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("code = %q", code)
	}
}

// PP7: the cards merge the CR with the link-home pod's live view.
func TestListAndDetailMergeTheLiveView(t *testing.T) {
	e := newPopEnv(t)
	for _, n := range []string{"linked", "offline", "waiting", "revoked", "ended"} {
		body := map[string]any{"name": n}
		if n == "ended" {
			body["notAfter"] = popNow.Add(time.Hour).Format(time.RFC3339)
		}
		e.decode(http.MethodPost, "/api/v1/pops", body, http.StatusCreated, nil)
	}
	since := metav1.NewTime(popNow.Add(-3 * time.Hour))
	e.paired("linked", &pops.Link{Connected: true, Since: &since})
	e.paired("offline", &pops.Link{Connected: false, Since: &since, LanAddr: "192.168.1.30"})
	e.paired("ended", nil)
	e.decode(http.MethodPost, "/api/v1/pops/revoked/revoke", nil, http.StatusOK, nil)

	e.fleet.set(relayscan.Snapshot{Pops: []relayscan.PopAggregate{{Pod: "gawk-server-1", PopState: relayscan.PopState{
		Name: "linked", Linked: true, Live: 7, Forwarded: 2, OutsideViewers: 11, OffersSent: 4,
		PublicAddrs: []string{"203.0.113.7"}, LanAddr: "192.168.1.20",
		Broadcasts: []relayscan.PopBroadcast{{ID: "ABC234", Label: "main pc", Forwarded: true, OutsideViewers: 11}},
		Health:     []relayscan.HealthLine{{Key: "firewall", OK: true}},
	}}}})
	e.now = popNow.Add(2 * time.Hour) // "ended" is past its notAfter now

	var page struct {
		Pops []wirePop `json:"pops"`
	}
	e.decode(http.MethodGet, "/api/v1/pops", nil, http.StatusOK, &page)
	states := map[string]string{}
	for _, p := range page.Pops {
		states[p.Name] = p.State
		if p.Broadcasts != nil || p.Health != nil {
			t.Errorf("list row %s carries the detail projection", p.Name)
		}
	}
	want := map[string]string{"linked": api.PopStateLinked, "offline": api.PopStateOffline, "waiting": api.PopStateWaiting,
		"revoked": api.PopStateRevoked, "ended": api.PopStateEnded}
	for n, s := range want {
		if states[n] != s {
			t.Errorf("state of %s = %q, want %q", n, states[n], s)
		}
	}

	var d wirePop
	e.decode(http.MethodGet, "/api/v1/pops/linked", nil, http.StatusOK, &d)
	if d.Live == nil || d.Live.Pod != "gawk-server-1" || d.Live.Live != 7 || d.Live.Forwarded != 2 || d.Live.OffersSent != 4 {
		t.Fatalf("live = %+v", d.Live)
	}
	if d.Link == nil || !d.Link.Connected || d.Link.LanAddr != "192.168.1.20" || len(d.Link.PublicAddrs) != 1 {
		t.Fatalf("link = %+v", d.Link)
	}
	if d.Broadcasts == nil || len(*d.Broadcasts) != 1 || (*d.Broadcasts)[0].ID != "ABC234" || !(*d.Broadcasts)[0].Forwarded ||
		(*d.Broadcasts)[0].Links == nil || (*d.Broadcasts)[0].Links.Watch != "https://gawk.example/#/view/ABC234" {
		t.Fatalf("broadcasts = %+v", d.Broadcasts)
	}
	if d.Health == nil || len(*d.Health) != 1 || (*d.Health)[0].Key != "firewall" {
		t.Fatalf("health = %+v", d.Health)
	}
	var off wirePop
	e.decode(http.MethodGet, "/api/v1/pops/offline", nil, http.StatusOK, &off)
	if off.Live != nil || off.Broadcasts == nil || len(*off.Broadcasts) != 0 || off.StateSince != since.UTC().Format(time.RFC3339) {
		t.Fatalf("offline detail = %+v", off)
	}
	if code := e.errorCode(http.MethodGet, "/api/v1/pops/nosuch", nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("missing pop = %q", code)
	}
}

func TestPatchPopEditsTheBudgetAndNeverThePairing(t *testing.T) {
	e := newPopEnv(t)
	e.decode(http.MethodPost, "/api/v1/pops", map[string]any{"name": "p1", "notAfter": popNow.Add(48 * time.Hour).Format(time.RFC3339),
		"guests": map[string]any{"autoSwitch": false}}, http.StatusCreated, nil)
	before := e.storedPop("p1")

	var out wirePop
	e.decode(http.MethodPatch, "/api/v1/pops/p1", map[string]any{
		"displayName": "Party", "notAfter": "", "uplink": map[string]any{"maxMbps": 50},
		"guests": map[string]any{"autoSwitch": true},
	}, http.StatusOK, &out)
	after := e.storedPop("p1")
	if after.Spec.DisplayName != "Party" || after.Spec.NotAfter != nil || after.Spec.Uplink.MaxMbps != 50 || !after.AutoSwitch() {
		t.Fatalf("spec after edit = %+v", after.Spec)
	}
	if after.Spec.Pairing.TokenSHA256 != before.Spec.Pairing.TokenSHA256 {
		t.Fatal("an edit rewrote the pairing")
	}
	if out.DisplayName != "Party" || out.NotAfter != "" || out.Uplink.MaxMbps != 50 || out.Uplink.MaxStreams != pops.DefaultUplinkStreams {
		t.Fatalf("edit answer = %+v", out)
	}
	if code := e.errorCode(http.MethodPatch, "/api/v1/pops/p1", map[string]any{"uplink": map[string]any{"maxMbps": -5}}, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("bad edit = %q", code)
	}
	if code := e.errorCode(http.MethodPatch, "/api/v1/pops/p1", map[string]any{"namedAddress": true}, http.StatusBadRequest); code != api.CodeBadRequest {
		t.Fatalf("named edit without the tier = %q", code)
	}
}

func TestRevokeAndDelete(t *testing.T) {
	e := newPopEnv(t)
	e.decode(http.MethodPost, "/api/v1/pops", map[string]any{"name": "p1"}, http.StatusCreated, nil)
	var out wirePop
	e.decode(http.MethodPost, "/api/v1/pops/p1/revoke", nil, http.StatusOK, &out)
	if !out.Revoked || out.State != api.PopStateRevoked || !e.storedPop("p1").Spec.Revoked {
		t.Fatalf("revoke = %+v", out)
	}
	e.decode(http.MethodPost, "/api/v1/pops/p1/revoke", nil, http.StatusOK, nil) // idempotent
	e.decode(http.MethodDelete, "/api/v1/pops/p1", nil, http.StatusNoContent, nil)
	if code := e.errorCode(http.MethodDelete, "/api/v1/pops/p1", nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("second delete = %q", code)
	}
}

func TestPopRoutesAreAbsentWithPopsOff(t *testing.T) {
	h := newHarnessWithoutPostgres(t)
	for _, r := range api.RouteTable() {
		if r.Requires != api.RequiresPops {
			continue
		}
		if status, _ := h.raw(r.Method, concretePath(r.Pattern), nil); status != http.StatusNotFound {
			t.Errorf("%s %s with pops off = %d, want 404", r.Method, r.Pattern, status)
		}
	}
	var me struct {
		Features map[string]bool `json:"features"`
	}
	h.decode(http.MethodGet, "/api/v1/me", nil, http.StatusOK, &me)
	if me.Features["pops"] || me.Features["popNamedAddress"] {
		t.Fatalf("features with pops off = %+v", me.Features)
	}
}
