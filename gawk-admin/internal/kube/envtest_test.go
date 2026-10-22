package kube_test

// The envtest tier (docs/42 §9 AP2/AP4, §11.1's envtest row): a REAL
// kube-apiserver + etcd, closing the two gaps the client-go fakes cannot —
// Lease optimistic concurrency actually CONTENDED, and the relay chart's CRD
// schema accepting (and rejecting) real payloads. RBAC stays with the kind
// tier, which has an authorizer.
//
// Skips without KUBEBUILDER_ASSETS, the storetest pattern: `go test ./...`
// stays green on a machine without the binaries, and CI both provides them
// and fails if these tests skipped there.
//
// The CRD under test is the CHART's template, not a test copy: the template's
// few directive lines are stripped (they carry only the enabled-gate and the
// labels include — the schema itself is static), so a schema edit in the
// chart is a schema edit here.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

const (
	crdTemplate = "../../../gawk-server/deploy/charts/gawk-server/templates/crd-ban.yaml"
	// roomCRDTemplate is R42's Room CRD (docs/44 §4.3, RM3), shipped by the
	// relay chart; envtest_rooms_test.go renders it beside the Ban one.
	roomCRDTemplate = "../../../gawk-server/deploy/charts/gawk-server/templates/crd-room.yaml"
	// popCRDTemplate is R74's Pop CRD (docs/76 §9, PP6), shipped by the
	// relay chart beside the Room one.
	popCRDTemplate = "../../../gawk-server/deploy/charts/gawk-server/templates/crd-pop.yaml"
)

// renderedCRD turns the chart's templated CRD into plain YAML. The template's
// only directives are the enabled-gate and the labels include, so stripping
// every line carrying `{{` yields the exact schema the chart installs.
func renderedCRD(t *testing.T, templates ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tmpl := range templates {
		raw, err := os.ReadFile(tmpl)
		if err != nil {
			t.Fatalf("read the chart's CRD template: %v", err)
		}
		var out []string
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "{{") {
				continue
			}
			out = append(out, line)
		}
		path := filepath.Join(dir, filepath.Base(tmpl))
		if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o600); err != nil {
			t.Fatalf("write rendered CRD: %v", err)
		}
	}
	return dir
}

// startEnv boots one apiserver+etcd with the chart's CRD(s) installed and a
// namespace to work in. The Ban CRD is always installed; extra templates are
// rendered beside it.
func startEnv(t *testing.T, extraCRDs ...string) (*rest.Config, string) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set: skipping the envtest-backed test (run `setup-envtest use -p path` and export it)")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{renderedCRD(t, append([]string{crdTemplate}, extraCRDs...)...)}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	const ns = "gawk-envtest"
	if _, err := cs.CoreV1().Namespaces().Create(t.Context(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return cfg, ns
}

// TestEnvtestCRDAcceptsTheReconcilersRealPayload is the gap §11.2 named: no
// real API server anywhere automated had validated a CRClient write against
// the chart's OpenAPI schema — a payload the schema rejects would silently
// stop projecting every ban while the fleet enforces stale state.
func TestEnvtestCRDAcceptsTheReconcilersRealPayload(t *testing.T) {
	cfg, ns := startEnv(t)
	ctx := t.Context()

	crs, err := kube.NewCRClient(cfg, ns)
	if err != nil {
		t.Fatalf("NewCRClient: %v", err)
	}

	// The two production shapes, built the way the API builds them: through
	// store.Ban's Record with the shared normalization — an ID kill ban with
	// an expiry, and a permanent CIDR ban.
	expiry := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	idBan := store.Ban{
		Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC234"},
		Reason: "terms violation", CreatedBy: "op@example.com", ExpiresAt: &expiry,
	}
	ipBan := store.Ban{
		Target: moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.0/24"},
		Reason: "ban evasion", CreatedBy: "op@example.com",
	}
	for _, b := range []store.Ban{idBan, ipBan} {
		rec, err := moderation.Normalize(b.Record())
		if err != nil {
			t.Fatalf("Normalize(%v): %v", b.Target, err)
		}
		if err := crs.Upsert(ctx, rec, "11111111-2222-3333-4444-555555555555"); err != nil {
			t.Fatalf("the real API server rejected the reconciler's payload for %v: %v", b.Target, err)
		}
		// Upsert twice: the update path goes through the schema too.
		if err := crs.Upsert(ctx, rec, "11111111-2222-3333-4444-555555555555"); err != nil {
			t.Fatalf("the update path was rejected for %v: %v", b.Target, err)
		}
	}

	objs, err := crs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("listed %d CRs, want 2: %+v", len(objs), objs)
	}
	for _, o := range objs {
		if o.Err != nil {
			t.Fatalf("a round-tripped CR did not parse back: %s: %v", o.Name, o.Err)
		}
	}

	// And the schema is genuinely ON: an unknown target.type must be
	// enum-rejected, not silently stored — the fail-closed half of §4.2's
	// additive-only contract.
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	bad := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": moderation.SchemeGroupVersion.String(),
		"kind":       moderation.Kind,
		"metadata":   map[string]any{"name": "ban-bad-type"},
		"spec": map[string]any{
			"target": map[string]any{"type": "userId", "value": "whoever"},
		},
	}}
	_, err = dc.Resource(moderation.GroupVersionResource).Namespace(ns).Create(ctx, bad, metav1.CreateOptions{})
	if err == nil {
		t.Fatal("the schema accepted an unknown target.type — OpenAPI validation is not in force")
	}
	if !strings.Contains(err.Error(), "supported values") && !strings.Contains(err.Error(), "Unsupported value") {
		t.Fatalf("unknown target.type failed for the wrong reason: %v", err)
	}
}

