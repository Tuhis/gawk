package serve

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// -pops off builds nothing; -pops with -pops-file builds the manager over
// the file and serves it on the admin route (the manager's AdminPops).
func TestWirePops(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := hub.NewRegistry(log, hub.Options{})
	off := config.Config{}
	m, err := wirePops(t.Context(), Wired{Registry: r, Server: transport.New(off, r, nil, log, nil), Cfg: off}, log)
	if err != nil || m != nil {
		t.Fatalf("-pops off: %v %v", m, err)
	}

	path := filepath.Join(t.TempDir(), "pops.yaml")
	body := "- name: lan-party\n  uid: 0a1b2c3d-4e5f-4a6b-8c7d-9e8f7a6b5c4d\n  credentialSHA256: " + pops.CredentialSHA256("c") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	on := config.Config{Pops: true, PopsFile: path, Addr: ":4433", AllowedOrigins: []string{"http://localhost:5173", "https://gawk.example"}}
	m, err = wirePops(t.Context(), Wired{Registry: r, Server: transport.New(on, r, nil, log, nil), Cfg: on}, log)
	if err != nil || m == nil {
		t.Fatalf("-pops-file: %v %v", m, err)
	}
	defer m.Close()
	if _, ok := m.Source().Get("lan-party"); !ok {
		t.Fatal("the file's Pop is not in the manager's source")
	}
	if rows := m.AdminPops(); len(rows) != 0 {
		t.Fatalf("AdminPops with no link = %+v", rows)
	}

	if _, err := wirePops(t.Context(), Wired{Registry: r, Server: transport.New(on, r, nil, log, nil),
		Cfg: config.Config{Pops: true}}, log); err == nil {
		t.Fatal("-pops with neither source wired")
	}
}

func TestHomeOriginIsTheFirstHTTPSAllowedOrigin(t *testing.T) {
	for _, tc := range []struct {
		origins []string
		want    string
	}{
		{nil, ""},
		{[]string{"http://localhost:5173"}, ""},
		{[]string{"http://localhost:5173", "https://gawk.ioio.fi", "https://other"}, "https://gawk.ioio.fi"},
	} {
		if got := HomeOrigin(config.Config{AllowedOrigins: tc.origins}); got != tc.want {
			t.Errorf("HomeOrigin(%v) = %q, want %q", tc.origins, got, tc.want)
		}
	}
}
