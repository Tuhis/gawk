package serve

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// wirePops installs the moderation reach on a -pops home: revoking a
// -pops-file entry releases its reservations in Wired.Reservations (the
// set the hub's mint reads), with nothing else wired by hand.
func TestWirePopsInstallsTheModerationReach(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := hub.NewRegistry(log, hub.Options{})
	path := filepath.Join(t.TempDir(), "pops.yaml")
	entry := "- name: lan-party\n  uid: 0a1b2c3d-4e5f-4a6b-8c7d-9e8f7a6b5c4d\n  credentialSHA256: " + pops.CredentialSHA256("c") + "\n"
	if err := os.WriteFile(path, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Pops: true, PopsFile: path, Addr: ":4433"}
	res := popsrv.NewMemReservations()
	if got, _ := res.Reserve(context.Background(), "ABC234", "lan-party", 0); got != popsrv.ReserveOK {
		t.Fatal(got)
	}
	m, err := wirePops(t.Context(), Wired{Registry: r, Server: transport.New(cfg, r, nil, log, nil), Cfg: cfg,
		Bans: moderation.NewSet(), Reservations: res}, log)
	if err != nil || m == nil {
		t.Fatalf("wirePops: %v %v", m, err)
	}
	defer m.Close()
	time.Sleep(50 * time.Millisecond)
	if len(res.List("lan-party")) != 1 {
		t.Fatal("a live PoP's reservation was released")
	}
	// A later mtime, so the poll sees the edit.
	if err := os.WriteFile(path, []byte(entry+"  revoked: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, later, later)
	deadline := time.Now().Add(10 * time.Second)
	for len(res.List("lan-party")) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the revoked PoP's reservation is still there")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
