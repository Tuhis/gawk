package relayconsole

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		current, latest string
		want            bool
	}{
		{"1.2.3", "1.2.4", true},
		{"1.2.3", "1.3.0", true},
		{"1.2.3", "2.0.0", true},
		{"v1.2.3", "1.10.0", true},
		{"1.2.3", "1.2.3", false},
		{"1.10.0", "1.9.9", false},
		{"1.2.3", "garbage", false},
		{"dev", "9.9.9", false}, // a dev build has no version to compare
		{"1.2.3-rc.1", "1.2.3", false},
		{"", "1.0.0", false},
	} {
		if got := newerVersion(c.current, c.latest); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

// docs/76 §7.1 Health: "update available" from releases/gawk-relay/latest.json
// (the docs/46 D5 manifest).
func TestCheckUpdate(t *testing.T) {
	body := `{"schema":1,"component":"gawk-server","distribution":"gawk-relay","version":"1.4.0","tag":"gawk-server/v1.4.0","release_url":"https://github.com/Tuhis/gawk/releases/tag/gawk-server/v1.4.0"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte(body))
		case "/big":
			w.Write([]byte(`{"version":"9.9.9","pad":"` + strings.Repeat("x", 100<<10) + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	if v, err := CheckUpdate(ctx, srv.Client(), srv.URL+"/ok", "1.3.2"); err != nil || v != "1.4.0" {
		t.Errorf("older binary: %q, %v; want 1.4.0", v, err)
	}
	if v, err := CheckUpdate(ctx, srv.Client(), srv.URL+"/ok", "1.4.0"); err != nil || v != "" {
		t.Errorf("current binary: %q, %v; want none", v, err)
	}
	if _, err := CheckUpdate(ctx, srv.Client(), srv.URL+"/missing", "1.0.0"); err == nil {
		t.Error("404: want an error")
	}
	if v, err := CheckUpdate(ctx, srv.Client(), srv.URL+"/big", "1.0.0"); err == nil || v != "" {
		t.Errorf("oversized manifest: %q, %v; want an error", v, err)
	}
}
