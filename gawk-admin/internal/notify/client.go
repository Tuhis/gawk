package notify

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// maxRedirects bounds a same-origin redirect chain. A receiver that needs more
// than three hops to accept a POST is misconfigured, and the chain is a cheap
// way to burn the dispatcher's request budget.
const maxRedirects = 3

// maxErrorBody is how much of a rejecting receiver's response body is kept for
// last_error. Enough to carry "invalid signature" or a stack-trace's first
// line; far short of a page of HTML.
const maxErrorBody = 512

// maxDrainBody bounds what a SUCCESSFUL response's body is drained of before
// the connection goes back to the pool. Draining lets the connection be
// reused; bounding it means a receiver that answers 200 with a gigabyte cannot
// make the dispatcher read it.
const maxDrainBody = 1 << 16

// errCrossOriginRedirect is what stops a signature from leaking. It is a
// distinct error so the log line and the portal's last_error both name the
// actual problem rather than a generic transport failure.
var errCrossOriginRedirect = errors.New("refusing to follow a redirect to a different origin")

// newHTTPClient builds the outbound client.
//
// Two properties are security-relevant, not tuning:
//
//   - **A bounded timeout.** The dispatcher is a singleton on the leader; a
//     receiver that accepts the connection and never answers must not be able
//     to stall the whole notification pipe.
//   - **No cross-origin redirects.** net/http strips Authorization across
//     hosts but forwards every other header, so a webhook URL that 302s
//     elsewhere would hand X-Gawk-Signature — a valid MAC under the
//     operator's key — to a host the operator never configured. Same-origin
//     hops (http→https on the same host, a trailing-slash canonicalization)
//     stay allowed because they are the same trust boundary the operator
//     already chose.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if !sameOrigin(via[0].URL, req.URL) {
				return fmt.Errorf("%w: %s → %s", errCrossOriginRedirect,
					origin(via[0].URL), origin(req.URL))
			}
			return nil
		},
	}
}

// sameOrigin compares scheme and host (port included), the pair that decides
// who receives the signature. A scheme UPGRADE on the same host is allowed —
// http→https is a receiver hardening its own endpoint, and refusing it would
// punish the correct move — but a downgrade is not.
func sameOrigin(from, to *url.URL) bool {
	if !strings.EqualFold(from.Host, to.Host) {
		return false
	}
	if strings.EqualFold(from.Scheme, to.Scheme) {
		return true
	}
	return strings.EqualFold(from.Scheme, "http") && strings.EqualFold(to.Scheme, "https")
}

func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

// cleanURLError reduces a transport error's URL to its origin.
//
// A webhook URL is frequently a CREDENTIAL — a Slack incoming-webhook path, an
// ntfy topic, a signed callback query — and net/http puts the whole URL into
// every *url.Error. That string becomes the delivery's last_error and the
// dispatcher's log line, both of which travel further than the OIDC-gated
// portal where the URL is legitimately shown. internal/config already keeps
// webhook URLs out of the startup log (LogAttrs logs names only); this is the
// same rule on the error path.
//
// Scheme and host survive, so a DNS or TLS failure is still debuggable; the
// path and query — where the token lives — do not.
func cleanURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	parsed, perr := url.Parse(ue.URL)
	if perr != nil || parsed.Host == "" {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return fmt.Errorf("%s %s: %w", ue.Op, origin(parsed), ue.Err)
}

// readErrorBody returns a bounded, printable snippet of a rejecting receiver's
// response for last_error, which the portal renders (§4.10 — "a failed
// delivery must be SEEN"). Control characters are dropped so a receiver cannot
// write terminal escapes into an operator's log or a newline-mangled row.
func readErrorBody(body io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(body, maxErrorBody))
	var b strings.Builder
	lastSpace := false
	for _, r := range string(raw) {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		if !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}
