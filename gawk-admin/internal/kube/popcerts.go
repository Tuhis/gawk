package kube

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// The named tier's two foreign resources (docs/76 §6.11 item 38). Neither
// operator's Go types are a dependency here: both are written as
// unstructured objects, which is all the reconciler needs.
var (
	// CertificateRequestResource is cert-manager's one-shot issuance.
	CertificateRequestResource = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificaterequests"}
	// DNSEndpointResource is external-dns's CRD source (--source=crd).
	DNSEndpointResource = schema.GroupVersionResource{Group: "externaldns.k8s.io", Version: "v1alpha1", Resource: "dnsendpoints"}
)

const (
	// AnnotationCSRSHA256 names the CSR a CertificateRequest was made for
	// (pops.CSRSHA256), so a pass can tell its own request from a stale one.
	AnnotationCSRSHA256 = "gawk.ioio.fi/csr-sha256"

	// DefaultPopCertInterval is the named tier's reconcile period. Issuance
	// is a human waiting at a party, so it is shorter than the ban janitor's.
	DefaultPopCertInterval = 15 * time.Second
	// DefaultFailedRetry is how long a failed or denied CertificateRequest
	// stays before it is deleted so the next pass asks again. cert-manager
	// never retries a CertificateRequest by itself, and Let's Encrypt allows
	// 5 failed validations per hostname per hour.
	DefaultFailedRetry = time.Hour

	// namedLabelAlphabet is the random label's: lowercase letters and digits.
	namedLabelAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	namedLabelLen      = 6
	// longLivedWarn is the leaf lifetime past which the issuer is not the
	// 6-day shortlived profile: then a revoke leaves a valid certificate on
	// the host for its lifetime, which the operator is told (item 38).
	longLivedWarn = 8 * 24 * time.Hour
)

// PopReconcilerOptions configure the named tier's reconciler (R74, docs/76
// item 38): cert-manager CertificateRequests and external-dns DNSEndpoints
// for every Pop with spec.namedAddress.enabled.
type PopReconcilerOptions struct {
	// Pops reads Pop CRs and writes the four tls status paths gawk-admin
	// owns (PopClient.PatchTLSStatus). Required.
	Pops *PopClient
	// Dynamic reaches the cert-manager and external-dns resources, which have
	// no Go dependency here (docs/76 §9).
	Dynamic dynamic.Interface
	// Namespace is where the CertificateRequests and DNSEndpoints live.
	Namespace string
	// Zone and Issuer are -pop-dns-zone and -pop-cert-issuer ("Kind/name").
	// Both empty is pinned-only: Run then has nothing to do.
	Zone, Issuer string
	// DNSTTL is -pop-dns-ttl, the DNSEndpoint record's TTL.
	DNSTTL time.Duration
	Log    *slog.Logger

	// Interval is the reconcile period (DefaultPopCertInterval).
	Interval time.Duration
	// FailedRetry is DefaultFailedRetry's knob (tests).
	FailedRetry time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// NewLabel draws a host label; nil means NewNamedLabel (tests).
	NewLabel func() (string, error)
}

// PopReconciler is the named tier's leader-only loop. Per Pop with the
// named address on, it:
//   - assigns status.tls.host once: p-<6 random>.<zone>, never the PoP's
//     name (CT logs publish every issued name, and the name is the default
//     wall code);
//   - keeps one DNSEndpoint, an A (or AAAA) record for the PoP's LAN
//     address, and deletes it when the PoP is revoked, ended, turned off or
//     gone;
//   - turns a valid status.tls.csr whose hash is not status.tls.issuedForCSR
//     into exactly one CertificateRequest, and on Ready writes the chain,
//     issuedForCSR and notAfter. The link-home pod delivers a chain only
//     when issuedForCSR matches the current CSR, so a renewal is never
//     issued twice and never answered with the previous chain.
type PopReconciler struct {
	opts       PopReconcilerOptions
	issuerKind string
	issuerName string
	kick       chan struct{}

	mu sync.Mutex
	// refused remembers the CSR hash last refused per Pop, so a bad CSR is
	// logged once rather than every pass.
	refused map[string]string
}

