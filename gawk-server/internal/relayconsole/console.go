package relayconsole

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// maxActionBody bounds a POST body: a pairing string is well under 200
// bytes, everything else smaller.
const maxActionBody = 4 << 10

// pairTimeout bounds one pairing exchange with the home fleet.
const pairTimeout = 30 * time.Second

//go:embed page
var pageFS embed.FS

// consoleCSP is the console page's policy: its own script and style only,
// never framed (a frame could clickjack Pair or the firewall button).
const consoleCSP = "default-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// Listen opens the console's TCP listener. It refuses any address that is
// not loopback — a wildcard, a LAN address, a name other than localhost —
// so no -console-addr value can expose the console (docs/76 PP3).
func Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("relayconsole: console address %q: %w", addr, err)
	}
	if !loopbackHost(host) {
		return nil, fmt.Errorf("relayconsole: console address %q is not loopback; the console never listens beyond this machine", addr)
	}
	network := "tcp"
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		// "localhost" could resolve to anything in a hostile hosts file;
		// bind the literal instead.
		_, port, _ := net.SplitHostPort(addr)
		addr, network = net.JoinHostPort("127.0.0.1", port), "tcp4"
	}
	l, err := net.Listen(network, addr)
	if err != nil {
		return nil, fmt.Errorf("relayconsole: console listener: %w", err)
	}
	if ap, err := netip.ParseAddrPort(l.Addr().String()); err != nil || !ap.Addr().Unmap().IsLoopback() {
		l.Close()
		return nil, fmt.Errorf("relayconsole: console bound %s, which is not loopback", l.Addr())
	}
	return l, nil
}

