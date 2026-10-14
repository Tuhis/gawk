package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Tuhis/gawk/gawk-server/screening"
)

// ErrNoFlagRoute is gawk-admin's `503 no_flag_route` (D12): no enabled
// webhook would deliver content_flag.raised. A flag-mode sampler pauses on
// it until the target list says the route is back.
var ErrNoFlagRoute = errors.New("admin: no flag route")

// Tokens is what the client needs of a token source.
type Tokens interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// Client calls gawk-admin.
type Client struct {
	// Base is -admin-url, without a trailing slash.
	Base   string
	Tokens Tokens
	HTTP   *http.Client
}

// Targets reads one shard's target list (D2, D22).
func (c *Client) Targets(ctx context.Context, shard, of int) (screening.TargetsResponse, error) {
	q := url.Values{"shard": {strconv.Itoa(shard)}, "of": {strconv.Itoa(of)}}
	var out screening.TargetsResponse
	status, body, err := c.do(ctx, http.MethodGet, screening.PathTargets+"?"+q.Encode(), nil)
	if err != nil {
		return out, err
	}
	if status != http.StatusOK {
		return out, statusError("targets", status, body)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("admin: targets: %w", err)
	}
	if out.Schema != screening.SchemaTargets || out.Shard != shard || out.Of != of {
		return out, fmt.Errorf("admin: targets: got %s shard %d of %d, asked for %s shard %d of %d",
			out.Schema, out.Shard, out.Of, screening.SchemaTargets, shard, of)
	}
	return out, nil
}

// PostFlag sends one content flag and returns gawk-admin's verdict.
func (c *Client) PostFlag(ctx context.Context, f screening.ContentFlag) (screening.FlagResponse, error) {
	var out screening.FlagResponse
	status, body, err := c.do(ctx, http.MethodPost, screening.PathContentFlags, f)
	if err != nil {
		return out, err
	}
	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
		if err := json.Unmarshal(body, &out); err != nil {
			return out, fmt.Errorf("admin: content flag: %w", err)
		}
		return out, nil
	case http.StatusServiceUnavailable:
		if errorCode(body) == "no_flag_route" {
			return out, ErrNoFlagRoute
		}
	}
	return out, statusError("content flag", status, body)
}

// PostSamples sends one sample batch (D20).
func (c *Client) PostSamples(ctx context.Context, b screening.SampleBatch) error {
	status, body, err := c.do(ctx, http.MethodPost, screening.PathSamples, b)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError("samples", status, body)
	}
	return nil
}

// do sends one request with a bearer token, retrying once with a fresh token
// when gawk-admin answers 401 — a token the IdP rotated keys under, or one
// that expired early.
func (c *Client) do(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var raw []byte
	if payload != nil {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			return 0, nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.Tokens.Token(ctx)
		if err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(raw))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("admin: %s %s: %w", method, path, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return 0, nil, fmt.Errorf("admin: %s %s: %w", method, path, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.Tokens.Invalidate()
			continue
		}
		return resp.StatusCode, body, nil
	}
}

// errorCode reads gawk-admin's {"error":{"code":…}} envelope.
func errorCode(body []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Error.Code
}

func statusError(what string, status int, body []byte) error {
	if code := errorCode(body); code != "" {
		return fmt.Errorf("admin: %s: status %d (%s)", what, status, code)
	}
	return fmt.Errorf("admin: %s: status %d", what, status)
}
