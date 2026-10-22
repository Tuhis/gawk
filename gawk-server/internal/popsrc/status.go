package popsrc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

// ErrLeaseLost: a lease write found another generation than the writer's.
// The writer's link is stale (another pod force-took it) and must close.
var ErrLeaseLost = errors.New("popsrc: link lease superseded")

// statusRetries bounds a read-modify-write's conflict retries.
const statusRetries = 8

// StatusWriter is the relays' half of the Pop status (docs/76 §9): the
// credential, the link lease, the link report and the named tier's CSR.
// gawk-admin's half (status.tls host/certificate) is never written here.
//
// Every write is a read-modify-write of the fresh object, fenced on its
// resourceVersion and retried from a fresh read on a conflict, so two
// writers on disjoint fields never lose each other's write.
type StatusWriter interface {
	// RedeemPairing records a credential paired under the token tokenSHA.
	// The redeem checks pops.Pop.Redeemable against the fresh object inside
	// the CAS, so a string pairs exactly one PoP: a second (or racing)
	// redeem is pops.ErrPairingUsed. Other refusals are Redeemable's.
	RedeemPairing(ctx context.Context, name, tokenSHA, credSHA string, now time.Time) error
	// TakeLease force-takes the link lease for holder (a fresh link is the
	// ground truth, docs/76 item 12): the generation becomes the stored
	// one plus one, whatever the holder was. It returns the new generation.
	TakeLease(ctx context.Context, name, holder, addr string, now time.Time) (gen int64, err error)
	// PatchLease writes l when the stored generation is expectGen, else
	// ErrLeaseLost. A lease with no holder is a release: it also clears
	// status.link.connected in the same write, so a stale pod's release
	// can never mark a newer link disconnected.
	PatchLease(ctx context.Context, name string, l pops.Lease, expectGen int64) error
	// PatchLink replaces status.link.
	PatchLink(ctx context.Context, name string, l pops.Link) error
	// AddPublicAddr adds one public address (v4) or /64 (v6) to
	// status.link.publicAddrs. Any pod may call it: /pop/addr lands
	// wherever the load balancer sends it.
	AddPublicAddr(ctx context.Context, name, addr string) error
	// PatchCSR writes status.tls.csr.
	PatchCSR(ctx context.Context, name, csrPEM string) error
	// Get reads the Pop fresh (the API, not the informer cache): the
	// fallback when a credential the cache does not know yet may have just
	// been redeemed on another pod.
	Get(ctx context.Context, name string) (*pops.Pop, error)
}

// NewK8sStatus writes the Pop CRs' status subresource in namespace ns.
func NewK8sStatus(dc dynamic.Interface, ns string) StatusWriter {
	return &k8sStatus{ri: dc.Resource(pops.GroupVersionResource).Namespace(ns)}
}

type k8sStatus struct {
	ri dynamic.ResourceInterface
}

func (k *k8sStatus) Get(ctx context.Context, name string) (*pops.Pop, error) { return k.get(ctx, name) }

func (k *k8sStatus) get(ctx context.Context, name string) (*pops.Pop, error) {
	u, err := k.ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var p pops.Pop
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p); err != nil {
		return nil, fmt.Errorf("popsrc: pop CR %q: %w", name, err)
	}
	return &p, nil
}