// NewPopReconciler builds the reconciler. It does no I/O.
func NewPopReconciler(opts PopReconcilerOptions) *PopReconciler {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	opts.Log = opts.Log.With("component", "pop-named")
	if opts.Interval <= 0 {
		opts.Interval = DefaultPopCertInterval
	}
	if opts.FailedRetry <= 0 {
		opts.FailedRetry = DefaultFailedRetry
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewLabel == nil {
		opts.NewLabel = NewNamedLabel
	}
	if opts.DNSTTL <= 0 {
		opts.DNSTTL = 60 * time.Second
	}
	opts.Zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(opts.Zone)), ".")
	r := &PopReconciler{opts: opts, kick: make(chan struct{}, 1), refused: map[string]string{}}
	if opts.Issuer != "" {
		// config validated it at startup; a bad value here leaves the
		// reconciler idle rather than half-working.
		if k, n, err := config.ParseIssuer(opts.Issuer); err == nil {
			r.issuerKind, r.issuerName = k, n
		} else {
			opts.Log.Error("named tier off: bad issuer", "err", err)
		}
	}
	return r
}

// configured is the named tier being on: a zone, an issuer and the clients.
func (r *PopReconciler) configured() bool {
	return r.opts.Zone != "" && r.issuerName != "" && r.opts.Pops != nil && r.opts.Dynamic != nil
}

// Kick asks for a pass now (non-blocking).
func (r *PopReconciler) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx is cancelled — leadership lost or shutdown —
// and returns then. Pinned-only (no zone or issuer) it only waits.
func (r *PopReconciler) Run(ctx context.Context) {
	if !r.configured() {
		<-ctx.Done()
		return
	}
	t := time.NewTicker(r.opts.Interval)
	defer t.Stop()
	for {
		if err := r.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
			r.opts.Log.Warn("named tier reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kick:
		}
	}
}

// ReconcileOnce is one pass over every Pop. Per-Pop failures are logged
// and the pass goes on; the error is the first one (or the list's).
func (r *PopReconciler) ReconcileOnce(ctx context.Context) error {
	if !r.configured() {
		return nil
	}
	objs, err := r.opts.Pops.List(ctx)
	if err != nil {
		return err
	}
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	hosts := map[string]string{}
	for _, o := range objs {
		if o.Err == nil && o.Pop.Status.TLS != nil && o.Pop.Status.TLS.Host != "" {
			hosts[o.Pop.Status.TLS.Host] = o.Name
		}
	}
	wanted := map[string]bool{}
	for i := range objs {
		o := &objs[i]
		if o.Err != nil {
			continue
		}
		p := &o.Pop
		if !r.active(p) {
			continue
		}
		wanted[p.Name] = true
		host, err := r.ensureHost(ctx, p, hosts)
		if err != nil {
			r.opts.Log.Warn("named host not assigned", "pop", p.Name, "err", err)
			keep(err)
			continue
		}
		if err := r.ensureDNS(ctx, p, host); err != nil {
			r.opts.Log.Warn("named DNS record not written", "pop", p.Name, "err", err)
			keep(err)
		}
		if err := r.ensureCertificate(ctx, p, host); err != nil {
			r.opts.Log.Warn("named certificate not issued", "pop", p.Name, "err", err)
			keep(err)
		}
	}
	keep(r.sweepDNS(ctx, wanted))
	return first
}

// active: the named address is on and the PoP may still link.
func (r *PopReconciler) active(p *pops.Pop) bool {
	if !p.Spec.NamedAddress.Enabled || p.Spec.Revoked {
		return false
	}
	return p.Spec.NotAfter == nil || r.opts.Now().Before(p.Spec.NotAfter.Time)
}

