package popsrc

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

// chartRBAC is the relay chart's Role template. The fakes never check RBAC,
// and the kind tier runs no gawk-relay, so this test is what ties the verbs
// the status writer sends to the verbs the chart grants: 0.36.0 granted
// pops/status get/patch while every write was an UpdateStatus, and every
// pairing on a real cluster failed with 503.
const chartRBAC = "../../deploy/charts/gawk-server/templates/rbac.yaml"

// chartVerbs returns the verbs the chart's rule for resource grants.
func chartVerbs(t *testing.T, resource string) []string {
	t.Helper()
	raw, err := os.ReadFile(chartRBAC)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`resources: \["` + regexp.QuoteMeta(resource) + `"\]\s*\n\s*verbs: \[([^\]]*)\]`)
	m := re.FindAllSubmatch(raw, -1)
	if len(m) != 1 {
		t.Fatalf("%s: want exactly one rule for %q, found %d", chartRBAC, resource, len(m))
	}
	var verbs []string
	for _, v := range strings.Split(string(m[0][1]), ",") {
		verbs = append(verbs, strings.Trim(strings.TrimSpace(v), `"`))
	}
	return verbs
}

// Every request the k8s status writer makes is granted by the chart's Role.
func TestK8sStatusVerbsAreGrantedByTheChart(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tok := pops.TokenSHA256("K7M2Q-9XDA4")
	client := newStatusFake(t, pairablePop(t, "assembly-2026", tok, now.Add(time.Hour)))
	st := NewK8sStatus(client, testNamespace)
	ctx := t.Context()

	if err := st.RedeemPairing(ctx, "assembly-2026", tok, "cred", now); err != nil {
		t.Fatal(err)
	}
	gen, err := st.TakeLease(ctx, "assembly-2026", "pod-a", "10.0.0.1:4433", now)
	if err != nil {
		t.Fatal(err)
	}
	renewed := metav1.NewTime(now.Add(time.Second))
	if err := st.PatchLease(ctx, "assembly-2026", pops.Lease{Holder: "pod-a", Addr: "10.0.0.1:4433", Generation: gen, RenewedAt: &renewed}, gen); err != nil {
		t.Fatal(err)
	}
	if err := st.PatchLink(ctx, "assembly-2026", pops.Link{Connected: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPublicAddr(ctx, "assembly-2026", "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	if err := st.PatchCSR(ctx, "assembly-2026", "csr-pem"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "assembly-2026"); err != nil {
		t.Fatal(err)
	}

	granted := map[string][]string{
		"pops":        chartVerbs(t, "pops"),
		"pops/status": chartVerbs(t, "pops/status"),
	}
	seen := map[string]bool{}
	for _, a := range client.Actions() {
		res := a.GetResource().Resource
		if sub := a.GetSubresource(); sub != "" {
			res += "/" + sub
		}
		key := a.GetVerb() + " " + res
		if seen[key] {
			continue
		}
		seen[key] = true
		if !slices.Contains(granted[res], a.GetVerb()) {
			t.Errorf("the status writer sends %q, but the chart grants %s only %v", key, res, granted[res])
		}
	}
	if !seen["update pops/status"] {
		t.Errorf("no status write observed (saw %v): the test no longer exercises the writer", seen)
	}
}
