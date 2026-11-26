package metrics

import (
	"strings"
	"testing"
)

func TestCountsAndExposition(t *testing.T) {
	m := New()
	m.Count(Proxied)
	m.Count(Proxied)
	m.Count(OverCap)
	m.Reset()
	m.Active(3)
	m.Active(-1)
	m.Draining(Slots, 2)
	m.Draining(OverCapPool, 1)
	m.SetRoutesOK(true)
	s := m.Snapshot()
	if s.Conns["proxied"] != 2 || s.Conns["over_cap"] != 1 || s.Resets != 1 || s.Active != 2 ||
		s.Draining["slots"] != 2 || s.Draining["over_cap"] != 1 || !s.RoutesOK {
		t.Fatalf("%+v", s)
	}
	var b strings.Builder
	if err := m.Write(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`gawk_mmproxy_connections_total{result="proxied"} 2`,
		`gawk_mmproxy_connections_total{result="undrained"} 0`,
		`gawk_mmproxy_resets_total 1`, `gawk_mmproxy_active 2`,
		`gawk_mmproxy_draining{pool="slots"} 2`, `gawk_mmproxy_routes_ok 1`,
	} {
		if !strings.Contains(b.String(), want+"\n") {
			t.Errorf("missing %q", want)
		}
	}
	// Every result has a name: none prints as "".
	for r := Result(0); r < numResults; r++ {
		if r.String() == "" {
			t.Errorf("result %d has no name", r)
		}
	}
}
