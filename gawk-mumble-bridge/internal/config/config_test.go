package config

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/bridge"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-server/voice"
)

const secret = "0123456789abcdef"

// required is the smallest valid flag set.
func required() []string {
	return []string{"-dev-cert", "-psk", "p", "-ticket-secret", secret, "-relay-url", "https://relay.example:4433", "-mumble-addr", "murmur:64738"}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Parse(required(), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Listen": ":4443", "InternalListen": ":4444", "OpsListen": ":2113",
		"MumbleNameTemplate": "{nick} [web]", "MumbleObserverName": "gawkBridgeBot",
		"MumbleTempTemplate": "gawk {key}", "MumbleTempParent": "",
		"MaxVoiceSessions": 200, "MaxTempChannels": 50, "DrainTimeout": 10 * time.Second,
		"RelayServerName": "relay.example", "LogFormat": "text",
		"MumbleClientVersion":      mumble.Version{Major: 1, Minor: 4, Patch: 0},
		"MumbleInsecureSkipVerify": false, "StatsKeyRandom": true,
	}
	v := reflect.ValueOf(c)
	for f, w := range want {
		if got := v.FieldByName(f).Interface(); !reflect.DeepEqual(got, w) {
			t.Errorf("%s default = %v, want %v", f, got, w)
		}
	}
	if len(c.StatsKey) != 32 || c.AllowedOrigins != nil {
		t.Errorf("stats key %d bytes, origins %v", len(c.StatsKey), c.AllowedOrigins)
	}
	c2, _ := Parse(required(), env(nil))
	if string(c2.StatsKey) == string(c.StatsKey) {
		t.Error("the random stats key repeats across processes")
	}
}

// every flag, its env twin and a non-default value, and where the value
// must land in bridge.Options (the R2 carry-all rule: every knob reaches the
// object that consumes it).
var knobs = []struct {
	flag, value string
	got         func(bridge.Options) any
	want        any
}{
	{"listen", "127.0.0.1:5443", func(o bridge.Options) any { return o.Listen }, "127.0.0.1:5443"},
	{"internal-listen", "127.0.0.1:5444", func(o bridge.Options) any { return o.InternalListen }, "127.0.0.1:5444"},
	{"ops-listen", "127.0.0.1:3113", func(o bridge.Options) any { return o.OpsListen }, "127.0.0.1:3113"},
	{"cert-file", "/tls/tls.crt", func(o bridge.Options) any { return o.CertFile }, "/tls/tls.crt"},
	{"key-file", "/tls/tls.key", func(o bridge.Options) any { return o.KeyFile }, "/tls/tls.key"},
	{"allowed-origins", "https://a.example, https://b.example", func(o bridge.Options) any { return o.AllowedOrigins }, []string{"https://a.example", "https://b.example"}},
	{"psk", "the-psk", func(o bridge.Options) any { return o.PSK }, "the-psk"},
	{"ticket-secret", "a-ticket-secret-of-length", func(o bridge.Options) any { return string(o.TicketSecret) }, "a-ticket-secret-of-length"},
	{"relay-url", "https://gawk-server.gawk:4433", func(o bridge.Options) any { return o.Relay.URL }, "https://gawk-server.gawk:4433"},
	{"relay-server-name", "relay.example.com", func(o bridge.Options) any { return o.Relay.ServerName }, "relay.example.com"},
	{"relay-ca-file", "/ca/relay.pem", func(o bridge.Options) any { return o.Relay.CAFile }, "/ca/relay.pem"},
	{"mumble-addr", "mumble.example:64739", func(o bridge.Options) any { return o.Mumble.Addr }, "mumble.example:64739"},
	{"mumble-password", "server-pw", func(o bridge.Options) any { return o.Mumble.Password }, "server-pw"},
	{"mumble-access-token", "bridge-token", func(o bridge.Options) any { return o.Mumble.AccessToken }, "bridge-token"},
	{"mumble-ca-file", "/ca/murmur.pem", func(o bridge.Options) any { return o.Mumble.TLS.CAFile }, "/ca/murmur.pem"},
	{"mumble-name-template", "{nick} (gawk)", func(o bridge.Options) any { return o.Mumble.NameTemplate }, "{nick} (gawk)"},
	{"mumble-observer-name", "watcher", func(o bridge.Options) any { return o.Mumble.ObserverName }, "watcher"},
	{"mumble-temp-parent", "Gawk/Rooms", func(o bridge.Options) any { return o.Mumble.TempParent }, "Gawk/Rooms"},
	{"mumble-temp-channel-template", "room {code}", func(o bridge.Options) any { return o.Mumble.TempChannelTemplate }, "room {code}"},
	{"mumble-client-version", "1.3.4", func(o bridge.Options) any { return o.Mumble.ClientVersion }, mumble.Version{Major: 1, Minor: 3, Patch: 4}},
	{"max-voice-sessions", "7", func(o bridge.Options) any { return o.MaxVoiceSessions }, 7},
	{"max-temp-channels", "3", func(o bridge.Options) any { return o.MaxTempChannels }, 3},
	{"drain-timeout", "25s", func(o bridge.Options) any { return o.DrainTimeout }, 25 * time.Second},
	{"stats-key", "fixed-stats-key", func(o bridge.Options) any { return string(o.StatsKey) }, "fixed-stats-key"},
}

