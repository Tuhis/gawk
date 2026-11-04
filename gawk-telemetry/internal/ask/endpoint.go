package ask

import (
	"net"
	"net/url"
	"strings"
)

// DefaultAnthropicBaseURL is where the anthropic provider sends a question
// unless -ask-base-url says otherwise.
const DefaultAnthropicBaseURL = "https://api.anthropic.com"

// Endpoint reports the host a provider at baseURL sends questions to, and
// whether that host is outside the cluster — so the console can say, under
// the box, that the question leaves it (docs/84 §5). In-cluster means a
// loopback or private address, a single-label name, or a Kubernetes service
// name ending in .svc or .cluster.local; anything else is treated as outside,
// which is the safe reading. (A bare `name.namespace` cannot be told from a
// public two-label domain, so it reads as outside: spell it with .svc.)
func Endpoint(baseURL string) (host string, external bool) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return baseURL, true
	}
	host = u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return host, !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	switch {
	case h == "localhost", !strings.Contains(h, "."),
		strings.HasSuffix(h, ".svc"), strings.HasSuffix(h, ".cluster.local"):
		return host, false
	}
	return host, true
}
