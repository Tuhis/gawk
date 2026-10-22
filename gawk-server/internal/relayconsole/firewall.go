package relayconsole

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// FirewallRuleName names the Windows Firewall rules the console adds.
const FirewallRuleName = "gawk-relay"

// FirewallCommand builds the one elevated command behind the console's
// Allow on this network button (docs/76 §7.1 Health): delete any earlier
// rule of ours for this program, then allow inbound UDP on the relay's port
// and TCP on the named UI's, from the local subnet only, on the Private and
// Public profiles, for this program only.
//
// It is a cmd.exe chain so the host answers one UAC prompt, not three:
// ShellExecute's "runas" starts one process. cmd.exe does not parse argv
// the way programs do, so the caller passes argv[1:] joined by spaces as the
// raw parameter string (firewall_windows.go). The chain does not begin with
// a quote, so cmd /c keeps every quote in it; the program path is quoted, so
// spaces and & in it are literal. What quoting cannot make safe — %, which
// cmd expands even inside quotes, a quote, a line break — is refused.
//
// subnet is the LAN interface's prefix; the invalid Prefix falls back to
// netsh's LocalSubnet keyword.
func FirewallCommand(exe string, subnet netip.Prefix, udpPort, tcpPort int) ([]string, error) {
	if !windowsAbs(exe) {
		return nil, fmt.Errorf("relayconsole: firewall rule needs an absolute program path, got %q", exe)
	}
	if strings.ContainsAny(exe, "%\"\r\n") {
		return nil, fmt.Errorf("relayconsole: program path %q cannot be passed to netsh safely", exe)
	}
	for _, p := range []int{udpPort, tcpPort} {
		if p <= 0 || p > 65535 {
			return nil, fmt.Errorf("relayconsole: invalid firewall port %d", p)
		}
	}
	remote := "localsubnet"
	if subnet.IsValid() {
		remote = subnet.Masked().String()
	}
	prog := `program="` + exe + `"`
	add := func(proto string, p int) string {
		return `netsh advfirewall firewall add rule name="` + FirewallRuleName + `" dir=in action=allow ` + prog +
			" enable=yes profile=private,public protocol=" + proto + " localport=" + strconv.Itoa(p) + " remoteip=" + remote
	}
	chain := `netsh advfirewall firewall delete rule name="` + FirewallRuleName + `" ` + prog +
		" & " + add("UDP", udpPort) +
		" & " + add("TCP", tcpPort)
	return []string{"cmd.exe", "/d", "/c", chain}, nil
}

// windowsAbs reports a drive-absolute (C:\…) or UNC (\\host\share\…) path,
// checked as text so the construction is testable on any host.
func windowsAbs(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		c := p[0] | 0x20
		return c >= 'a' && c <= 'z'
	}
	return strings.HasPrefix(p, `\\`) && len(p) > 2
}

// ErrFirewallUnsupported is returned off Windows: the other platforms'
// prompts (macOS) or rules (Linux distributions) are not the console's to
// manage.
var ErrFirewallUnsupported = errors.New("relayconsole: the firewall helper is Windows-only")
