package popsrv

// certs.go: the named tier's CSR and certificate delivery (docs/76 §6.11
// item 38), on the link-home pod.
//
//   - A csr record from the PoP is checked against the host gawk-admin
//     assigned (status.tls.host) and written to status.tls.csr, the relays'
//     one tls path. gawk-admin's leader issues it.
//   - A chain goes down the link only when status.tls.issuedForCSR is the
//     hash of the CURRENT status.tls.csr — and, when the PoP sent a CSR on
//     this link, of that one. So a renewal CSR written over an issued one
//     is never answered with the previous chain, however the informer
//     interleaves the two writers' updates.
//   - It is (re)sent on every link-up and whenever the Pop changes, once
//     per link and CSR.

import (
	"sync"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// namedCerts is the Manager's named-tier state.
type namedCerts struct {
	m *Manager

	mu sync.Mutex
	// asked is the hash of the CSR the PoP last sent on its current link.
	asked map[string]linkSHA
	// sent is the CSR hash whose chain went down the current link.
	sent map[string]linkSHA
}

type linkSHA struct {
	l   *link
	sha string
}

// EnableNamed turns on the named tier's link records on m: the PoP's CSRs
// reach status.tls.csr and issued chains reach the PoP. wirePopNamed calls
// it under -pops; the returned func stops following the source.
func (m *Manager) EnableNamed() (stop func()) {
	n := &namedCerts{m: m, asked: map[string]linkSHA{}, sent: map[string]linkSHA{}}
	m.OnLinkRecord(poplink.TCSR, func(pop string, rec any) {
		if r, ok := rec.(*poplink.CSR); ok {
			n.onCSR(pop, r.PEM)
		}
	})
	m.OnLinkRecord(poplink.THello, func(pop string, _ any) { go n.deliver(pop) })
	if m.opts.Source == nil {
		return func() {}
	}
	return m.opts.Source.Subscribe(func(name string) {
		if m.local(name) != nil {
			go n.deliver(name)
		}
	})
}

// onCSR runs on the link's reader goroutine: the check is cheap, the
// status write is not, so it goes to its own goroutine.
func (n *namedCerts) onCSR(pop, csrPEM string) {
	l := n.m.local(pop)
	if l == nil {
		return
	}
	log := l.log
	p, ok := n.m.pop(pop)
	if !ok || p.Status.TLS == nil || p.Status.TLS.Host == "" || !p.Spec.NamedAddress.Enabled {
		log.Info("pop csr ignored: no named host is assigned")
		return
	}
	if _, err := pops.ValidateCSR(csrPEM, p.Status.TLS.Host); err != nil {
		log.Warn("pop csr refused", "host", p.Status.TLS.Host, "err", err)
		return
	}
	sha := pops.CSRSHA256(csrPEM)
	n.mu.Lock()
	n.asked[pop] = linkSHA{l, sha}
	n.mu.Unlock()
	if pops.CSRSHA256(p.Status.TLS.CSR) == sha {
		// A restart re-sending the request status already holds: no write,
		// and if it was issued the chain goes down now.
		go n.deliver(pop)
		return
	}
	go func() {
		ctx, cancel := background()
		defer cancel()
		if err := n.m.opts.Status.PatchCSR(ctx, pop, csrPEM); err != nil {
			log.Warn("pop csr not stored", "err", err)
			return
		}
		log.Info("pop csr stored", "host", p.Status.TLS.Host, "csr_sha256", sha)
	}()
}

// deliver sends the issued chain down the PoP's link when it answers the
// current CSR and has not gone down this link yet.
func (n *namedCerts) deliver(pop string) {
	l := n.m.local(pop)
	if l == nil || !l.ready.Load() {
		return
	}
	p, ok := n.m.pop(pop)
	if !ok || p.Status.TLS == nil {
		return
	}
	t := p.Status.TLS
	if t.Certificate == "" || t.CSR == "" || t.IssuedForCSR == "" {
		return
	}
	sha := pops.CSRSHA256(t.CSR)
	if sha != t.IssuedForCSR {
		return // the current CSR is not issued yet
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if a, ok := n.asked[pop]; ok && a.l == l && a.sha != sha {
		return // the PoP asked for another one on this link
	}
	if s, ok := n.sent[pop]; ok && s.l == l && s.sha == sha {
		return
	}
	if err := l.write(poplink.Certificate{ChainPEM: t.Certificate, ForCSRSHA256: sha}); err != nil {
		l.log.Debug("pop certificate write failed", "err", err)
		return
	}
	n.sent[pop] = linkSHA{l, sha}
	l.log.Info("pop certificate delivered", "host", t.Host, "csr_sha256", sha)
}