// NewNamedLabel draws "p-" and six random lowercase letters or digits
// (36^6 ≈ 2.2e9 labels).
func NewNamedLabel() (string, error) {
	b := make([]byte, namedLabelLen)
	max := big.NewInt(int64(len(namedLabelAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = namedLabelAlphabet[n.Int64()]
	}
	return "p-" + string(b), nil
}

// ensureHost returns the Pop's host, assigning one when it has none (or
// one outside the current zone).
func (r *PopReconciler) ensureHost(ctx context.Context, p *pops.Pop, taken map[string]string) (string, error) {
	if p.Status.TLS != nil && strings.HasSuffix(p.Status.TLS.Host, "."+r.opts.Zone) {
		return p.Status.TLS.Host, nil
	}
	for range 8 {
		label, err := r.opts.NewLabel()
		if err != nil {
			return "", err
		}
		label = strings.ToLower(label)
		host := label + "." + r.opts.Zone
		if label == p.Name || strings.Contains(label, p.Name) {
			continue // never the PoP's name (item 38)
		}
		if owner, ok := taken[host]; ok && owner != p.Name {
			continue
		}
		if err := r.opts.Pops.PatchTLSStatus(ctx, p.Name, pops.TLS{Host: host}); err != nil {
			return "", err
		}
		taken[host] = p.Name
		if p.Status.TLS == nil {
			p.Status.TLS = &pops.TLS{}
		}
		p.Status.TLS.Host = host
		r.opts.Log.Info("named host assigned", "pop", p.Name, "host", host)
		return host, nil
	}
	return "", errors.New("kube: could not draw a free named label")
}

func dnsEndpointName(pop string) string { return "pop-" + pop }

// ensureDNS keeps the Pop's DNSEndpoint naming its LAN address. With no
// LAN address reported yet it leaves any record as it is.
func (r *PopReconciler) ensureDNS(ctx context.Context, p *pops.Pop, host string) error {
	if p.Status.Link == nil {
		return nil
	}
	lan, err := netip.ParseAddr(p.Status.Link.LanAddr)
	if err != nil {
		return nil
	}
	lan = lan.Unmap()
	recordType := "A"
	if lan.Is6() {
		recordType = "AAAA"
	}
	spec := map[string]any{"endpoints": []any{map[string]any{
		"dnsName":    host,
		"recordType": recordType,
		"recordTTL":  int64(r.opts.DNSTTL / time.Second),
		"targets":    []any{lan.String()},
	}}}
	ri := r.opts.Dynamic.Resource(DNSEndpointResource).Namespace(r.opts.Namespace)
	name := dnsEndpointName(p.Name)
	cur, err := ri.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": DNSEndpointResource.GroupVersion().String(),
			"kind":       "DNSEndpoint",
			"spec":       spec,
		}}
		u.SetName(name)
		u.SetNamespace(r.opts.Namespace)
		r.owned(u, p)
		if _, err := ri.Create(ctx, u, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create DNSEndpoint %s: %w", name, err)
		}
		r.opts.Log.Info("named DNS record written", "pop", p.Name, "host", host)
		return nil
	case err != nil:
		return fmt.Errorf("get DNSEndpoint %s: %w", name, err)
	}
	if old, _, _ := unstructured.NestedMap(cur.Object, "spec"); reflect.DeepEqual(old, spec) {
		return nil
	}
	cur.Object["spec"] = spec
	if _, err := ri.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update DNSEndpoint %s: %w", name, err)
	}
	return nil
}

// sweepDNS deletes every PoP DNSEndpoint not wanted this pass: revoked,
// ended, named address off, or the Pop gone (the ownerReference GC covers
// the last case too, but not the others).
func (r *PopReconciler) sweepDNS(ctx context.Context, wanted map[string]bool) error {
	ri := r.opts.Dynamic.Resource(DNSEndpointResource).Namespace(r.opts.Namespace)
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: pops.LabelKey})
	if err != nil {
		return fmt.Errorf("list DNSEndpoints: %w", err)
	}
	var first error
	for _, u := range list.Items {
		pop := u.GetLabels()[pops.LabelKey]
		if pop == "" || wanted[pop] {
			continue
		}
		if err := ri.Delete(ctx, u.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			if first == nil {
				first = fmt.Errorf("delete DNSEndpoint %s: %w", u.GetName(), err)
			}
			continue
		}
		r.opts.Log.Info("named DNS record deleted", "pop", pop)
	}
	return first
}

// owned labels u with the Pop and makes the Pop its owner.
func (r *PopReconciler) owned(u *unstructured.Unstructured, p *pops.Pop) {
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[pops.LabelKey] = p.Name
	u.SetLabels(labels)
	if ref, ok := PopOwnerReference(*p); ok {
		u.SetOwnerReferences([]metav1.OwnerReference{ref})
	}
}

func certificateRequestName(pop, sha string) string { return "pop-" + pop + "-" + sha[:16] }

