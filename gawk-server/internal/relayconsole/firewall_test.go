package relayconsole

import (
	"net/netip"
	"strings"
	"testing"
)

// docs/76 §7.1 Health: one elevation adds a rule scoped to this program,
// UDP and TCP on the relay's port, the local subnet, and the Private and
// Public profiles — replacing an older rule of ours rather than stacking.
func TestFirewallCommand(t *testing.T) {
	exe := `C:\Users\Host Name\Downloads\gawk-relay & co\gawk-relay.exe`
	argv, err := FirewallCommand(exe, netip.MustParsePrefix("192.168.1.20/24"), 4433, 4433)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd.exe", "/d", "/c",
		`netsh advfirewall firewall delete rule name="gawk-relay" program="` + exe + `"` +
			` & netsh advfirewall firewall add rule name="gawk-relay" dir=in action=allow program="` + exe + `" enable=yes profile=private,public protocol=UDP localport=4433 remoteip=192.168.1.0/24` +
			` & netsh advfirewall firewall add rule name="gawk-relay" dir=in action=allow program="` + exe + `" enable=yes profile=private,public protocol=TCP localport=4433 remoteip=192.168.1.0/24`}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q", argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Errorf("argv[%d]:\n got  %s\n want %s", i, argv[i], want[i])
		}
	}
}

// An unknown subnet falls back to netsh's own LocalSubnet keyword; another
// port is carried through.
func TestFirewallCommandFallbacks(t *testing.T) {
	argv, err := FirewallCommand(`C:\gawk\gawk-relay.exe`, netip.Prefix{}, 5544, 8443)
	if err != nil {
		t.Fatal(err)
	}
	chain := argv[3]
	for _, s := range []string{"protocol=UDP localport=5544 remoteip=localsubnet", "protocol=TCP localport=8443 remoteip=localsubnet"} {
		if !strings.Contains(chain, s) {
			t.Errorf("chain lacks %q:\n%s", s, chain)
		}
	}
	// IPv6 LAN: the prefix is masked too.
	argv, err = FirewallCommand(`C:\gawk\gawk-relay.exe`, netip.MustParsePrefix("fd12:3456::1/64"), 4433, 4433)
	if err != nil || !strings.Contains(argv[3], "remoteip=fd12:3456::/64") {
		t.Errorf("v6: %v %q", err, argv)
	}
}

// cmd.exe expands %VAR% even inside quotes and a quote would end the
// program= value early, so a path holding either is refused rather than
// escaped; so is a relative one (the rule must name this binary).
func TestFirewallCommandRefusesUnsafePaths(t *testing.T) {
	for _, exe := range []string{
		`C:\gawk\%PATH%\gawk-relay.exe`,
		`C:\gawk\"x\gawk-relay.exe`,
		"C:\\gawk\\a\nb\\gawk-relay.exe",
		`gawk-relay.exe`,
		``,
	} {
		if _, err := FirewallCommand(exe, netip.Prefix{}, 4433, 4433); err == nil {
			t.Errorf("%q: want an error", exe)
		}
	}
	if _, err := FirewallCommand(`C:\gawk\gawk-relay.exe`, netip.Prefix{}, 0, 4433); err == nil {
		t.Error("port 0: want an error")
	}
}
