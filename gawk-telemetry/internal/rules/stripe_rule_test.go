package rules

import (
	"strings"
	"testing"
)

// R30 (docs/35 §7): the rule's two branches call for opposite responses —
// an idle stripe should engage, an active one that still shows the shape
// means the composition failed on that path and the answer is Resilient.
func TestBurstThresholdLossDistinguishesIdleFromActiveStripe(t *testing.T) {
	rule := ruleByID(t, "burst-threshold-loss")

	idle := viewerFacts()
	idle.SetClient("stripeLargeLossPct", 3.8)
	idle.SetClient("stripeSmallLossPct", 0.0)
	idle.SetClient("stripeLargeChunks", 5000)
	idle.SetClient("stripeActive", 0)
	got := rule.Eval(idle)
	if got == nil {
		t.Fatal("idle-stripe case did not fire")
	}
	if !strings.Contains(got.Verdict, "not") || !strings.Contains(got.Action, "CAP_STRIPED_DELIVERY") {
		t.Errorf("idle branch verdict/action wrong: %q / %q", got.Verdict, got.Action)
	}

	active := viewerFacts()
	active.SetClient("stripeLargeLossPct", 3.8)
	active.SetClient("stripeSmallLossPct", 0.0)
	active.SetClient("stripeLargeChunks", 5000)
	active.SetClient("stripeActive", 3)
	got = rule.Eval(active)
	if got == nil {
		t.Fatal("active-stripe case did not fire")
	}
	if !strings.Contains(got.Action, "Resilient") {
		t.Errorf("active branch must route to Resilient mode: %q", got.Action)
	}
}

// Uniform loss takes small frames too — striping cannot help, and the rule
// must stay silent so the parity/delivery rules own the case.
func TestBurstThresholdLossSilentOnUniformLoss(t *testing.T) {
	rule := ruleByID(t, "burst-threshold-loss")
	f := viewerFacts()
	f.SetClient("stripeLargeLossPct", 3.8)
	f.SetClient("stripeSmallLossPct", 3.5)
	f.SetClient("stripeLargeChunks", 5000)
	f.SetClient("stripeActive", 0)
	if got := rule.Eval(f); got != nil {
		t.Fatalf("fired on uniform loss: %+v", got)
	}
}

func TestBurstThresholdLossSilentOnHealthyAndSparseSessions(t *testing.T) {
	rule := ruleByID(t, "burst-threshold-loss")

	clean := viewerFacts()
	clean.SetClient("stripeLargeLossPct", 0.1)
	clean.SetClient("stripeSmallLossPct", 0.0)
	clean.SetClient("stripeLargeChunks", 5000)
	if got := rule.Eval(clean); got != nil {
		t.Fatalf("fired on a clean link: %+v", got)
	}

	sparse := viewerFacts()
	sparse.SetClient("stripeLargeLossPct", 5.0)
	sparse.SetClient("stripeSmallLossPct", 0.0)
	sparse.SetClient("stripeLargeChunks", 100) // under the evidence floor
	if got := rule.Eval(sparse); got != nil {
		t.Fatalf("fired below the evidence floor: %+v", got)
	}

	// No small-frame evidence at all: the shape is unprovable, stay silent.
	unprovable := viewerFacts()
	unprovable.SetClient("stripeLargeLossPct", 5.0)
	unprovable.SetClient("stripeLargeChunks", 5000)
	if got := rule.Eval(unprovable); got != nil {
		t.Fatalf("fired without small-frame evidence: %+v", got)
	}
}

// A session predating R30 carries none of these fields and must diagnose
// without the rule rather than firing on zeros (D15: skew is permanent).
func TestBurstThresholdLossQuietWithoutStripeFields(t *testing.T) {
	rule := ruleByID(t, "burst-threshold-loss")
	if got := rule.Eval(viewerFacts()); got != nil {
		t.Fatalf("fired on a session with no stripe fields: %+v", got)
	}
}
