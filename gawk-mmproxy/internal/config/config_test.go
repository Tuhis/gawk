package config

import (
	"flag"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-mmproxy/internal/routes"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestServeDefaults(t *testing.T) {
	s, err := ParseServe(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	o := s.Options()
	if o.Listen != ":64739" || o.MurmurPort != 64738 || o.RouteTable != routes.DefaultTable || !o.RequireRoutes ||
		o.HeaderTimeout != 2*time.Second || o.MaxConns != 1024 || s.OpsListen != ":2114" {
		t.Errorf("defaults %+v, ops %s", o, s.OpsListen)
	}
	r, err := ParseRoutes(nil, env(nil))
	if err != nil || r.RouteTable != routes.DefaultTable {
		t.Errorf("routes defaults %+v, %v", r, err)
	}
}

// Every serve flag, a non-default value, and where it must land (the R2
// carry-all rule).
var knobs = []struct {
	flag, value string
	got         func(Serve) any
	want        any
}{
	{"listen", "10.0.0.1:7000", func(s Serve) any { return s.Options().Listen }, "10.0.0.1:7000"},
	{"ops-listen", "127.0.0.1:3000", func(s Serve) any { return s.OpsOptions(nil).Listen }, "127.0.0.1:3000"},
	{"murmur-port", "64740", func(s Serve) any { return []any{s.Options().MurmurPort, s.OpsOptions(nil).MurmurPort} }, []any{uint16(64740), uint16(64740)}},
	{"route-table", "4242", func(s Serve) any { return s.Options().RouteTable }, uint32(4242)},
	{"require-routes", "false", func(s Serve) any { return []any{s.Options().RequireRoutes, s.OpsOptions(nil).RequireRoutes} }, []any{false, false}},
	{"header-timeout", "750ms", func(s Serve) any { return s.Options().HeaderTimeout }, 750 * time.Millisecond},
	{"max-conns", "17", func(s Serve) any { return s.Options().MaxConns }, 17},
	{"log-level", "debug", func(s Serve) any { return s.LogLevel.String() }, "DEBUG"},
	{"log-format", "json", func(s Serve) any { return s.LogFormat }, "json"},
}

func TestEveryServeFlagIsCovered(t *testing.T) {
	covered := map[string]bool{}
	for _, k := range knobs {
		covered[k.flag] = true
	}
	f := newFlags("serve", env(nil))
	defineServe(f)
	n := 0
	f.fs.VisitAll(func(fl *flag.Flag) {
		n++
		if !covered[fl.Name] {
			t.Errorf("flag -%s has no carry-all entry", fl.Name)
		}
	})
	if n != len(knobs) {
		t.Errorf("%d flags, %d knobs", n, len(knobs))
	}
}

func TestEveryFlagReachesItsConsumer(t *testing.T) {
	for _, k := range knobs {
		s, err := ParseServe([]string{"-" + k.flag + "=" + k.value}, env(nil))
		if err != nil {
			t.Fatalf("-%s: %v", k.flag, err)
		}
		if got := k.got(s); !reflect.DeepEqual(got, k.want) {
			t.Errorf("-%s=%s: got %v, want %v", k.flag, k.value, got, k.want)
		}
	}
}

func TestEveryEnvTwinWorksAndFlagsWin(t *testing.T) {
	for _, k := range knobs {
		s, err := ParseServe(nil, env(map[string]string{envName(k.flag): k.value}))
		if err != nil {
			t.Fatalf("%s: %v", envName(k.flag), err)
		}
		if got := k.got(s); !reflect.DeepEqual(got, k.want) {
			t.Errorf("%s=%s: got %v, want %v", envName(k.flag), k.value, got, k.want)
		}
	}
	s, err := ParseServe([]string{"-max-conns=5"}, env(map[string]string{"GAWK_MMPROXY_MAX_CONNS": "9"}))
	if err != nil || s.MaxConns != 5 {
		t.Errorf("flag over env: %d, %v", s.MaxConns, err)
	}
	r, err := ParseRoutes(nil, env(map[string]string{"GAWK_MMPROXY_ROUTE_TABLE": "300"}))
	if err != nil || r.RouteTable != 300 {
		t.Errorf("routes env twin: %+v, %v", r, err)
	}
}

func TestValidation(t *testing.T) {
	for _, args := range [][]string{
		{"-listen", "nope"}, {"-ops-listen", "x"}, {"-murmur-port", "0"}, {"-murmur-port", "70000"},
		{"-route-table", "0"}, {"-route-table", "254"}, {"-route-table", "x"},
		{"-header-timeout", "10ms"}, {"-header-timeout", "1m"}, {"-max-conns", "0"},
		{"-log-level", "loud"}, {"-log-format", "xml"}, {"extra"},
	} {
		if _, err := ParseServe(args, env(nil)); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	if _, err := ParseServe(nil, env(map[string]string{"GAWK_MMPROXY_REQUIRE_ROUTES": "maybe"})); err == nil || !strings.Contains(err.Error(), "GAWK_MMPROXY_REQUIRE_ROUTES") {
		t.Errorf("a bad boolean env: %v", err)
	}
}
