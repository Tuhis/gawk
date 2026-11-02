package config

import (
	"flag"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/gateway"
)

func required() []string { return []string{"-relay-url", "https://relay.example:4433"} }

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Parse(required(), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Options("v", nil)
	checks := map[string][2]any{
		"listen":            {c.Listen, ":8080"},
		"internal":          {c.InternalListen, ":8081"},
		"ops":               {c.OpsListen, ":9090"},
		"media port":        {o.MediaPort, 8189},
		"max sessions":      {o.MaxSessions, 20},
		"max sessions hard": {o.MaxSessionsHard, 25}, // ceil(1.25 × 20)
		"post rate":         {o.POSTRate, 1.0},
		"post burst":        {o.POSTBurst, 5},
		"initial rtt":       {o.Reorder.Window.InitialRTT, 50 * time.Millisecond},
		"floor":             {o.Reorder.Window.Floor, 20 * time.Millisecond},
		"cap":               {o.Reorder.Window.Cap, 150 * time.Millisecond},
		"multiplier":        {o.Reorder.Window.Multiplier, 1.25},
		"offset":            {o.Reorder.Window.Offset, 5 * time.Millisecond},
		"gop warn":          {o.GOP.WarnAbove, 1100 * time.Millisecond},
		"gop max":           {o.GOP.EndAbove, 2200 * time.Millisecond},
		"gop early":         {o.GOP.EarlyGap, 4500 * time.Millisecond},
		"gop samples":       {o.GOP.Samples, 5},
		"backoff initial":   {o.Backoff.Initial, 30 * time.Second},
		"backoff max":       {o.Backoff.Max, 10 * time.Minute},
		"record ttl":        {o.RecordTTL, 10 * time.Minute},
		"max records":       {o.MaxRecords, 4096},
		"peer timeout":      {o.PeerTimeout, 200 * time.Millisecond},
		"full retry":        {o.FullRetryAfter, 30 * time.Second},
		"busy retry":        {o.RelayBusyRetryAfter, 30 * time.Second},
		"dial timeout":      {o.Relay.DialTimeout, 10 * time.Second},
	}
	for name, c := range checks {
		if !reflect.DeepEqual(c[0], c[1]) {
			t.Errorf("%s default = %v, want %v", name, c[0], c[1])
		}
	}
	if o.Peers != nil || o.TrustedProxyCIDRs != nil || o.AllowedOrigins != nil {
		t.Errorf("lists default to empty: %v %v %v", o.Peers, o.TrustedProxyCIDRs, o.AllowedOrigins)
	}
}

