package mumble

import (
	"errors"
	"fmt"
	"strings"
)

// Typed failures (CODE-REVIEW.md: type your failure phases). The bridge maps
// each to a 4016 reason or a 4015 removal; a caller branches with errors.As /
// errors.Is, never on a message.

// RejectError is murmur's Reject: it refused the login.
type RejectError struct {
	Type   RejectType
	Reason string
}

func (e *RejectError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("mumble: rejected: %s", e.Type)
	}
	return fmt.Sprintf("mumble: rejected: %s: %s", e.Type, e.Reason)
}

// DialError is a failure to reach murmur at all: DNS, TCP or the TLS
// handshake with a server that answered.
type DialError struct {
	Err error
}

func (e *DialError) Error() string { return "mumble: dial: " + e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

// ErrRefused is murmur closing the connection before ServerSync without a
// Reject. In practice that is the autoban (murmur drops a banned address's
// connection without a word, docs/80 §5) or a server that will not take
// new connections.
var ErrRefused = errors.New("mumble: connection closed before login completed (autobanned or refused)")

// ErrClosed is returned by operations on a client that has closed.
var ErrClosed = errors.New("mumble: connection closed")

// RemovedError ends a client that murmur removed: a kick or a ban (UserRemove
// naming our own session).
type RemovedError struct {
	Ban bool
	// Actor is the session that removed us, when HasActor.
	Actor    uint32
	HasActor bool
	Reason   string
}

// Ghost reports murmur's "ghost" removal: a newer login with the same name
// from the same address (or certificate) replaced this one. Murmur sends it
// with no actor and a fixed reason; it is not moderation.
func (e *RemovedError) Ghost() bool {
	return !e.HasActor && !e.Ban && strings.Contains(strings.ToLower(e.Reason), "another device")
}

func (e *RemovedError) Error() string {
	what := "kicked"
	if e.Ban {
		what = "banned"
	}
	if e.Reason == "" {
		return "mumble: " + what
	}
	return fmt.Sprintf("mumble: %s: %s", what, e.Reason)
}

// DeniedError is a PermissionDenied answering one of our requests.
type DeniedError struct {
	PermissionDenied
}

func (e *DeniedError) Error() string {
	s := "mumble: permission denied: " + e.Type.String()
	if e.Reason != "" {
		s += ": " + e.Reason
	}
	return s
}

// ErrChannelNotFound: a channel path does not resolve.
var ErrChannelNotFound = errors.New("mumble: channel not found")
