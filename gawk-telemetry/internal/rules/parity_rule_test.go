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

// R29 finding 3 (docs/34): a third cause the first two branches could not see,
// and the reason the shipped verdict on session 9b078876 was wrong.
//
// A shallow BROWSER receive queue drops from the head of each frame's burst,
// so the parity symbols — always written last — survive intact while the data
// chunks ahead of them die in runs. Parity then repairs a plausible-looking
// share, the rule reads "under-provisioned" and recommends raising the fleet
// level, which buys almost nothing: the erasures are clustered by the
// receiver, not spread by the network.
//
// The discriminator is a fact, not an inference: the browser's own queue depth.
func TestParityIneffectiveNamesAShallowReceiveQueue(t *testing.T) {
	rule := ruleByID(t, "parity-ineffective")

	shallow := viewerFacts()
	shallow.SetClient("parityChunksReceived", 400)
	shallow.SetClient("framesRecoveredByParity", 40)
	shallow.SetClient("framesDroppedIncomplete", 30)
	// Firefox 154, measured: one datagram, against a frame burst of ~11.
	shallow.SetClient("datagramBufferDefault", 1)
	shallow.SetClient("datagramBufferGovernsDrops", 0)
	got := rule.Eval(shallow)
	if got == nil {
		t.Fatal("shallow-queue case did not fire")
	}
	if !strings.Contains(got.Verdict, "receive queue") {
		t.Errorf("verdict = %q, want it to name the receive queue", got.Verdict)
	}
	// It must outrank the under-provisioned reading: the recovery ratio here
	// is identical to the under-provisioned case above, and following that
	// advice would waste fleet uplink on a client-side defect.
	if strings.Contains(got.Verdict, "under-provisioned") {
		t.Errorf("verdict fell back to under-provisioned: %q", got.Verdict)
	}
	// The evidence has to carry the depth, or an operator cannot check it.
	var sawDepth bool
	for _, e := range got.Evidence {
		if e.Signal == "datagramBufferDefault" {
			sawDepth = true
		}
	}
	if !sawDepth {
		t.Errorf("evidence does not carry datagramBufferDefault: %+v", got.Evidence)
	}

	// A browser whose queue is deep enough must NOT be blamed for it.
	deep := viewerFacts()
	deep.SetClient("parityChunksReceived", 400)
	deep.SetClient("framesRecoveredByParity", 40)
	deep.SetClient("framesDroppedIncomplete", 30)
	deep.SetClient("datagramBufferDefault", 256)
	deep.SetClient("datagramBufferGovernsDrops", 1)
	got = rule.Eval(deep)
	if got == nil {
		t.Fatal("deep-queue case did not fire at all")
	}
	if strings.Contains(got.Verdict, "receive queue") {
		t.Errorf("blamed a deep receive queue: %q", got.Verdict)
	}
}
