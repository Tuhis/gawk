package ops

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-mmproxy/internal/listeners"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/metrics"
	"github.com/Tuhis/gawk/gawk-mmproxy/internal/routes"
)

func TestReady(t *testing.T) {
	both := routes.State{V4: true, V6: true}
	for _, c := range []struct {
		name    string
		require bool
		st      routes.State
		stErr   error
		reach   listeners.Reach
		rErr    error
		want    bool
		why     string
	}{
		{"all good", true, both, nil, listeners.Reach{V4: true, V6: true}, nil, true, "ready"},
		{"IPv4 only, no IPv6 routes", true, routes.State{V4: true}, nil, listeners.Reach{V4: true}, nil, true, "ready"},
		{"routes missing", true, routes.State{}, nil, listeners.Reach{V4: true, V6: true}, nil, false, "routes are missing"},
		{"routes unreadable", true, routes.State{}, errors.New("boom"), listeners.Reach{V4: true}, nil, false, "can't read"},
		{"routes not required (the e2e)", false, routes.State{}, nil, listeners.Reach{V4: true}, nil, true, "ready"},
		{"murmur on ::1 only", true, both, nil, listeners.Reach{V6: true}, nil, false, "127.0.0.1 reaches"},
		{"murmur v6 missing with v6 routes", true, both, nil, listeners.Reach{V4: true}, nil, false, "::1 reaches"},
		{"inet_diag refused", true, both, nil, listeners.Reach{}, fmt.Errorf("%w: x", listeners.ErrUnavailable), false, "inet_diag"},
	} {
		o := Options{MurmurPort: 64738, RequireRoutes: c.require,
			Routes:    func() (routes.State, error) { return c.st, c.stErr },
			Reachable: func(uint16) (listeners.Reach, error) { return c.reach, c.rErr }}
		ok, why := Ready(o)
		if ok != c.want || !strings.Contains(why, c.why) {
			t.Errorf("%s: %v %q, want %v %q", c.name, ok, why, c.want, c.why)
		}
	}
}

func TestHandler(t *testing.T) {
	m := metrics.New()
	m.Count(metrics.Proxied)
	m.Count(metrics.Undrained)
	o := Options{MurmurPort: 1, RequireRoutes: true,
		Routes:    func() (routes.State, error) { return routes.State{}, nil },
		Reachable: func(uint16) (listeners.Reach, error) { return listeners.Reach{V4: true}, nil }}
	h := Handler(o, m)
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 503, "/metrics": 200} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
		if path == "/metrics" {
			body := rec.Body.String()
			for _, l := range []string{
				`gawk_mmproxy_connections_total{result="proxied"} 1`,
				`gawk_mmproxy_connections_total{result="undrained"} 1`,
				`gawk_mmproxy_draining{pool="over_cap"} 0`,
				`gawk_mmproxy_active 0`, `gawk_mmproxy_resets_total 0`, `gawk_mmproxy_routes_ok 0`,
			} {
				if !strings.Contains(body, l+"\n") {
					t.Errorf("/metrics lacks %q:\n%s", l, body)
				}
			}
		}
	}
}