// Every flag, a non-default value, and where it must land in
// gateway.Options (CODE-REVIEW.md: walk each knob to its consumer).
var knobs = []struct {
	flag, value string
	got         func(Config, gateway.Options) any
	want        any
}{
	{"listen", "127.0.0.1:18080", func(c Config, _ gateway.Options) any { return c.Listen }, "127.0.0.1:18080"},
	{"internal-listen", "127.0.0.1:18081", func(c Config, _ gateway.Options) any { return c.InternalListen }, "127.0.0.1:18081"},
	{"ops-listen", "127.0.0.1:19090", func(c Config, _ gateway.Options) any { return c.OpsListen }, "127.0.0.1:19090"},
	{"tls-cert-file", "/tls/tls.crt", func(c Config, _ gateway.Options) any { return c.TLSCertFile }, "/tls/tls.crt"},
	{"tls-key-file", "/tls/tls.key", func(c Config, _ gateway.Options) any { return c.TLSKeyFile }, "/tls/tls.key"},
	{"relay-url", "https://gawk-server.gawk:4433", func(_ Config, o gateway.Options) any { return o.Relay.URL }, "https://gawk-server.gawk:4433"},
	{"relay-publish-secret", "pub-secret", func(_ Config, o gateway.Options) any { return o.Relay.PublishSecret }, "pub-secret"},
	{"relay-publish-proxy-token", "proxy-tok", func(_ Config, o gateway.Options) any { return o.Relay.PublishProxyToken }, "proxy-tok"},
	{"relay-origin", "https://whip.example", func(_ Config, o gateway.Options) any { return o.Relay.Origin }, "https://whip.example"},
	{"relay-insecure", "true", func(_ Config, o gateway.Options) any { return o.Relay.Insecure }, true},
	{"relay-dial-timeout", "3s", func(_ Config, o gateway.Options) any { return o.Relay.DialTimeout }, 3 * time.Second},
	{"public-address", "203.0.113.10", func(_ Config, o gateway.Options) any { return o.PublicAddress }, "203.0.113.10"},
	{"media-bind", "0.0.0.0", func(_ Config, o gateway.Options) any { return o.MediaBind }, "0.0.0.0"},
	{"media-base-port", "40000", func(_ Config, o gateway.Options) any { return o.MediaPort }, 40000},
	{"pod-name", "gawk-whip-gateway-2", func(c Config, _ gateway.Options) any { return c.Ordinal }, 2},
	{"ice-disconnected-timeout", "4s", func(_ Config, o gateway.Options) any { return o.ICEDisconnectedTimeout }, 4 * time.Second},
	{"ice-failed-timeout", "9s", func(_ Config, o gateway.Options) any { return o.ICEFailedTimeout }, 9 * time.Second},
	{"ice-keepalive", "1s", func(_ Config, o gateway.Options) any { return o.ICEKeepalive }, time.Second},
	{"allowed-origins", "https://a.example, https://b.example", func(_ Config, o gateway.Options) any { return o.AllowedOrigins }, []string{"https://a.example", "https://b.example"}},
	{"trusted-proxy-cidrs", "10.42.0.0/16, fd00:42::/64", func(_ Config, o gateway.Options) any { return o.TrustedProxyCIDRs }, []string{"10.42.0.0/16", "fd00:42::/64"}},
	{"post-rate", "2.5", func(_ Config, o gateway.Options) any { return o.POSTRate }, 2.5},
	{"post-burst", "9", func(_ Config, o gateway.Options) any { return o.POSTBurst }, 9},
	{"max-sessions", "8", func(_ Config, o gateway.Options) any { return o.MaxSessions }, 8},
	{"max-sessions-hard", "30", func(_ Config, o gateway.Options) any { return o.MaxSessionsHard }, 30},
	{"full-retry-after", "45s", func(_ Config, o gateway.Options) any { return o.FullRetryAfter }, 45 * time.Second},
	{"relay-busy-retry-after", "50s", func(_ Config, o gateway.Options) any { return o.RelayBusyRetryAfter }, 50 * time.Second},
	{"reorder-initial-rtt", "70ms", func(_ Config, o gateway.Options) any { return o.Reorder.Window.InitialRTT }, 70 * time.Millisecond},
	{"reorder-window-floor", "25ms", func(_ Config, o gateway.Options) any { return o.Reorder.Window.Floor }, 25 * time.Millisecond},
	{"reorder-window-cap", "120ms", func(_ Config, o gateway.Options) any { return o.Reorder.Window.Cap }, 120 * time.Millisecond},
	{"reorder-window-multiplier", "1.5", func(_ Config, o gateway.Options) any { return o.Reorder.Window.Multiplier }, 1.5},
	{"reorder-window-offset", "2ms", func(_ Config, o gateway.Options) any { return o.Reorder.Window.Offset }, 2 * time.Millisecond},
	{"reorder-max-packets", "2048", func(_ Config, o gateway.Options) any { return o.Reorder.MaxPackets }, 2048},
	{"gop-warn", "1.2s", func(_ Config, o gateway.Options) any { return o.GOP.WarnAbove }, 1200 * time.Millisecond},
	{"gop-max", "2.5s", func(_ Config, o gateway.Options) any { return o.GOP.EndAbove }, 2500 * time.Millisecond},
	{"gop-early-gap", "5s", func(_ Config, o gateway.Options) any { return o.GOP.EarlyGap }, 5 * time.Second},
	{"gop-samples", "7", func(_ Config, o gateway.Options) any { return o.GOP.Samples }, 7},
	{"backoff-initial", "20s", func(_ Config, o gateway.Options) any { return o.Backoff.Initial }, 20 * time.Second},
	{"backoff-max", "5m", func(_ Config, o gateway.Options) any { return o.Backoff.Max }, 5 * time.Minute},
	{"record-ttl", "15m", func(_ Config, o gateway.Options) any { return o.RecordTTL }, 15 * time.Minute},
	{"max-records", "100", func(_ Config, o gateway.Options) any { return o.MaxRecords }, 100},
	{"peers", "http://gw-1.peers:8081, http://gw-2.peers:8081", func(_ Config, o gateway.Options) any { return o.Peers }, []string{"http://gw-1.peers:8081", "http://gw-2.peers:8081"}},
	{"internal-token", "int-tok", func(_ Config, o gateway.Options) any { return o.InternalToken }, "int-tok"},
	{"peer-template", "http://gw-{ordinal}.peers:8081", func(_ Config, o gateway.Options) any { return o.Peers }, []string{"http://gw-1.peers:8081"}},
	{"replicas", "3", func(c Config, _ gateway.Options) any { return c.Replicas }, 3},
	{"peer-timeout", "300ms", func(_ Config, o gateway.Options) any { return o.PeerTimeout }, 300 * time.Millisecond},
	{"log-level", "debug", func(c Config, _ gateway.Options) any { return c.LogLevel.String() }, "DEBUG"},
	{"log-format", "json", func(c Config, _ gateway.Options) any { return c.LogFormat }, "json"},
}

