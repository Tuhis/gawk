package api_test

import (
	"net/http"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/config"
)

// withStrictRequireRole makes the injected role check refuse an empty role
// list the way production's oidcauth.Verifier.RequireAnyRole does: by
// panicking while the routes are wired.
func withStrictRequireRole() harnessOption {
	return func(o *api.Options, _ *harness) {
		inner := o.RequireRole
		o.RequireRole = func(roles ...string) func(http.Handler) http.Handler {
			if len(roles) == 0 {
				panic("RequireAnyRole(): with no required role every valid token would be admitted")
			}
			return inner(roles...)
		}
	}
}

// R40's three sampler routes require only the flagger role. With that role
// off they resolved to an empty role list, and the production role check
// panics on one — inside Routes(), so the portal crash-looped at start-up
// (PR #500 review). Config now refuses -screening with the role off; the
// route table fails closed regardless: a route no configured role can reach
// is not registered, and answers the catch-all's 404 as a feature-off route
// does — even to a token carrying every role.
func TestARouteNoRoleCanReachIsNotServed(t *testing.T) {
	h := newHarnessWithoutPostgres(t, withScreening(),
		withConfig(func(c *config.Config) { c.FlaggerRole = "" }),
		withStrictRequireRole())
	h.identity.Roles = []string{"operator", "flagger", "rooms-reader", "rooms-manager"}
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/content-flags"},
		{http.MethodGet, "/api/v1/screening/targets"},
		{http.MethodPost, "/api/v1/screening/samples"},
	} {
		if code := h.errorCode(r.method, r.path, nil, http.StatusNotFound); code != api.CodeNotFound {
			t.Errorf("%s %s: error code %q, want %q", r.method, r.path, code, api.CodeNotFound)
		}
	}
	// The operator's screening routes still are.
	if status, _ := h.raw(http.MethodGet, "/api/v1/content-flags", nil); status == http.StatusNotFound {
		t.Error("GET /api/v1/content-flags went missing with the flagger role off")
	}
}
