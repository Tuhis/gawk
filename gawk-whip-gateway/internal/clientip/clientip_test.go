package clientip

import (
	"errors"
	"net/http"
	"testing"
)

func req(remote string, xff ...string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "http://gw/whip", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

// WG5's client-IP rows (D4), one by one.
func TestD4ClientIP(t *testing.T) {
	ingress, err := New([]string{"10.42.0.0/16", "fd00:42::/64"}) // the ingress controller's pod ranges
	if err != nil {
		t.Fatal(err)
	}
	empty, _ := New(nil)
	cases := []struct {
		name    string
		r       *Resolver
		req     *http.Request
		want    string
		outcome Outcome
		err     bool
	}{
		{"inside the trusted range: the right-most untrusted entry", ingress, req("10.42.3.4:5555", "198.51.100.9, 203.0.113.7"), "203.0.113.7", FromForwarded, false},
		{"append mode: forged, <client> from an ingress pod yields <client>", ingress, req("10.42.3.4:5555", "6.6.6.6, 203.0.113.7"), "203.0.113.7", FromForwarded, false},
		{"trusted hops are walked past", ingress, req("10.42.3.4:5555", "203.0.113.7, 10.42.9.9"), "203.0.113.7", FromForwarded, false},
		{"several header lines are joined in order", ingress, req("10.42.3.4:5555", "6.6.6.6", "203.0.113.7, 10.42.1.1"), "203.0.113.7", FromForwarded, false},
		{"an unparsable entry stops the walk: 400", ingress, req("10.42.3.4:5555", "203.0.113.7, junk"), "", FromForwarded, true},
		{"junk to the left of the client is never reached", ingress, req("10.42.3.4:5555", "junk, 203.0.113.7"), "203.0.113.7", FromForwarded, false},
		{"a missing header from a trusted peer: 400", ingress, req("10.42.3.4:5555"), "", FromForwarded, true},
		{"an empty header from a trusted peer: 400", ingress, req("10.42.3.4:5555", " "), "", FromForwarded, true},
		{"an all-trusted chain yields its left-most entry", ingress, req("10.42.3.4:5555", "10.42.7.7, 10.42.8.8"), "10.42.7.7", FromForwarded, false},
		{"from outside the range a forged header is ignored", ingress, req("203.0.113.50:4000", "1.2.3.4"), "203.0.113.50", UntrustedForwarded, false},
		{"from outside the range without a header", ingress, req("203.0.113.50:4000"), "203.0.113.50", FromRemoteAddr, false},
		{"empty list: a forged header is ignored, never 400", empty, req("10.42.3.4:5555", "1.2.3.4"), "10.42.3.4", FromRemoteAddr, false},
		{"empty list: junk never 400", empty, req("10.42.3.4:5555", "junk"), "10.42.3.4", FromRemoteAddr, false},
		{"a v4-mapped entry is canonicalised", ingress, req("10.42.3.4:5555", "::ffff:203.0.113.7"), "203.0.113.7", FromForwarded, false},
		{"a v4-mapped peer is canonicalised before the CIDR match", ingress, req("[::ffff:10.42.3.4]:5555", "203.0.113.7"), "203.0.113.7", FromForwarded, false},
		{"[v6]:port and v4:port entries", ingress, req("[fd00:42::5]:443", "[2001:db8::1]:1234, 203.0.113.7:80"), "203.0.113.7", FromForwarded, false},
		{"a zone is stripped", ingress, req("10.42.3.4:5555", "fe80::1%eth0"), "fe80::1", FromForwarded, false},
		{"dual-stack: a v6 ingress pod is trusted too", ingress, req("[fd00:42::5]:443", "2001:db8::77"), "2001:db8::77", FromForwarded, false},
	}
	for _, c := range cases {
		got, outcome, err := c.r.Resolve(c.req)
		if c.err {
			if !errors.Is(err, ErrUnknown) {
				t.Errorf("%s: got %v %v, want ErrUnknown", c.name, got, err)
			}
			continue
		}
		if err != nil || got.String() != c.want || outcome != c.outcome {
			t.Errorf("%s: got %v outcome %v err %v, want %s outcome %v", c.name, got, outcome, err, c.want, c.outcome)
		}
	}
	if _, err := New([]string{"not a cidr"}); err == nil {
		t.Fatal("bad CIDR accepted")
	}
	r, _ := New([]string{"::ffff:10.42.0.0/112"})
	if a, _, err := r.Resolve(req("10.42.0.9:1", "203.0.113.7")); err != nil || a.String() != "203.0.113.7" {
		t.Fatalf("a v4-mapped trusted prefix must match the plain v4 peer: %v %v", a, err)
	}
}
