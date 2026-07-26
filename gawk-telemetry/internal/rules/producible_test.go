package rules

import "testing"

// The mechanical guard finding 5 asks for. A rule whose Requires names a
// signal no producer emits is dead on arrival — it evaluates to `unavailable`
// on every session, forever, and nothing at runtime ever says so.
func TestEveryRuleRequirementIsProducible(t *testing.T) {
	for _, r := range Playbook() {
		for _, req := range r.Requires {
			if _, ok := ProducibleFacts[req]; !ok {
				t.Errorf("rule %q requires %q, which no producer emits — the rule can never fire",
					r.ID, req)
			}
		}
	}
}

// The inventory is a contract, not a wishlist: an entry nothing requires and
// nothing produces would quietly make the guard above weaker.
func TestInventoryEntriesAreQualified(t *testing.T) {
	for name := range ProducibleFacts {
		if _, _, ok := splitSignal(name); !ok {
			t.Errorf("inventory entry %q is not a qualified signal name", name)
		}
	}
}