// The knobs needing a partner flag to validate.
var partners = map[string][]string{
	"tls-cert-file": {"-tls-key-file", "/k"},
	"tls-key-file":  {"-tls-cert-file", "/c"},
	"peers":         {"-internal-token", "t"},
	"peer-template": {"-replicas", "2", "-internal-token", "t"},
}

// allFlags lists every registered flag, so a new one without a knob entry
// fails the test.
func allFlags(t *testing.T) []string {
	var names []string
	onFlagSet = func(fs *flag.FlagSet) { fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) }) }
	defer func() { onFlagSet = nil }()
	if _, err := Parse(required(), env(nil)); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestEveryFlagReachesOptions(t *testing.T) {
	covered := map[string]bool{}
	for _, k := range knobs {
		covered[k.flag] = true
		args := append(required(), "-"+k.flag+"="+k.value)
		args = append(args, partners[k.flag]...)
		c, err := Parse(args, env(nil))
		if err != nil {
			t.Fatalf("-%s %s: %v", k.flag, k.value, err)
		}
		if got := k.got(c, c.Options("v", nil)); !reflect.DeepEqual(got, k.want) {
			t.Errorf("-%s %s: got %v, want %v", k.flag, k.value, got, k.want)
		}
	}
	for _, name := range allFlags(t) {
		if !covered[name] {
			t.Errorf("flag -%s has no knob entry: walk it to gateway.Options", name)
		}
	}
}

func TestEveryEnvTwinWorksAndFlagsWin(t *testing.T) {
	for _, k := range knobs {
		e := map[string]string{envName(k.flag): k.value, "GAWK_WHIP_RELAY_URL": "https://relay.example:4433"}
		for i := 0; i+1 < len(partners[k.flag]); i += 2 {
			e[envName(strings.TrimPrefix(partners[k.flag][i], "-"))] = partners[k.flag][i+1]
		}
		if k.flag == "relay-url" {
			e["GAWK_WHIP_RELAY_URL"] = k.value
		}
		c, err := Parse(nil, env(e))
		if err != nil {
			t.Fatalf("%s=%s: %v", envName(k.flag), k.value, err)
		}
		if got := k.got(c, c.Options("v", nil)); !reflect.DeepEqual(got, k.want) {
			t.Errorf("%s=%s: got %v, want %v", envName(k.flag), k.value, got, k.want)
		}
	}
	c, err := Parse([]string{"-max-sessions", "3"}, env(map[string]string{"GAWK_WHIP_MAX_SESSIONS": "9", "GAWK_WHIP_RELAY_URL": "https://r:1"}))
	if err != nil || c.MaxSessions != 3 {
		t.Fatalf("flag must win over env: %v %d", err, c.MaxSessions)
	}
}

