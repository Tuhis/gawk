package rules

import (
	"strings"
	"testing"
)

// R29 (docs/34 §7.3): the rule's whole value is that it distinguishes two
// causes needing OPPOSITE responses. A single verdict covering both would send
// operators to raise the parity level against bursty loss, which no per-frame
// code covers at any level.
func TestParityIneffectiveDistinguishesBurstFromUnderProvisioning(t *testing.T) {
	rule := ruleByID(t, "parity-ineffective")

	// Under-provisioned: parity repairs plenty, it just cannot keep up.
	under := viewerFacts()
	under.SetClient("parityChunksReceived", 400)
	under.SetClient("framesRecoveredByParity", 40)
	under.SetClient("framesDroppedIncomplete", 30)
	got := rule.Eval(under)
	if got == nil {
		t.Fatal("under-provisioned case did not fire")
	}
	if !strings.Contains(got.Verdict, "under-provisioned") {
		t.Errorf("verdict = %q, want the under-provisioned wording", got.Verdict)
	}

	// Bursty: parity arrived in the same volume and repaired almost nothing,
	// because the erasures cluster into the frames whose symbols they also
	// took out.
	bursty := viewerFacts()
	bursty.SetClient("parityChunksReceived", 400)
	bursty.SetClient("framesRecoveredByParity", 5)
	bursty.SetClient("framesDroppedIncomplete", 60)
	got = rule.Eval(bursty)
	if got == nil {
		t.Fatal("bursty case did not fire")
	}
	if !strings.Contains(got.Verdict, "bursty") {
		t.Errorf("verdict = %q, want the bursty wording", got.Verdict)
	}

	// And the action must name Resilient mode, since that is the only real
	// answer to clustered loss.
	if !strings.Contains(rule.Action, "Resilient") {
		t.Errorf("action does not point at Resilient mode: %q", rule.Action)
	}
}

// A session predating R29 carries none of these fields, and must diagnose
// without the rule rather than firing on zeros.
func TestParityIneffectiveQuietWithoutParityFields(t *testing.T) {
	rule := ruleByID(t, "parity-ineffective")
	if got := rule.Eval(viewerFacts()); got != nil {
		t.Fatalf("fired on a session with no parity fields: %+v", got)
	}
}

func ruleByID(t *testing.T, id string) Rule {
	t.Helper()
	for _, r := range Playbook() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("rule %q not found", id)
	return Rule{}
}
