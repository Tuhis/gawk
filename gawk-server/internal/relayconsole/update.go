package relayconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ReleaseManifestURL is gawk-relay's release manifest (docs/46 D5) on the
// repository's badges branch, which the console's "update available" line
// reads (docs/76 §7.1 Health). Nothing is ever downloaded or installed
// from it: the relay has no self-update.
const ReleaseManifestURL = "https://raw.githubusercontent.com/Tuhis/gawk/badges/releases/gawk-relay/latest.json"

// maxManifest bounds the manifest read; the real one is under a kilobyte.
const maxManifest = 64 << 10

// CheckUpdate fetches the release manifest at url and returns its version
// when it is newer than current, else "". A dev build ("dev", or anything
// that is not X.Y.Z) never sees an update.
func CheckUpdate(ctx context.Context, c *http.Client, url, current string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	rsp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("relayconsole: release manifest: HTTP %d", rsp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(rsp.Body, maxManifest+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxManifest {
		return "", errors.New("relayconsole: release manifest is too large")
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("relayconsole: release manifest: %w", err)
	}
	if newerVersion(current, m.Version) {
		return m.Version, nil
	}
	return "", nil
}

// newerVersion: latest is a strictly newer X.Y.Z than current. A current
// with a pre-release suffix or no version at all compares as nothing.
func newerVersion(current, latest string) bool {
	c, ok1 := parseVersion(current)
	l, ok2 := parseVersion(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return v, false
		}
		v[i] = n
	}
	return v, true
}