// Handler serves the console: the page and its API at /, the embedded web
// app at /app/ with appConfig as its config.js, and /healthz.
//
// Every request must come from a loopback peer and name a loopback Host;
// anything else gets 403 before routing. The Host check is the DNS
// rebinding defence: a page on evil.example that rebinds its name to
// 127.0.0.1 has a loopback peer, but its Host header still names
// evil.example. State changes are POSTs that must also be same-origin
// JSON, which a cross-site page cannot send without a CORS preflight the
// console never answers.
func Handler(p Provider, app fs.FS, appConfig func() []byte) http.Handler {
	mux := http.NewServeMux()
	page, _ := fs.Sub(pageFS, "page")

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, page, "index.html")
	})
	mux.HandleFunc("GET /console.js", func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, page, "console.js")
	})
	mux.HandleFunc("GET /console.css", func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, page, "console.css")
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, newView(p))
	})
	mux.HandleFunc("GET /qr.svg", func(w http.ResponseWriter, _ *http.Request) {
		link, _ := JoinQR(p.State())
		if link == "" {
			http.Error(w, "no join link yet", http.StatusNotFound)
			return
		}
		q, err := EncodeQR([]byte(link))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(q.SVG())
	})

	action(mux, "/api/pair", func(r *http.Request, body []byte) error {
		var in struct {
			Pairing string `json:"pairing"`
		}
		if err := decode(body, &in); err != nil {
			return err
		}
		s := strings.TrimSpace(in.Pairing)
		if s == "" {
			return badRequest("paste the pairing string from the admin portal")
		}
		ctx, cancel := context.WithTimeout(r.Context(), pairTimeout)
		defer cancel()
		return p.Pair(ctx, s)
	})
	action(mux, "/api/standalone", func(*http.Request, []byte) error { return p.RunStandalone() })
	action(mux, "/api/lan", func(_ *http.Request, body []byte) error {
		var in struct {
			Addr string `json:"addr"`
		}
		if err := decode(body, &in); err != nil {
			return err
		}
		var a netip.Addr
		if s := strings.TrimSpace(in.Addr); s != "" && !strings.EqualFold(s, "auto") {
			var err error
			if a, err = netip.ParseAddr(s); err != nil {
				return badRequest(fmt.Sprintf("%q is not an IP address", s))
			}
		}
		return p.SetLANOverride(a)
	})
	budget := func(set func(Budget) error) func(*http.Request, []byte) error {
		return func(_ *http.Request, body []byte) error {
			var in struct {
				Mbps    int `json:"mbps"`
				Streams int `json:"streams"`
			}
			if err := decode(body, &in); err != nil {
				return err
			}
			if in.Mbps < 0 || in.Streams < 0 {
				return badRequest("a budget cannot be negative")
			}
			return set(Budget{Mbps: in.Mbps, Streams: in.Streams})
		}
	}
	action(mux, "/api/uplink", budget(p.LowerUplink))
	action(mux, "/api/downlink", budget(p.LowerDownlink))
	if fa, ok := p.(FirewallAllower); ok && FirewallSupported {
		action(mux, "/api/firewall", func(r *http.Request, _ []byte) error {
			// The UAC prompt waits for the host; give them time.
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			defer cancel()
			return fa.AllowFirewall(ctx)
		})
	}

	appHandler := http.StripPrefix("/app", appServer(app, appConfig))
	mux.Handle("GET /app/", appHandler)
	mux.Handle("GET /app", http.RedirectHandler("/app/", http.StatusMovedPermanently))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackPeer(r.RemoteAddr) || !loopbackHostHeader(r.Host) {
			http.Error(w, "the gawk-relay console answers this machine only", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

// view is GET /api/state: the State plus what the console derives from it.
type view struct {
	State
	Join struct {
		URL  string `json:"url"`
		Tier string `json:"tier"`
	} `json:"join"`
	ServerLink string `json:"serverLink,omitempty"`
	// Firewall: the Allow on this network button can act.
	Firewall bool `json:"firewall"`
}

func newView(p Provider) view {
	v := view{State: p.State()}
	if v.Live == nil {
		v.Live = []LiveRow{}
	}
	if v.LanCandidates == nil {
		v.LanCandidates = []netip.Addr{}
	}
	v.Join.URL, v.Join.Tier = JoinQR(v.State)
	v.ServerLink = ServerLink(v.State)
	_, fa := p.(FirewallAllower)
	v.Firewall = fa && FirewallSupported
	return v
}

// servePage writes one embedded console file with the page's headers.
func servePage(w http.ResponseWriter, r *http.Request, page fs.FS, name string) {
	b, err := fs.ReadFile(page, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(name[strings.LastIndexByte(name, '.'):])
	if strings.HasSuffix(name, ".js") {
		ct = "text/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Security-Policy", consoleCSP)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// appServer serves the embedded gawk-app, with the generated config.js.
func appServer(app fs.FS, appConfig func() []byte) http.Handler {
	files := http.FileServerFS(app)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config.js" {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(appConfig())
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			// The shell must pick up a new config.js and build.
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// errBadRequest marks a request the page got wrong (400); every other
// error is the provider's refusal, also shown to the host as is.
type errBadRequest string

func (e errBadRequest) Error() string { return string(e) }

func badRequest(msg string) error { return errBadRequest(msg) }

func decode(body []byte, v any) error {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return badRequest("the request is not valid JSON")
	}
	return nil
}

// action registers a same-origin JSON POST endpoint.
func action(mux *http.ServeMux, path string, fn func(r *http.Request, body []byte) error) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, errBody("POST only"))
			return
		}
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, errBody("cross-origin request refused"))
			return
		}
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, errBody("send application/json"))
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxActionBody))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("request too large"))
			return
		}
		if err := fn(r, body); err != nil {
			// Every refusal is the host's to read: a provider error says
			// what to fix ("pairing string expired"), a bad request what
			// was wrong with it.
			var br errBadRequest
			code := http.StatusBadRequest
			if !errors.As(err, &br) && errors.Is(err, context.DeadlineExceeded) {
				code = http.StatusGatewayTimeout
			}
			writeJSON(w, code, errBody(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	})
}

func errBody(msg string) any { return struct{ Error string }{msg} }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// sameOrigin accepts a POST from the console's own page: Origin, when the
// browser sends one, must be http://<this Host>; Sec-Fetch-Site, when sent,
// must be same-origin (or none, a typed-in request).
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Scheme != "http" || u.Path != "" && u.Path != "/" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) && loopbackHostHeader(u.Host)
}

// loopbackPeer: the TCP peer is this machine.
func loopbackPeer(remote string) bool {
	ap, err := netip.ParseAddrPort(remote)
	return err == nil && ap.Addr().Unmap().IsLoopback()
}

// loopbackHostHeader: a Host (or Origin host) naming this machine, with or
// without a port.
func loopbackHostHeader(h string) bool {
	if h == "" {
		return false
	}
	host := h
	if hh, _, err := net.SplitHostPort(h); err == nil {
		host = hh
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		host = h[1 : len(h)-1]
	}
	return loopbackHost(host)
}

// loopbackHost: localhost (any case, with or without the root dot) or a
// loopback literal. Subdomains of localhost are refused: resolvers other
// than the browser's may send them anywhere.
func loopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Zone() == "" && a.Unmap().IsLoopback()
}
