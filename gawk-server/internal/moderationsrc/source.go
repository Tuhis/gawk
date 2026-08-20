// Package moderationsrc feeds a moderation.Set from the configured ban
// source (R39 AP2, docs/42 §4.3): a Kubernetes informer on Ban CRs, a JSON
// file, or nothing at all.
//
// It is deliberately NOT part of the public gawk-server/moderation package.
// That one is a contract shared with gawk-admin and must stay
// dependency-light; this one drags in client-go informers, signal handling
// and the filesystem, none of which the admin service wants from it.
package moderationsrc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// Kind is the parsed form of -moderation-source.
type Kind string

const (
	// KindOff constructs nothing: the Set stays empty and every publish-path
	// check is a cheap miss. The default, so a relay that predates R39 and a
	// relay with moderation off behave identically.
	KindOff Kind = "off"
	// KindK8s watches Ban CRs in POD_NAMESPACE. Independent of -cluster-mode
	// (docs/42 §4.3): enforcement is not a federation feature.
	KindK8s Kind = "k8s"
	// KindFile reloads a JSON array of moderation.Records from disk — the
	// dev/compose lane (docs/42 §4.14).
	KindFile Kind = "file"
)

// Parse splits a -moderation-source value into its kind and, for the file
// form, its path. Exported so internal/config validates with the same
// parser the source itself uses — a value that parses at startup is a value
// Start can honour.
func Parse(raw string) (Kind, string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.EqualFold(s, string(KindOff)) {
		return KindOff, "", nil
	}
	if strings.EqualFold(s, string(KindK8s)) {
		return KindK8s, "", nil
	}
	if len(s) >= 5 && strings.EqualFold(s[:5], "file:") {
		path := strings.TrimSpace(s[5:])
		if path == "" {
			return "", "", fmt.Errorf("invalid moderation source %q: file: needs a path", raw)
		}
		return KindFile, path, nil
	}
	return "", "", fmt.Errorf("invalid moderation source %q: want off, k8s or file:<path>", raw)
}

// Defaults. The sync timeout is the watchdog behind the docs/42 §6 residual
// risk: a relay cold-starting while the API server is unreachable must SAY
// it is enforcing nothing rather than look healthy and silently fail open.
const (
	defaultSyncTimeout = 30 * time.Second
	defaultResync      = 5 * time.Minute
	defaultPoll        = time.Second
)

// Options configures Start.
type Options struct {
	// Source is the raw -moderation-source value.
	Source string
	// Set is the destination; required.
	Set *moderation.Set
	// Log is required.
	Log *slog.Logger

	// OnBanAdded is the actuation trigger (R39 AP3, docs/42 §4.3): called
	// once per record whenever a source applies one — an informer add/update,
	// AND every record of a file reload or an informer resync. It fires for
	// records that were already in force, so the handler must be idempotent;
	// that is deliberate, because "already applied" and "newly applied" are
	// not distinguishable from a resync and the safe direction is to re-kill
	// a broadcast that is already gone (a no-op) rather than to miss one.
	//
	// Never called for a REMOVED ban: lifting a ban does not resurrect
	// anything, so there is nothing to actuate.
	//
	// Called from the source's own goroutine, so a slow handler delays that
	// source's next event — kills are synchronous by design (a ban that
	// returns before the broadcast is dead is a ban an operator cannot
	// trust), and terminate's work is bounded.
	OnBanAdded func(moderation.Record)

	// Namespace overrides POD_NAMESPACE for the k8s source (tests).
	Namespace string
	// PollInterval overrides the file source's mtime poll cadence (tests).
	PollInterval time.Duration
	// SyncTimeout overrides how long the k8s source waits for the informer's
	// first LIST before warning that it is enforcing an empty ban set.
	SyncTimeout time.Duration
	// ResyncInterval overrides the k8s source's store→Set reconcile cadence.
	ResyncInterval time.Duration
}

// Start brings up the configured source and returns once it is running (or
// immediately, for "off"). Background work stops when ctx is cancelled.
//
// Errors are startup errors — a source that cannot be constructed fails the
// process rather than leaving the relay silently un-enforcing. Losing contact
// with an already-constructed source is NOT an error: it is logged and
// retried, per docs/42 §6.
func Start(ctx context.Context, opts Options) error {
	kind, path, err := Parse(opts.Source)
	if err != nil {
		return err
	}
	if opts.Set == nil || opts.Log == nil {
		return fmt.Errorf("moderationsrc: Set and Log are required")
	}
	switch kind {
	case KindOff:
		return nil
	case KindFile:
		return startFile(ctx, path, opts)
	case KindK8s:
		return startK8s(ctx, opts)
	default:
		// Unreachable: Parse rejects everything else.
		return fmt.Errorf("moderationsrc: unhandled source kind %q", kind)
	}
}
