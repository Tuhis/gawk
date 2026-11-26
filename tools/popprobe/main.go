// popprobe serves the R74 PP0 probe page (docs/76 §3, §11 PP0): a static
// page that dials a gawk-relay's /echo by IP literal with
// serverCertificateHashes, the way a party guest's browser does, and prints
// one result line with the method, the browser build and the outcome.
//
// It is a measuring aid, not part of any deployable: stdlib only, so
// `go run .` works on any host with Go, and it cross-compiles for the
// owner's machines. See README.md for the rows it serves.
//
// The relay and its pins come from, in order: -relay and -cert-hash; a
// join link (-join, the console's QR link or a gawk://server link); or the
// console's /api/state (-console), read on every page load so a rotated
// certificate is never stale.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed probe.html
var probeHTML []byte

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type target struct {
	Relay      string   `json:"relay"`
	CertHashes []string `json:"certHashes"`
	Source     string   `json:"source"`
}

// parseJoin reads relay= and certHash= from a console join link
// (https://…/#/?relay=…&certHash=a,b), a view link (#/view/ID?relay=…) or
// a gawk://server?relay=… link.
func parseJoin(link string) (target, error) {
	q := link
	if i := strings.LastIndex(q, "?"); i >= 0 {
		q = q[i+1:]
	}
	v, err := url.ParseQuery(q)
	if err != nil {
		return target{}, fmt.Errorf("join link: %w", err)
	}
	t := target{Relay: v.Get("relay"), Source: "join link"}
	if t.Relay == "" {
		return target{}, errors.New("join link names no relay=")
	}
	t.CertHashes, err = splitHashes(v.Get("certHash"))
	return t, err
}

func splitHashes(s string) ([]string, error) {
	var out []string
	for _, h := range strings.Split(s, ",") {
		h = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(h, ":", "")))
		if h == "" {
			continue
		}
		if !hashRE.MatchString(h) {
			return nil, fmt.Errorf("cert hash %q is not 64 hex digits", h)
		}
		out = append(out, h)
	}
	return out, nil
}

// fromConsole reads the join link gawk-relay's console publishes.
func fromConsole(console string) (target, error) {
	c := http.Client{Timeout: 3 * time.Second}
	r, err := c.Get(strings.TrimRight(console, "/") + "/api/state")
	if err != nil {
		return target{}, fmt.Errorf("console: %w", err)
	}
	defer r.Body.Close()
	var st struct {
		Join struct {
			URL string `json:"url"`
		} `json:"join"`
	}
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		return target{}, fmt.Errorf("console /api/state: %w", err)
	}
	if st.Join.URL == "" {
		return target{}, errors.New("console publishes no join link yet (no LAN address?)")
	}
	t, err := parseJoin(st.Join.URL)
	t.Source = "console " + console
	return t, err
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "popprobe"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(13 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				tmpl.IPAddresses = append(tmpl.IPAddresses, n.IP)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "loopback listener for the page (a secure context on this machine)")
	lanHTTP := flag.String("lan-http", "", "optional plain-HTTP listener, e.g. 10.0.0.5:8091: serves /ping, the Local Network Access fetch control (and the page, as a non-secure context)")
	lanHTTPS := flag.String("lan-https", "", "optional HTTPS listener with a throwaway self-signed certificate, e.g. :8443, for a phone or another machine on the LAN")
	relay := flag.String("relay", "", "relay origin, e.g. https://10.0.0.5:4433 (default: from -join or -console)")
	certHash := flag.String("cert-hash", "", "comma-separated hex SHA-256 pins (with -relay)")
	join := flag.String("join", "", "a gawk-relay join link (the console's QR link) or gawk://server link")
	console := flag.String("console", "http://127.0.0.1:4480", "gawk-relay console, read when neither -relay nor -join is given")
	out := flag.String("out", "", "append every posted result as a JSON line to this file")
	exitAfter := flag.Int("exit-after", 0, "exit after this many posted results; 0 = run until stopped (for scripted headless runs)")
	flag.Parse()

	var fixed *target
	switch {
	case *relay != "":
		hs, err := splitHashes(*certHash)
		if err != nil {
			log.Fatal(err)
		}
		fixed = &target{Relay: strings.TrimRight(*relay, "/"), CertHashes: hs, Source: "flags"}
	case *join != "":
		t, err := parseJoin(*join)
		if err != nil {
			log.Fatal(err)
		}
		fixed = &t
	}
	resolve := func() (target, error) {
		if fixed != nil {
			return *fixed, nil
		}
		return fromConsole(*console)
	}
	if t, err := resolve(); err != nil {
		log.Printf("warning: %v (the page can still take ?relay=&certHash=)", err)
	} else {
		log.Printf("relay %s, %d pin(s), from %s", t.Relay, len(t.CertHashes), t.Source)
	}

	done := make(chan struct{})
	var mu sync.Mutex
	posted := 0

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(probeHTML)
	})
	mux.HandleFunc("GET /config.json", func(w http.ResponseWriter, r *http.Request) {
		t, err := resolve()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(t)
	})
	// /ping is the control: did a request from the page reach this host at
	// all? It is logged on arrival, so a browser that blocks the fetch
	// leaves no line here whatever the page reports.
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		log.Printf("ping %s from %s (Origin %q)", r.Method, r.RemoteAddr, r.Header.Get("Origin"))
		io.WriteString(w, "pong")
	})
	mux.HandleFunc("POST /result", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var res struct {
			Line string `json:"line"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Printf("RESULT %s\n", res.Line)
		mu.Lock()
		defer mu.Unlock()
		if *out != "" {
			if f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				f.Write(append(body, '\n'))
				f.Close()
			}
		}
		w.WriteHeader(http.StatusNoContent)
		if posted++; *exitAfter > 0 && posted == *exitAfter {
			close(done)
		}
	})

	serve := func(addr string, tlsCfg *tls.Config) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatal(err)
		}
		scheme := "http"
		if tlsCfg != nil {
			ln = tls.NewListener(ln, tlsCfg)
			scheme = "https"
		}
		log.Printf("page at %s://%s/", scheme, ln.Addr())
		go func() { log.Fatal(http.Serve(ln, mux)) }()
	}
	serve(*addr, nil)
	if *lanHTTP != "" {
		serve(*lanHTTP, nil)
	}
	if *lanHTTPS != "" {
		cert, err := selfSigned()
		if err != nil {
			log.Fatal(err)
		}
		serve(*lanHTTPS, &tls.Config{Certificates: []tls.Certificate{cert}})
	}
	if *exitAfter > 0 {
		<-done
		time.Sleep(200 * time.Millisecond) // let the 204 reach the page
		return
	}
	select {}
}
