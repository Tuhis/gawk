package popsrv

import (
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// namedPop is a Pop whose named tier is in the given state.
func namedPop(enabled bool, host, cert string) *pops.Pop {
	p := &pops.Pop{}
	p.Name = popName
	p.Spec.NamedAddress.Enabled = enabled
	if host != "" || cert != "" {
		p.Status.TLS = &pops.TLS{Host: host, Certificate: cert}
	}
	return p
}

// The named candidate is the host gawk-admin assigned and issued (docs/76
// §6.11), never the one the PoP claims in its Hello: the link-home pod
// offers exactly what every other pod offers from status.
func TestTheLinkHomePodOffersOnlyTheIssuedNamedHost(t *testing.T) {
	const issued = "p-7k2m9x.pop.gawk.test"
	for _, tc := range []struct {
		name string
		pop  *pops.Pop
		want string
	}{
		{"issued", namedPop(true, issued, "PEM"), issued},
		{"assigned, not yet issued", namedPop(true, issued, ""), ""},
		{"no tls status", namedPop(true, "", ""), ""},
		{"named address turned off", namedPop(false, issued, "PEM"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &link{name: popName, ended: make(chan struct{}),
				hello: poplink.Hello{NamedHost: "relay.attacker.example"}}
			l.follow(tc.pop)
			lp, ok := l.linkedPop()
			if !ok || lp.NamedHost != tc.want {
				t.Fatalf("linkedPop().NamedHost = %q, want %q", lp.NamedHost, tc.want)
			}
		})
	}
}

// Turning the named address off stops home naming the host, both in the
// offers every pod builds from status and in the PoP's config, so the PoP
// stops announcing a name whose DNS record gawk-admin just deleted.
func TestTurningTheNamedAddressOffStopsNamingTheHost(t *testing.T) {
	const issued = "p-7k2m9x.pop.gawk.test"
	m := &Manager{}
	if got := m.config(namedPop(true, issued, "PEM")).NamedHost; got != issued {
		t.Fatalf("enabled: config NamedHost = %q", got)
	}
	if got := m.config(namedPop(false, issued, "PEM")).NamedHost; got != "" {
		t.Fatalf("disabled: config NamedHost = %q, want none", got)
	}
	if got := offeredNamedHost(namedPop(false, issued, "PEM")); got != "" {
		t.Fatalf("disabled: offered named host = %q, want none", got)
	}
}
