package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
)

// statusClient reads the gateway's GET /whip/status (D9) for one key, as the
// OBS page does.
type statusClient struct {
	whipURL string
	client  *http.Client
	// every is the poll interval; 0 is 1 s.
	every time.Duration
}

func (c statusClient) get(ctx context.Context, key streamkey.Key) (status.Status, error) {
	cl := c.client
	if cl == nil {
		cl = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.whipURL, "/")+"/status", nil)
	if err != nil {
		return status.Status{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key.String())
	rsp, err := cl.Do(req)
	if err != nil {
		return status.Status{}, err
	}
	defer rsp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(rsp.Body, 64<<10))
	if err != nil {
		return status.Status{}, err
	}
	if rsp.StatusCode != http.StatusOK {
		return status.Status{}, fmt.Errorf("GET /whip/status: %d %s", rsp.StatusCode, strings.TrimSpace(string(body)))
	}
	var st status.Status
	if err := json.Unmarshal(body, &st); err != nil {
		return status.Status{}, fmt.Errorf("GET /whip/status: %w", err)
	}
	return st, nil
}

// waitState polls until the key is live (live=true) or anything but live
// (live=false). A gateway that is not up yet, or a failed poll, is waited out.
func (c statusClient) waitState(ctx context.Context, key streamkey.Key, live bool) error {
	every := c.every
	if every <= 0 {
		every = time.Second
	}
	for {
		st, err := c.get(ctx, key)
		if err == nil && (st.State == status.StateLive) == live {
			return nil
		}
		if !sleep(ctx, every) {
			return ctx.Err()
		}
	}
}
