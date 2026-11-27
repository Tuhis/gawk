package main

import (
	"reflect"
	"testing"
)

func TestParseJoin(t *testing.T) {
	a := "a843cb7be3cd9a23b89158b8bf64ff64c39273f350cbfc13477b672733731e5e"
	b := "378f6a338f272bcd6e9bf9faf4c8ce65f8fd8afd0f96f14524413216331f339c"
	for _, link := range []string{
		// The console's QR link (relayconsole.JoinQR, standalone tier).
		"https://gawk.ioio.fi/#/?relay=https%3A%2F%2F10.0.2.2%3A4433&certHash=" + a + "," + b,
		// A view link.
		"https://gawk.ioio.fi/#/view/ABC123?relay=https%3A%2F%2F10.0.2.2%3A4433&certHash=" + a + "," + b,
		// The native apps' form (relayconsole.ServerLink).
		"gawk://server?relay=https%3A%2F%2F10.0.2.2%3A4433&certHash=" + a + "," + b + "&label=LAN%20party",
	} {
		got, err := parseJoin(link)
		if err != nil {
			t.Fatalf("%s: %v", link, err)
		}
		if got.Relay != "https://10.0.2.2:4433" || !reflect.DeepEqual(got.CertHashes, []string{a, b}) {
			t.Errorf("%s: got %+v", link, got)
		}
	}
	if _, err := parseJoin("https://gawk.ioio.fi/#/?certHash=" + a); err == nil {
		t.Error("a link with no relay= parsed")
	}
	if _, err := parseJoin("gawk://server?relay=https%3A%2F%2F10.0.2.2%3A4433&certHash=zz"); err == nil {
		t.Error("a malformed hash parsed")
	}
}
