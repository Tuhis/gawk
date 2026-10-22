package relayconsole

import (
	"slices"
	"testing"
)

// docs/76 §6.1 item 2: the console opens in the default browser on the
// first interactive start — per OS, and never somewhere with no desktop.
func TestBrowserCommand(t *testing.T) {
	const u = "http://127.0.0.1:4480/"
	for _, c := range []struct {
		goos    string
		env     map[string]string
		want    []string
		wantNil bool
	}{
		{goos: "windows", want: []string{"rundll32", "url.dll,FileProtocolHandler", u}},
		{goos: "darwin", want: []string{"open", u}},
		{goos: "linux", env: map[string]string{"DISPLAY": ":0"}, want: []string{"xdg-open", u}},
		{goos: "linux", env: map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, want: []string{"xdg-open", u}},
		{goos: "linux", wantNil: true}, // headless: a server or a container
		{goos: "freebsd", env: map[string]string{"DISPLAY": ":0"}, want: []string{"xdg-open", u}},
		{goos: "plan9", wantNil: true},
	} {
		got := BrowserCommand(c.goos, func(k string) string { return c.env[k] }, u)
		if c.wantNil {
			if got != nil {
				t.Errorf("%s %v: %q, want none", c.goos, c.env, got)
			}
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s %v: %q, want %q", c.goos, c.env, got, c.want)
		}
	}
	// Only the console's own loopback URL is ever handed to a shell helper.
	for _, bad := range []string{"https://evil.example/", "file:///etc/passwd", "http://127.0.0.1:4480/&calc", "javascript:alert(1)"} {
		if got := BrowserCommand("windows", func(string) string { return "" }, bad); got != nil {
			t.Errorf("%q: %q, want refused", bad, got)
		}
	}
}
