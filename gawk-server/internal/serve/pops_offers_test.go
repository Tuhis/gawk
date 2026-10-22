package serve

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// With -pops on, wirePops installs the offer matcher and its counters.
func TestWirePopsInstallsOffers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := hub.NewRegistry(log, hub.Options{})
	path := filepath.Join(t.TempDir(), "pops.yaml")
	body := "- name: lan-party\n  uid: 0a1b2c3d-4e5f-4a6b-8c7d-9e8f7a6b5c4d\n  credentialSHA256: " + pops.CredentialSHA256("c") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Pops: true, PopsFile: path, Addr: ":4433", PopOfferDefer: 2 * time.Second}
	prom := prometheus.NewRegistry()
	m, err := wirePops(t.Context(), Wired{Registry: r, Server: transport.New(cfg, r, nil, log, nil), Cfg: cfg, Prom: prom}, log)
	if err != nil || m == nil {
		t.Fatalf("wirePops: %v %v", m, err)
	}
	defer m.Close()
	families, err := prom.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"gawk_pop_offers_total": false, "gawk_pop_offer_defers_total": false}
	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("%s not registered", name)
		}
	}
}