// Knobs that exclude one another get their own carry test.
var exclusiveKnobs = []struct {
	flag, value string
	drop        []string
	got         func(bridge.Options) any
	want        any
}{
	{"dev-cert=true", "", []string{"cert-file", "key-file"}, func(o bridge.Options) any { return o.DevCert }, true},
	{"mumble-cert-sha256", strings.Repeat("ab", 32), []string{"mumble-ca-file"}, func(o bridge.Options) any { return o.Mumble.TLS.CertSHA256 }, strings.Repeat("ab", 32)},
	{"mumble-insecure-skip-verify=true", "", []string{"mumble-ca-file"}, func(o bridge.Options) any { return o.Mumble.TLS.InsecureSkipVerify }, true},
}

func argsFor(skip ...string) []string {
	var args []string
	for _, k := range knobs {
		if !contains(skip, k.flag) {
			args = append(args, "-"+k.flag, k.value)
		}
	}
	return args
}

func envFor(skip ...string) map[string]string {
	m := map[string]string{}
	for _, k := range knobs {
		if !contains(skip, k.flag) {
			m[envName(k.flag)] = k.value
		}
	}
	return m
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestEveryFlagReachesTheBridge(t *testing.T) {
	c, err := Parse(argsFor(), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Options("v1", nil)
	for _, k := range knobs {
		if got := k.got(o); !reflect.DeepEqual(got, k.want) {
			t.Errorf("-%s: Options got %v, want %v", k.flag, got, k.want)
		}
	}
	if o.Version != "v1" {
		t.Error("version")
	}
	for _, k := range exclusiveKnobs {
		args := append(argsFor(k.drop...), "-"+k.flag)
		if k.value != "" {
			args = append(args, k.value)
		}
		c, err := Parse(args, env(nil))
		if err != nil {
			t.Fatalf("-%s: %v", k.flag, err)
		}
		if got := k.got(c.Options("", nil)); !reflect.DeepEqual(got, k.want) {
			t.Errorf("-%s: Options got %v, want %v", k.flag, got, k.want)
		}
	}
	// No Options field is left at its zero value by a full flag set: a new
	// field without a flag fails here.
	ov := reflect.ValueOf(o)
	for i := 0; i < ov.NumField(); i++ {
		name := ov.Type().Field(i).Name
		switch name {
		case "DevCert", "Logger", "Timings":
			continue // exclusive (above), and not flags
		}
		if ov.Field(i).IsZero() {
			t.Errorf("Options.%s is zero with every flag set", name)
		}
	}
}

func TestEveryEnvTwinWorksAndFlagsWin(t *testing.T) {
	c, err := Parse(nil, env(envFor()))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Options("", nil)
	for _, k := range knobs {
		if got := k.got(o); !reflect.DeepEqual(got, k.want) {
			t.Errorf("%s: got %v, want %v", envName(k.flag), got, k.want)
		}
	}
	for _, k := range exclusiveKnobs {
		e := envFor(k.drop...)
		name, val, _ := strings.Cut(k.flag, "=")
		if val == "" {
			val = k.value
		}
		e[envName(name)] = val
		c, err := Parse(nil, env(e))
		if err != nil {
			t.Fatalf("%s: %v", envName(k.flag), err)
		}
		if got := k.got(c.Options("", nil)); !reflect.DeepEqual(got, k.want) {
			t.Errorf("%s: got %v", envName(k.flag), got)
		}
	}
	if envName("relay-url") != "GAWK_BRIDGE_RELAY_URL" {
		t.Fatal(envName("relay-url"))
	}
	e := envFor()
	c, err = Parse([]string{"-max-voice-sessions", "9"}, env(e))
	if err != nil || c.MaxVoiceSessions != 9 {
		t.Fatalf("flag must beat env: %v %d", err, c.MaxVoiceSessions)
	}
	c, err = Parse(append(required(), "-log-level", "debug", "-log-format", "json"), env(nil))
	if err != nil || c.LogFormat != "json" || c.LogLevel.String() != "DEBUG" {
		t.Fatalf("log knobs: %v", err)
	}
}

func TestValidation(t *testing.T) {
	bad := map[string][]string{
		"no psk":             {"-psk", ""},
		"short secret":       {"-ticket-secret", "short"},
		"http relay":         {"-relay-url", "http://relay:4433"},
		"relay with path":    {"-relay-url", "https://relay:4433/x"},
		"relay with query":   {"-relay-url", "https://relay:4433?a=b"},
		"no relay":           {"-relay-url", ""},
		"no mumble":          {"-mumble-addr", ""},
		"mumble no port":     {"-mumble-addr", "murmur"},
		"bad listen":         {"-listen", "nope"},
		"bad internal":       {"-internal-listen", "nope"},
		"bad ops":            {"-ops-listen", "nope"},
		"no {nick}":          {"-mumble-name-template", "web user"},
		"empty observer":     {"-mumble-observer-name", " "},
		"constant temp name": {"-mumble-temp-channel-template", "gawk"},
		"protobuf audio":     {"-mumble-client-version", "1.5.0"},
		"no opus":            {"-mumble-client-version", "1.2.3"},
		"bad version":        {"-mumble-client-version", "1.4"},
		"zero sessions":      {"-max-voice-sessions", "0"},
		"negative temp":      {"-max-temp-channels", "-1"},
		"bad drain":          {"-drain-timeout", "soon"},
		"zero drain":         {"-drain-timeout", "0s"},
		"bad bool":           {"-mumble-insecure-skip-verify", "maybe"},
		"bad dev-cert":       {"-dev-cert=maybe"},
		"bad pin":            {"-mumble-cert-sha256", "abc"},
		"two trusts":         {"-mumble-insecure-skip-verify", "true", "-mumble-ca-file", "/x"},
		"link origin listed": {"-allowed-origins", voice.LinkOrigin},
		"dev and file certs": {"-cert-file", "/a", "-key-file", "/b"},
		"bad level":          {"-log-level", "loud"},
		"bad format":         {"-log-format", "xml"},
		"stray argument":     {"extra"},
		"unknown flag":       {"-nope"},
	}
	for name, extra := range bad {
		if _, err := Parse(append(required(), extra...), env(nil)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse(required(), env(map[string]string{"GAWK_BRIDGE_MUMBLE_INSECURE_SKIP_VERIFY": "maybe"})); err == nil {
		t.Error("bad boolean env accepted")
	}
	// Without -dev-cert, the file pair is required.
	if _, err := Parse([]string{"-psk", "p", "-ticket-secret", secret, "-relay-url", "https://r:1", "-mumble-addr", "m:1"}, env(nil)); err == nil {
		t.Error("no certificate accepted")
	}
}

func TestLogAttrsNeverCarrySecrets(t *testing.T) {
	c, err := Parse(argsFor(), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	attrs := c.LogAttrs()
	joined := strings.ToLower(strings.TrimSpace(strings.Join(func() []string {
		var s []string
		for _, a := range attrs {
			s = append(s, strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(toString(a), "\n", ""), " ", "")))
		}
		return s
	}(), " ")))
	for _, secret := range []string{"the-psk", "a-ticket-secret-of-length", "server-pw", "bridge-token", "fixed-stats-key"} {
		if strings.Contains(joined, secret) {
			t.Errorf("start-up log carries %q", secret)
		}
	}
}

func toString(a any) string {
	switch v := a.(type) {
	case string:
		return v
	case []string:
		return strings.Join(v, ",")
	}
	return ""
}

// The chart is the third half of every knob (CODE-REVIEW.md): every flag
// but -dev-cert (development only) has its env in the Deployment, and the
// Deployment sets no env the binary does not read.
func TestChartSetsEveryFlag(t *testing.T) {
	tpl, err := os.ReadFile("../../deploy/charts/gawk-mumble-bridge/templates/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	inChart := map[string]bool{}
	for _, m := range regexp.MustCompile(`GAWK_BRIDGE_[A-Z0-9_]+`).FindAllString(string(tpl), -1) {
		inChart[m] = true
	}
	flags := map[string]bool{}
	for _, k := range knobs {
		flags[envName(k.flag)] = true
	}
	for _, k := range exclusiveKnobs {
		name, _, _ := strings.Cut(k.flag, "=")
		flags[envName(name)] = true
	}
	flags[envName("log-level")], flags[envName("log-format")] = true, true
	for f := range flags {
		if f != envName("dev-cert") && !inChart[f] {
			t.Errorf("the chart does not set %s", f)
		}
	}
	for e := range inChart {
		if !flags[e] {
			t.Errorf("the chart sets %s, which no flag reads", e)
		}
	}
}