// D10: the pod's media port is basePort + its StatefulSet ordinal, and the
// peer template lists every other ordinal.
func TestOrdinalMediaPortAndPeerTemplate(t *testing.T) {
	for name, want := range map[string]int{"": 0, "gawk-whip-gateway-0": 0, "gawk-whip-gateway-3": 3, "whip": 0, "gw-12": 12} {
		if got := Ordinal(name); got != want {
			t.Errorf("Ordinal(%q) = %d, want %d", name, got, want)
		}
	}
	c, err := Parse(append(required(), "-pod-name", "gawk-whip-gateway-1", "-media-base-port", "30000", "-replicas", "3",
		"-peer-template", "http://gawk-whip-gateway-{ordinal}.gawk-whip-gateway-peers:8081", "-internal-token", "t"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.MediaPort != 30001 {
		t.Fatalf("media port %d, want 30001", c.MediaPort)
	}
	want := []string{"http://gawk-whip-gateway-0.gawk-whip-gateway-peers:8081", "http://gawk-whip-gateway-2.gawk-whip-gateway-peers:8081"}
	if !reflect.DeepEqual(c.Peers, want) {
		t.Fatalf("peers %v, want %v", c.Peers, want)
	}
}

func TestInvalidConfigs(t *testing.T) {
	bad := [][]string{
		{},
		{"-relay-url", "http://relay:4433"},
		{"-relay-url", "https://relay:4433/path"},
		append(required(), "-max-sessions", "0"),
		append(required(), "-max-sessions", "10", "-max-sessions-hard", "5"),
		append(required(), "-peers", "http://gw-1:8081"), // no internal token
		append(required(), "-peers", "gw-1:8081", "-internal-token", "t"),
		append(required(), "-peers", "http://a:1", "-peer-template", "http://gw-{ordinal}:1", "-internal-token", "t"),
		append(required(), "-peer-template", "http://gw:1", "-internal-token", "t"),
		append(required(), "-trusted-proxy-cidrs", "nope"),
		append(required(), "-public-address", "gw.example"),
		append(required(), "-reorder-window-floor", "200ms"),
		append(required(), "-gop-warn", "3s"),
		append(required(), "-backoff-initial", "20m"),
		append(required(), "-tls-cert-file", "/c"),
		append(required(), "-relay-dial-timeout", "0s"),
		append(required(), "-media-base-port", "65535", "-pod-name", "gw-1"),
		append(required(), "-internal-token", "same", "-relay-publish-proxy-token", "same"),
		append(required(), "-log-format", "xml"),
		append(required(), "stray"),
	}
	for _, args := range bad {
		if _, err := Parse(args, env(nil)); err == nil {
			t.Errorf("Parse(%q) accepted", args)
		}
	}
	if _, err := Parse(nil, env(map[string]string{"GAWK_WHIP_RELAY_URL": "https://r:1", "GAWK_WHIP_RELAY_INSECURE": "maybe"})); err == nil {
		t.Error("a bad boolean env was accepted")
	}
}

func TestLogAttrsHideSecrets(t *testing.T) {
	c, err := Parse(append(required(), "-relay-publish-secret", "SEKRIT1", "-relay-publish-proxy-token", "SEKRIT2",
		"-internal-token", "SEKRIT3", "-peers", "http://gw-1:8081"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range c.LogAttrs() {
		if s, ok := a.(string); ok && strings.Contains(s, "SEKRIT") {
			t.Fatalf("a secret reached the start-up log: %v", c.LogAttrs())
		}
	}
}