// mutate applies fn to the fresh Pop and writes its status, retrying from a
// fresh read on a conflict. fn's error aborts without a write.
func (k *k8sStatus) mutate(ctx context.Context, name string, fn func(p *pops.Pop) error) error {
	var err error
	for range statusRetries {
		var p *pops.Pop
		if p, err = k.get(ctx, name); err != nil {
			return err
		}
		if err = fn(p); err != nil {
			return err
		}
		p.TypeMeta = metav1.TypeMeta{APIVersion: pops.SchemeGroupVersion.String(), Kind: pops.Kind}
		raw, cerr := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
		if cerr != nil {
			return cerr
		}
		_, err = k.ri.UpdateStatus(ctx, &unstructured.Unstructured{Object: raw}, metav1.UpdateOptions{})
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("popsrc: status retries exhausted: %w", err)
}

func (k *k8sStatus) RedeemPairing(ctx context.Context, name, tokenSHA, credSHA string, now time.Time) error {
	err := k.mutate(ctx, name, func(p *pops.Pop) error {
		if err := p.Redeemable(tokenSHA, now); err != nil {
			return err
		}
		at := metav1.NewTime(now)
		p.Status.Credential = &pops.Credential{SHA256: credSHA, PairingTokenSHA256: p.Spec.Pairing.TokenSHA256, PairedAt: &at}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return pops.ErrPairingUnknown
	}
	return err
}

func (k *k8sStatus) TakeLease(ctx context.Context, name, holder, addr string, now time.Time) (int64, error) {
	var gen int64
	err := k.mutate(ctx, name, func(p *pops.Pop) error {
		gen = 1
		if p.Status.Lease != nil {
			gen = p.Status.Lease.Generation + 1
		}
		at := metav1.NewTime(now)
		p.Status.Lease = &pops.Lease{Holder: holder, Addr: addr, Generation: gen, RenewedAt: &at}
		return nil
	})
	return gen, err
}

func (k *k8sStatus) PatchLease(ctx context.Context, name string, l pops.Lease, expectGen int64) error {
	return k.mutate(ctx, name, func(p *pops.Pop) error {
		var cur int64
		if p.Status.Lease != nil {
			cur = p.Status.Lease.Generation
		}
		if cur != expectGen {
			return ErrLeaseLost
		}
		l.Generation = expectGen
		p.Status.Lease = &l
		if l.Holder == "" && p.Status.Link != nil {
			p.Status.Link.Connected = false
		}
		return nil
	})
}

func (k *k8sStatus) PatchLink(ctx context.Context, name string, l pops.Link) error {
	return k.mutate(ctx, name, func(p *pops.Pop) error {
		p.Status.Link = &l
		return nil
	})
}

func (k *k8sStatus) AddPublicAddr(ctx context.Context, name, addr string) error {
	return k.mutate(ctx, name, func(p *pops.Pop) error {
		if p.Status.Link == nil {
			p.Status.Link = &pops.Link{}
		}
		if !slices.Contains(p.Status.Link.PublicAddrs, addr) {
			p.Status.Link.PublicAddrs = append(p.Status.Link.PublicAddrs, addr)
		}
		return nil
	})
}

func (k *k8sStatus) PatchCSR(ctx context.Context, name, csrPEM string) error {
	return k.mutate(ctx, name, func(p *pops.Pop) error {
		if p.Status.TLS == nil {
			p.Status.TLS = &pops.TLS{}
		}
		p.Status.TLS.CSR = csrPEM
		return nil
	})
}

// NewMemStatus is the -pops-file status writer: in memory, per process. A
// file Pop has no pairing (its credential hash is the credential, docs/76
// item 9), so RedeemPairing always answers pops.ErrPairingUnknown; the
// lease still fences a link against a stale one on the same pod.
func NewMemStatus(src Source) StatusWriter {
	return &memStatus{src: src, st: map[string]*pops.PopStatus{}}
}

type memStatus struct {
	src Source
	mu  sync.Mutex
	st  map[string]*pops.PopStatus
}

// Get returns the source's Pop with this writer's lease, link and CSR laid
// over it (the credential stays the file's).
func (m *memStatus) Get(_ context.Context, name string) (*pops.Pop, error) {
	if m.src == nil {
		return nil, fmt.Errorf("popsrc: pop %q: %w", name, pops.ErrPairingUnknown)
	}
	p, ok := m.src.Get(name)
	if !ok {
		return nil, fmt.Errorf("popsrc: pop %q not found", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.st[name]; ok {
		var st pops.PopStatus
		s.DeepCopyInto(&st)
		p.Status.Lease, p.Status.Link, p.Status.TLS = st.Lease, st.Link, st.TLS
	}
	return p, nil
}

func (m *memStatus) of(name string) *pops.PopStatus {
	s, ok := m.st[name]
	if !ok {
		s = &pops.PopStatus{}
		m.st[name] = s
	}
	return s
}

func (m *memStatus) RedeemPairing(context.Context, string, string, string, time.Time) error {
	return pops.ErrPairingUnknown
}

func (m *memStatus) TakeLease(_ context.Context, name, holder, addr string, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.of(name)
	var gen int64 = 1
	if s.Lease != nil {
		gen = s.Lease.Generation + 1
	}
	at := metav1.NewTime(now)
	s.Lease = &pops.Lease{Holder: holder, Addr: addr, Generation: gen, RenewedAt: &at}
	return gen, nil
}

func (m *memStatus) PatchLease(_ context.Context, name string, l pops.Lease, expectGen int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.of(name)
	var cur int64
	if s.Lease != nil {
		cur = s.Lease.Generation
	}
	if cur != expectGen {
		return ErrLeaseLost
	}
	l.Generation = expectGen
	s.Lease = &l
	if l.Holder == "" && s.Link != nil {
		s.Link.Connected = false
	}
	return nil
}

func (m *memStatus) PatchLink(_ context.Context, name string, l pops.Link) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.of(name).Link = &l
	return nil
}

func (m *memStatus) AddPublicAddr(_ context.Context, name, addr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.of(name)
	if s.Link == nil {
		s.Link = &pops.Link{}
	}
	if !slices.Contains(s.Link.PublicAddrs, addr) {
		s.Link.PublicAddrs = append(s.Link.PublicAddrs, addr)
	}
	return nil
}

func (m *memStatus) PatchCSR(_ context.Context, name, csrPEM string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.of(name)
	if s.TLS == nil {
		s.TLS = &pops.TLS{}
	}
	s.TLS.CSR = csrPEM
	return nil
}
