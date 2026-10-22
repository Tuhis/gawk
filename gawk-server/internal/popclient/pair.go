package popclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// CredentialSecret is the state-directory secret holding the PoP's link
// credential (DPAPI on Windows, 0600 elsewhere: relaystate.SaveSecret).
const CredentialSecret = "pop-credential"

// CredentialPrefix marks a static credential for a -pops-file home, which
// has no pairing (docs/76 item 9): "gawk-cred:<host:port>/<credential>",
// accepted wherever a pairing string is (-pair, the console). The relay
// stores it as is; the first link checks it.
const CredentialPrefix = "gawk-cred:"

// ErrPairSuperseded: a newer Pair replaced this one before it ran.
var ErrPairSuperseded = errors.New("popclient: pairing superseded by a newer one")

// storedCredential is the CredentialSecret's JSON.
type storedCredential struct {
	// Endpoint is home's host:port (from the pairing string).
	Endpoint   string `json:"endpoint"`
	Credential string `json:"credential"`
	// Pop is the Pop's name from home's config (sent as ?pop=, so home
	// checks one Pop instead of scanning).
	Pop string `json:"pop,omitempty"`
	// PairTokenSHA is pops.TokenSHA256 of the string this credential came
	// from, so a -pair still set at the next start is not redeemed again.
	PairTokenSHA string `json:"pairTokenSHA256,omitempty"`
}

func loadCredential(dir string) (*storedCredential, error) {
	if dir == "" {
		return nil, nil
	}
	b, err := relaystate.LoadSecret(dir, CredentialSecret)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st storedCredential
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("popclient: %s: %w", CredentialSecret, err)
	}
	if st.Endpoint == "" || st.Credential == "" {
		return nil, fmt.Errorf("popclient: %s is incomplete", CredentialSecret)
	}
	return &st, nil
}

func saveCredential(dir string, st *storedCredential) error {
	if dir == "" {
		return nil
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return relaystate.SaveSecret(dir, CredentialSecret, b)
}

// pairRequest is one Pair waiting for Run's next pairing dial.
type pairRequest struct {
	endpoint, token string
	once            sync.Once
	done            chan error
}

func (r *pairRequest) finish(err error) {
	if r == nil {
		return
	}
	r.once.Do(func() { r.done <- err; close(r.done) })
}

func (c *Client) failPairing(req *pairRequest, err error) { req.finish(err) }

// Pair redeems a pairing string ("gawk-pair:<host:port>/<token>") with the
// home it names and stores the credential home returns. It blocks until
// the pairing link either delivered the credential (nil; the link then
// carries on as the PoP's link) or was refused (a *PairError with home's
// status and reason, which the console shows). Run must be running.
//
// A "gawk-cred:" string (a -pops-file home's static credential) is stored
// directly and the link redialed with it.
func (c *Client) Pair(ctx context.Context, s string) error {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, CredentialPrefix); ok {
		endpoint, cred, ok := strings.Cut(rest, "/")
		if !ok || endpoint == "" || cred == "" {
			return fmt.Errorf("popclient: malformed %s string", strings.TrimSuffix(CredentialPrefix, ":"))
		}
		st := &storedCredential{Endpoint: endpoint, Credential: cred}
		if err := saveCredential(c.opts.StateDir, st); err != nil {
			return err
		}
		c.mu.Lock()
		c.stored = st
		c.mu.Unlock()
		c.setState(func(s *LinkState) { s.Header, s.Endpoint, s.ByeReason, s.LastStatus = HeaderWaiting, endpoint, "", 0 })
		c.closeLink("credential replaced")
		c.wake()
		return nil
	}
	endpoint, token, err := pops.ParsePairing(s)
	if err != nil {
		return err
	}
	req := &pairRequest{endpoint: endpoint, token: token, done: make(chan error, 1)}
	c.mu.Lock()
	old := c.pending
	c.pending = req
	c.mu.Unlock()
	old.finish(ErrPairSuperseded)
	// A live link holds Run; end it so the pairing dial goes next.
	c.closeLink("pairing")
	c.wake()
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pairAtStart redeems -pair unless the stored credential came from it.
func (c *Client) pairAtStart(ctx context.Context, s string) {
	if _, token, err := pops.ParsePairing(s); err == nil {
		c.mu.Lock()
		st := c.stored
		c.mu.Unlock()
		if st != nil && st.PairTokenSHA == pops.TokenSHA256(token) {
			return
		}
	}
	if err := c.Pair(ctx, s); err != nil && ctx.Err() == nil {
		c.log.Warn("pairing with -pair failed", "err", err)
	}
}

// closeLink ends the live link, if any, so Run's loop moves on.
func (c *Client) closeLink(why string) {
	c.mu.Lock()
	sess := c.ctrl
	c.mu.Unlock()
	if sess != nil {
		_ = sess.CloseWithError(0, why)
	}
}

// rememberPop stores the Pop's name from home's config.
func (c *Client) rememberPop(name string) {
	if name == "" {
		return
	}
	c.mu.Lock()
	st := c.stored
	if st == nil || st.Pop == name {
		c.mu.Unlock()
		return
	}
	nst := *st
	nst.Pop = name
	c.stored = &nst
	c.mu.Unlock()
	if err := saveCredential(c.opts.StateDir, &nst); err != nil {
		c.log.Warn("could not store the PoP name", "err", err)
	}
}
