package kube

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/client-go/dynamic"
)

// PopReconcilerOptions configure the named tier's reconciler (R74, docs/76
// item 38): cert-manager CertificateRequests and external-dns DNSEndpoints
// for every Pop with spec.namedAddress.enabled.
//
// This is the Wave 2 seam. W3-named fills the reconciler in this file and
// popcerts_test.go; main.go already constructs it under -pops and starts Run
// on the leader, so that wave touches no wiring.
type PopReconcilerOptions struct {
	// Pops reads Pop CRs and writes the four tls status paths gawk-admin
	// owns (PopClient.PatchTLSStatus). Required.
	Pops *PopClient
	// Dynamic reaches the cert-manager and external-dns resources, which have
	// no Go dependency here (docs/76 §9). nil until the named tier is filled.
	Dynamic dynamic.Interface
	// Namespace is where the CertificateRequests and DNSEndpoints live.
	Namespace string
	// Zone and Issuer are -pop-dns-zone and -pop-cert-issuer ("kind/name").
	// Both empty is pinned-only: Run then has nothing to do.
	Zone, Issuer string
	// DNSTTL is -pop-dns-ttl, the DNSEndpoint record's TTL.
	DNSTTL time.Duration
	Log    *slog.Logger
}

// PopReconciler is the named tier's leader-only loop.
type PopReconciler struct {
	opts PopReconcilerOptions
}

// NewPopReconciler builds the reconciler. It does no I/O.
func NewPopReconciler(opts PopReconcilerOptions) *PopReconciler {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	return &PopReconciler{opts: opts}
}

// Run reconciles until ctx is cancelled — leadership lost or shutdown —
// and returns then. Today it only waits: issuance and DNS are W3-named's.
func (r *PopReconciler) Run(ctx context.Context) {
	<-ctx.Done()
}
