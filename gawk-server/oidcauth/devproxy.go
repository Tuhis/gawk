package oidcauth

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// DevProxyPath is where the dev-only IdP reverse proxy is mounted. Every
// binary that offers it mounts it here, so the docs/41 compose lane can put
// each one's issuer at <externalUrl>/idp and get the identical route.
const DevProxyPath = "/idp/"

// NewDevProxy reverse-proxies /idp/* to the dev IdP at target (the
// -dev-oidc-proxy flag, docs/42 §11.1). Mount the result at DevProxyPath.
//
// It exists for exactly one URL problem: the SPA fetches the issuer's
// discovery document FROM THE BROWSER and the server fetches the same URL from
// inside its container, so the issuer must resolve in both worlds. With the
// issuer at <externalUrl>/idp, the browser reaches it through the published
// port and the server through its own listener — the Keycloak
// frontend/backchannel split, answered with one path instead of two knobs.
//
// Dev only (docs/41): the flag that enables it is deliberately absent from
// every chart, and a caller that mounts it should say so loudly in its log on
// every startup.
func NewDevProxy(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("not a base URL: %q", target)
	}
	prefix := strings.TrimSuffix(DevProxyPath, "/")
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
		},
	}, nil
}