// ensureCertificate issues the Pop's current CSR once.
func (r *PopReconciler) ensureCertificate(ctx context.Context, p *pops.Pop, host string) error {
	tls := p.Status.TLS
	if tls == nil || tls.CSR == "" {
		return nil
	}
	sha := pops.CSRSHA256(tls.CSR)
	if sha != "" && sha == tls.IssuedForCSR {
		return nil // issued; the link-home pod delivers it
	}
	csr, err := pops.ValidateCSR(tls.CSR, host)
	if err != nil {
		r.mu.Lock()
		again := r.refused[p.Name] == sha+tls.CSR
		r.refused[p.Name] = sha + tls.CSR
		r.mu.Unlock()
		if !again {
			r.opts.Log.Warn("named CSR refused", "pop", p.Name, "err", err)
		}
		return nil
	}
	ri := r.opts.Dynamic.Resource(CertificateRequestResource).Namespace(r.opts.Namespace)
	name := certificateRequestName(p.Name, sha)
	cr, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": CertificateRequestResource.GroupVersion().String(),
			"kind":       "CertificateRequest",
			"spec": map[string]any{
				"request":   base64.StdEncoding.EncodeToString([]byte(tls.CSR)),
				"issuerRef": map[string]any{"name": r.issuerName, "kind": r.issuerKind, "group": "cert-manager.io"},
				"usages":    []any{"digital signature", "server auth"},
			},
		}}
		u.SetName(name)
		u.SetNamespace(r.opts.Namespace)
		u.SetAnnotations(map[string]string{AnnotationCSRSHA256: sha})
		r.owned(u, p)
		if _, err := ri.Create(ctx, u, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create CertificateRequest %s: %w", name, err)
		}
		r.opts.Log.Info("named certificate requested", "pop", p.Name, "host", host, "request", name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get CertificateRequest %s: %w", name, err)
	}
	switch requestState(cr) {
	case "ready":
	case "failed":
		if r.opts.Now().Sub(cr.GetCreationTimestamp().Time) >= r.opts.FailedRetry {
			r.opts.Log.Warn("named certificate request failed; asking again", "pop", p.Name, "request", name)
			if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		return nil
	default:
		return nil // pending
	}
	enc, _, _ := unstructured.NestedString(cr.Object, "status", "certificate")
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) == 0 {
		return fmt.Errorf("CertificateRequest %s: ready with no certificate", name)
	}
	chain, err := pops.CheckChain(string(raw), csr, host)
	if err != nil {
		return fmt.Errorf("CertificateRequest %s: %w", name, err)
	}
	leaf := chain[0]
	notAfter := metav1.NewTime(leaf.NotAfter.UTC())
	if err := r.opts.Pops.PatchTLSStatus(ctx, p.Name, pops.TLS{Certificate: string(raw), IssuedForCSR: sha, NotAfter: &notAfter}); err != nil {
		return err
	}
	r.opts.Log.Info("named certificate issued", "pop", p.Name, "host", host, "not_after", leaf.NotAfter)
	if life := leaf.NotAfter.Sub(leaf.NotBefore); life > longLivedWarn {
		r.opts.Log.Warn("the named issuer is not short-lived: a revoked PoP keeps a valid certificate for this name until it expires; use Let's Encrypt's shortlived profile",
			"pop", p.Name, "lifetime", life.Round(time.Hour).String())
	}
	r.deleteStaleRequests(ctx, p.Name, name)
	return nil
}

// deleteStaleRequests removes the Pop's other CertificateRequests once a
// newer one is issued.
func (r *PopReconciler) deleteStaleRequests(ctx context.Context, pop, keep string) {
	ri := r.opts.Dynamic.Resource(CertificateRequestResource).Namespace(r.opts.Namespace)
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: pops.LabelKey + "=" + pop})
	if err != nil {
		return
	}
	for _, u := range list.Items {
		if u.GetName() == keep || u.GetLabels()[pops.LabelKey] != pop {
			continue
		}
		if err := ri.Delete(ctx, u.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			r.opts.Log.Debug("stale certificate request not deleted", "request", u.GetName(), "err", err)
		}
	}
}

// requestState reads a CertificateRequest's conditions: "ready",
// "failed" (Ready False/Failed, or Denied/InvalidRequest True) or
// "pending".
func requestState(u *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	state := "pending"
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		status, _ := m["status"].(string)
		reason, _ := m["reason"].(string)
		switch {
		case typ == "Ready" && status == "True":
			return "ready"
		case typ == "Ready" && status == "False" && reason == "Failed",
			(typ == "Denied" || typ == "InvalidRequest") && status == "True":
			state = "failed"
		}
	}
	return state
}