// TestEnvtestLeaseContentionElectsExactlyOneLeader is the other §11.2 gap:
// the fake clientset's object tracker enforces no resourceVersion conflicts,
// so until this test the mutual-exclusion property rested on client-go's
// election being *called* correctly, never on Lease CAS being *contended* —
// and a double leader double-sends webhooks.
func TestEnvtestLeaseContentionElectsExactlyOneLeader(t *testing.T) {
	cfg, ns := startEnv(t)

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	// Short timings so a handover fits a test; they keep the library's
	// RetryPeriod*1.2 < RenewDeadline < LeaseDuration constraint.
	var leadingA, leadingB atomic.Bool
	mk := func(id string, flag *atomic.Bool) *kube.Election {
		e, err := kube.NewElection(kube.LeaderOptions{
			Client: cs, Namespace: ns, Identity: id,
			OnLeading: func(ctx context.Context) {
				flag.Store(true)
				<-ctx.Done()
				flag.Store(false)
			},
			// A clean handover, so the test does not wait out the TTL.
			ReleaseOnCancel: true,
			LeaseDuration:   3 * time.Second,
			RenewDeadline:   2 * time.Second,
			RetryPeriod:     500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewElection(%s): %v", id, err)
		}
		return e
	}
	a, b := mk("replica-a", &leadingA), mk("replica-b", &leadingB)

	ctxA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(t.Context())
	defer cancelB()
	go a.Run(ctxA)
	go b.Run(ctxB)

	waitFor(t, "one candidate to lead", func() bool { return leadingA.Load() || leadingB.Load() })
	// Both campaigned against the SAME Lease on a real etcd; give the loser
	// several retry periods to (wrongly) win before asserting exclusion.
	time.Sleep(1500 * time.Millisecond)
	if leadingA.Load() && leadingB.Load() {
		t.Fatal("both candidates lead at once — Lease CAS did not exclude")
	}

	// Kill the leader; the other must take over.
	first := &leadingA
	firstCancel, second := cancelA, &leadingB
	if leadingB.Load() {
		first, second = &leadingB, &leadingA
		firstCancel = cancelB
	}
	firstCancel()
	waitFor(t, "the survivor to take the lease", func() bool { return second.Load() })
	if first.Load() {
		t.Fatal("the cancelled leader still reports leading")
	}
}

// TestEnvtestPopCRDKeepsEveryField is PP6's schema check: the relay chart's
// Pop CRD on a real API server keeps every spec and status field the
// documented shape uses (docs/76 §9) — a structural schema PRUNES what it
// does not declare, silently — keeps status writes off the main resource
// (the status subresource is what lets the two status writers and the spec
// writer never clobber each other), and refuses a name that cannot be the
// gawk/pop label value.
func TestEnvtestPopCRDKeepsEveryField(t *testing.T) {
	cfg, ns := startEnv(t, popCRDTemplate)
	ctx := t.Context()
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	ri := dc.Resource(pops.GroupVersionResource).Namespace(ns)

	at := func(s string) *metav1.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		m := metav1.NewTime(tm)
		return &m
	}
	on := true
	tokSHA := pops.TokenSHA256("AAAQE-AYEAU-DAOCA-JBIFQ-YDIOB-4")
	want := pops.Pop{
		TypeMeta:   metav1.TypeMeta{APIVersion: pops.SchemeGroupVersion.String(), Kind: pops.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "assembly-2026"},
		Spec: pops.PopSpec{
			DisplayName:  "Assembly 2026",
			NotAfter:     at("2026-11-09T00:00:00Z"),
			Revoked:      true,
			Pairing:      pops.Pairing{TokenSHA256: tokSHA, IssuedAt: at("2026-10-21T18:00:00Z"), ExpiresAt: at("2026-10-24T18:00:00Z")},
			Uplink:       pops.Budget{MaxMbps: 25, MaxStreams: 3},
			Downlink:     pops.Budget{MaxMbps: 100, MaxStreams: 8},
			Limits:       pops.Limits{MaxBroadcasts: 40, MaxDownlinkStreams: 20},
			Guests:       pops.Guests{AutoSwitch: &on},
			Wall:         &pops.Wall{RoomCode: "assembly-2026"},
			NamedAddress: pops.NamedAddress{Enabled: true},
		},
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&want)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: raw}
	delete(u.Object, "status")
	created, err := ri.Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("the real API server rejected the documented Pop: %v", err)
	}

	// Status goes through the subresource, as both writers patch it.
	status := pops.PopStatus{
		Credential: &pops.Credential{SHA256: pops.CredentialSHA256("c"), PairingTokenSHA256: tokSHA, PairedAt: at("2026-10-21T18:04:11Z")},
		Lease:      &pops.Lease{Holder: "gawk-server-7c9f", Addr: "10.42.0.17:4433", Generation: 4, RenewedAt: at("2026-10-21T18:05:00Z")},
		Link: &pops.Link{Connected: true, Since: at("2026-10-21T18:04:12Z"), Version: "0.44.0", Proto: 1,
			PublicAddrs: []string{"203.0.113.7", "2001:db8:1:2::/64"}, LanAddr: "192.168.1.20", PinnedHashes: []string{"h1", "h2"}},
		TLS: &pops.TLS{Host: "p-7k2m9x.pop.gawk.ioio.fi", CSR: "CSR", Certificate: "CERT", IssuedForCSR: "dd", NotAfter: at("2026-10-28T00:00:00Z")},
	}
	patch, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ri.Patch(ctx, created.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}, "status"); err != nil {
		t.Fatalf("status merge patch rejected: %v", err)
	}

	gotU, err := ri.Get(ctx, "assembly-2026", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got pops.Pop
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(gotU.Object, &got); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(t, got.Spec, want.Spec) {
		t.Errorf("the schema pruned part of the spec:\n got %+v\nwant %+v", got.Spec, want.Spec)
	}
	if !equalJSON(t, got.Status, status) {
		t.Errorf("the schema pruned part of the status:\n got %+v\nwant %+v", got.Status, status)
	}

	// A status written through the main resource is ignored.
	gotU.Object["status"] = map[string]any{"credential": map[string]any{"sha256": "forged"}}
	updated, err := ri.Update(ctx, gotU, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("main-resource update: %v", err)
	}
	if sha, _, _ := unstructured.NestedString(updated.Object, "status", "credential", "sha256"); sha != status.Credential.SHA256 {
		t.Errorf("a main-resource write changed status.credential to %q: the status subresource is not in force", sha)
	}

	// The schema is ON: a malformed pairing hash, and a name with a dot (a
	// valid CR name, never a label value), are refused.
	create := func(name string, spec map[string]any) error {
		obj := map[string]any{
			"apiVersion": pops.SchemeGroupVersion.String(), "kind": pops.Kind,
			"metadata": map[string]any{"name": name},
			"spec":     spec,
		}
		_, err := ri.Create(ctx, &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{})
		return err
	}
	if err := create("bad-hash", map[string]any{"displayName": "x", "pairing": map[string]any{"tokenSHA256": "NOT-HEX"}}); err == nil {
		t.Error("the schema accepted a malformed pairing hash")
	}
	if err := create("a.b", map[string]any{"displayName": "x"}); err == nil {
		t.Error("the schema accepted a Pop name that cannot be a label value")
	}
}

