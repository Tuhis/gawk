// Package relay fetches a broadcast's cached keyframe from the relay pod that
// holds its origin (docs/71 D1, §9.2): one GET on the ops listener, the
// StreamFrame verbatim, under the screening token — never the admin token,
// which the sampler does not hold.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-server/screening"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// ErrNotFound is the relay's 404: no hub, no cached keyframe, or no active
// publisher — the origin moved or the broadcast ended or went away between
// target refreshes. It is not a sample (D20).
var ErrNotFound = errors.New("relay: no keyframe")

// Fetcher reads keyframes.
type Fetcher struct {
	// Client must not follow redirects (New sets that up): the bearer must
	// reach the relay and nothing else.
	Client *http.Client
	Token  string
}

// New builds a Fetcher whose requests time out after timeout.
func New(token string, timeout time.Duration) *Fetcher {
	return &Fetcher{
		Token: token,
		Client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Keyframe is one fetch's outcome. NotModified means the relay still holds
// the frame named by the If-None-Match tag: the caller repeats that frame's
// score (D20).
type Keyframe struct {
	NotModified bool
	// Frame is the StreamFrame message; nil when NotModified. Frame bytes:
	// never logged, written or exposed (D8).
	Frame []byte
	// Epoch is the publisher session the frame belongs to (§9.1).
	Epoch string
	Codec string
	ETag  string
}

// Fetch GETs relay+KeyframePath(id). etag, when non-empty, is sent as
// If-None-Match.
func (f *Fetcher) Fetch(ctx context.Context, relayBase, id, etag string) (Keyframe, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(relayBase, "/")+screening.KeyframePath(id), nil)
	if err != nil {
		return Keyframe{}, fmt.Errorf("relay: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.Token)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return Keyframe{}, fmt.Errorf("relay: %w", err)
	}
	defer resp.Body.Close()
	k := Keyframe{
		Epoch: resp.Header.Get(screening.HeaderPublisherEpoch),
		Codec: resp.Header.Get(screening.HeaderCodec),
		ETag:  resp.Header.Get("ETag"),
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		k.NotModified = true
		return k, nil
	case http.StatusNotFound:
		return Keyframe{}, ErrNotFound
	default:
		return Keyframe{}, fmt.Errorf("relay: unexpected status %d", resp.StatusCode)
	}
	if k.Epoch == "" {
		return Keyframe{}, errors.New("relay: response carries no publisher epoch")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, wire.MaxKeyframeBytes+1))
	if err != nil {
		return Keyframe{}, fmt.Errorf("relay: read: %w", err)
	}
	if len(body) > wire.MaxKeyframeBytes {
		return Keyframe{}, fmt.Errorf("relay: keyframe larger than %d bytes", wire.MaxKeyframeBytes)
	}
	k.Frame = body
	return k, nil
}
