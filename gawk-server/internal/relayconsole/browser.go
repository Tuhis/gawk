package relayconsole

import (
	"os/exec"
	"regexp"
)

// consoleURL is the only shape BrowserCommand hands to a shell helper: the
// console's own loopback URL. rundll32 and xdg-open interpret what they
// are given, so nothing else may reach them.
var consoleURL = regexp.MustCompile(`^http://(127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|\[::1\]|localhost):[0-9]{1,5}/[A-Za-z0-9/._-]*$`)

// BrowserCommand is the command that opens u in the default browser on
// goos, or nil where there is none to open: a Linux or BSD host with no
// display (a server, a container), an unknown OS, or a URL that is not the
// console's.
func BrowserCommand(goos string, getenv func(string) string, u string) []string {
	if !consoleURL.MatchString(u) {
		return nil
	}
	switch goos {
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler", u}
	case "darwin":
		return []string{"open", u}
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		if getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
			return nil
		}
		return []string{"xdg-open", u}
	}
	return nil
}

// OpenBrowser runs BrowserCommand without waiting for the browser; it
// reports false when there was nothing to run or it failed to start.
func OpenBrowser(goos string, getenv func(string) string, u string) bool {
	argv := BrowserCommand(goos, getenv, u)
	if argv == nil {
		return false
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return false
	}
	go cmd.Wait() // reap it
	return true
}