// TestEnvtestRoomCRDKeepsPopAndMaxParticipants: R74's two new Room fields
// survive the relay chart's Room schema (docs/76 item 33). Without them in
// the schema, a party wall would silently lose spec.pop, and with it the
// vouched-only attach rule.
func TestEnvtestRoomCRDKeepsPopAndMaxParticipants(t *testing.T) {
	cfg, ns := startEnv(t, roomCRDTemplate)
	ctx := t.Context()
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	r := rooms.Room{
		TypeMeta:   metav1.TypeMeta{APIVersion: rooms.SchemeGroupVersion.String(), Kind: rooms.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "assembly-2026"},
		Spec: rooms.RoomSpec{Kind: rooms.KindStatic, DisplayCode: "assembly-2026", Pop: "assembly-2026",
			MaxBroadcasts: 40, MaxParticipants: 200},
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&r)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: raw}
	delete(u.Object, "status")
	ri := dc.Resource(rooms.GroupVersionResource).Namespace(ns)
	if _, err := ri.Create(ctx, u, metav1.CreateOptions{}); err != nil {
		t.Fatalf("the Room schema rejected a party wall: %v", err)
	}
	got, err := ri.Get(ctx, "assembly-2026", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pop, _, _ := unstructured.NestedString(got.Object, "spec", "pop")
	maxP, _, _ := unstructured.NestedInt64(got.Object, "spec", "maxParticipants")
	if pop != "assembly-2026" || maxP != 200 {
		t.Fatalf("the Room schema pruned spec.pop=%q / spec.maxParticipants=%d", pop, maxP)
	}
	u2 := u.DeepCopy()
	u2.SetName("zero-cap")
	if err := unstructured.SetNestedField(u2.Object, int64(0), "spec", "maxParticipants"); err != nil {
		t.Fatal(err)
	}
	if _, err := ri.Create(ctx, u2, metav1.CreateOptions{}); err == nil {
		t.Error("the Room schema accepted maxParticipants 0")
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(ja) == string(jb)
}
