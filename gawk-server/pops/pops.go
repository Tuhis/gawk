package pops

import (
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// The Pop CRD's group/version/kind (docs/76 §9). Namespaced, beside the
// relay pods that hold its link, like Rooms and origin Leases.
const (
	GroupName = "gawk.ioio.fi"
	Version   = "v1alpha1"
	Kind      = "Pop"
	ListKind  = "PopList"
	// Resource is the plural lowercase resource name.
	Resource = "pops"

	// LabelKey marks a broadcast Lease reserved for, or held through, a PoP
	// (docs/76 item 14); its value is the Pop name, which is why a name is a
	// DNS-1123 label of at most MaxNameLen characters. Equal to
	// internal/cluster.PopLabelKey (pinned by a test there).
	LabelKey   = "gawk/pop"
	MaxNameLen = validation.DNS1123LabelMaxLength

	// Budget and limit defaults (docs/76 §9): a zero field in the CR means
	// these for the uplink and the limits, and "unlimited" for the downlink.
	DefaultUplinkMbps         = 25
	DefaultUplinkStreams      = 3
	DefaultMaxBroadcasts      = 40
	DefaultMaxDownlinkStreams = 20

	// DefaultPairingTTL is how long an unused pairing string can be redeemed
	// (gawk-admin's -pop-pairing-ttl default). It never limits a credential
	// that was paired in time (docs/76 item 8).
	DefaultPairingTTL = 72 * time.Hour

	// PairingPrefix starts every pairing string.
	PairingPrefix = "gawk-pair:"
)

// Errors. The link and leg routes map them onto statuses (contract §2.4):
// ErrCredentialNotCurrent → 401, ErrRevoked / ErrExpired → 403; the pairing
// dial maps ErrPairingUsed / ErrPairingExpired → 410, ErrPairingUnknown → 404.
var (
	ErrBadName              = errors.New("pops: invalid Pop name")
	ErrBadPairing           = errors.New("pops: malformed pairing string")
	ErrPairingUsed          = errors.New("pops: pairing string already used")
	ErrPairingExpired       = errors.New("pops: pairing string expired")
	ErrPairingUnknown       = errors.New("pops: unknown pairing string")
	ErrCredentialNotCurrent = errors.New("pops: credential not current")
	ErrRevoked              = errors.New("pops: PoP revoked")
	ErrExpired              = errors.New("pops: PoP past its end date")
)

// SchemeGroupVersion is the group/version this package registers.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// GroupVersionResource addresses Pops through a dynamic client.
var GroupVersionResource = SchemeGroupVersion.WithResource(Resource)

// SchemeBuilder / AddToScheme follow the client-go convention.
var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion, &Pop{}, &PopList{})
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

// Pop is one PoP relay a home fleet accepts links from (docs/76 §9). spec
// is written by gawk-admin (or kubectl); status has two writers on disjoint
// paths, see PopStatus.
type Pop struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PopSpec   `json:"spec"`
	Status PopStatus `json:"status,omitempty"`
}

// PopList is the list type for the Pop kind.
type PopList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Pop `json:"items"`
}

// PopSpec mirrors the YAML in docs/76 §9.
type PopSpec struct {
	DisplayName string `json:"displayName"`
	// NotAfter, when set, refuses links and legs after it (ErrExpired).
	NotAfter *metav1.Time `json:"notAfter,omitempty"`
	// Revoked refuses links and legs (ErrRevoked), docs/76 §6.9.
	Revoked bool `json:"revoked,omitempty"`
	// Pairing is the current pairing string's hash. A Re-pair writes a new
	// one and nothing else, which makes every older credential not current.
	Pairing Pairing `json:"pairing,omitempty"`
	// Uplink is the PoP → home budget; a zero field is the default.
	Uplink Budget `json:"uplink,omitempty"`
	// Downlink is the home → PoP budget; a zero field is unlimited.
	Downlink     Budget       `json:"downlink,omitempty"`
	Limits       Limits       `json:"limits,omitempty"`
	Guests       Guests       `json:"guests,omitempty"`
	Wall         *Wall        `json:"wall,omitempty"`
	NamedAddress NamedAddress `json:"namedAddress,omitempty"`
}

// Pairing is the stored half of a pairing string: its hash, never the
// string (docs/76 item 7).
type Pairing struct {
	TokenSHA256 string       `json:"tokenSHA256,omitempty"`
	IssuedAt    *metav1.Time `json:"issuedAt,omitempty"`
	ExpiresAt   *metav1.Time `json:"expiresAt,omitempty"`
}

// Budget is a Mbps and stream ceiling. 0 means the default (uplink) or
// unlimited (downlink).
type Budget struct {
	MaxMbps    int `json:"maxMbps,omitempty"`
	MaxStreams int `json:"maxStreams,omitempty"`
}

// Limits are enforced at home; 0 means the default.
type Limits struct {
	MaxBroadcasts      int `json:"maxBroadcasts,omitempty"`
	MaxDownlinkStreams int `json:"maxDownlinkStreams,omitempty"`
}

// Guests holds the guest-side options. AutoSwitch nil means true.
type Guests struct {
	AutoSwitch *bool `json:"autoSwitch,omitempty"`
}

// Wall names the party wall room (docs/76 §6.10); absent = no wall.
type Wall struct {
	RoomCode string `json:"roomCode"`
}

// NamedAddress turns on the named tier (docs/76 §6.11).
type NamedAddress struct {
	Enabled bool `json:"enabled,omitempty"`
}

// PopStatus has two writers on disjoint paths, so their merge patches never
// collide (docs/76 §9):
//   - the relays (the link-home pod) own Credential, Lease, Link and TLS.CSR;
//   - gawk-admin owns TLS.Host, TLS.Certificate, TLS.IssuedForCSR and
//     TLS.NotAfter.
//
// gawk-admin never writes Credential: invalidation goes through spec.
type PopStatus struct {
	Credential *Credential `json:"credential,omitempty"`
	Lease      *Lease      `json:"lease,omitempty"`
	Link       *Link       `json:"link,omitempty"`
	TLS        *TLS        `json:"tls,omitempty"`
}

// Credential records the PoP's credential by its SHA-256 only, never the
// credential itself (the R42 creatorTokenFingerprint precedent), and the
// pairing string it was minted under.
type Credential struct {
	SHA256             string       `json:"sha256,omitempty"`
	PairingTokenSHA256 string       `json:"pairingTokenSHA256,omitempty"`
	PairedAt           *metav1.Time `json:"pairedAt,omitempty"`
}

// Lease is the link-home record with the R17 generation-CAS semantics.
type Lease struct {
	Holder     string       `json:"holder,omitempty"`
	Addr       string       `json:"addr,omitempty"`
	Generation int64        `json:"generation,omitempty"`
	RenewedAt  *metav1.Time `json:"renewedAt,omitempty"`
}

// Link is what the PoP reported on its live link. It carries addresses: the
// Kubernetes API is internal, and no event ever copies them (docs/76 §8).
type Link struct {
	Connected    bool         `json:"connected,omitempty"`
	Since        *metav1.Time `json:"since,omitempty"`
	Version      string       `json:"version,omitempty"`
	Proto        int          `json:"proto,omitempty"`
	PublicAddrs  []string     `json:"publicAddrs,omitempty"`
	LanAddr      string       `json:"lanAddr,omitempty"`
	PinnedHashes []string     `json:"pinnedHashes,omitempty"`
}

// TLS is the named tier's certificate state. CSR is the relays'; the rest
// is gawk-admin's.
type TLS struct {
	Host         string       `json:"host,omitempty"`
	CSR          string       `json:"csr,omitempty"`
	Certificate  string       `json:"certificate,omitempty"`
	IssuedForCSR string       `json:"issuedForCSR,omitempty"`
	NotAfter     *metav1.Time `json:"notAfter,omitempty"`
}

// FilePop is one -pops-file entry: the shape an operator writes by hand
// where there is no Kubernetes API (the docs/42 / -rooms-file posture).
// There is no pairing: CredentialSHA256 is the credential, and rotating it
// means editing the file (docs/76 item 9).
type FilePop struct {
	Name string `json:"name"`
	// UID is the PoP's stable handle (a UUID), what a CR's metadata.uid is
	// in cluster mode (docs/76 item 41).
	UID              string     `json:"uid"`
	DisplayName      string     `json:"displayName,omitempty"`
	CredentialSHA256 string     `json:"credentialSHA256"`
	NotAfter         *time.Time `json:"notAfter,omitempty"`
	Revoked          bool       `json:"revoked,omitempty"`
	Uplink           Budget     `json:"uplink,omitempty"`
	Downlink         Budget     `json:"downlink,omitempty"`
	Limits           Limits     `json:"limits,omitempty"`
	Guests           Guests     `json:"guests,omitempty"`
	Wall             *Wall      `json:"wall,omitempty"`
}

// ToPop converts a file entry into the Pop every consumer reads. The
// credential is current by construction: both pairing hashes are empty.
func (f FilePop) ToPop() Pop {
	p := Pop{
		TypeMeta:   metav1.TypeMeta{APIVersion: SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: f.Name, UID: types.UID(f.UID)},
		Spec: PopSpec{
			DisplayName: f.DisplayName,
			Revoked:     f.Revoked,
			Uplink:      f.Uplink,
			Downlink:    f.Downlink,
			Limits:      f.Limits,
		},
		Status: PopStatus{Credential: &Credential{SHA256: f.CredentialSHA256}},
	}
	if f.NotAfter != nil {
		t := metav1.NewTime(*f.NotAfter)
		p.Spec.NotAfter = &t
	}
	if f.Guests.AutoSwitch != nil {
		v := *f.Guests.AutoSwitch
		p.Spec.Guests.AutoSwitch = &v
	}
	if f.Wall != nil {
		w := *f.Wall
		p.Spec.Wall = &w
	}
	return p
}

// ValidateName checks a Pop name: a DNS-1123 label of at most MaxNameLen
// characters, because it is also the gawk/pop label value.
func ValidateName(name string) error {
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrBadName, errs[0])
	}
	return nil
}

// ValidateWallCode checks a wall room code: an ordinary static room code
// (rooms.NormalizeCode) that is not dynamic-shaped. A Pop name can be up to
// 63 characters and a room code only 32, so a long or six-character
// broadcast-alphabet name needs an explicit wall code.
func ValidateWallCode(code string) error {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return err
	}
	if rooms.IsDynamicShape(norm) {
		return fmt.Errorf("%w: a wall code must not have a broadcast ID's shape", rooms.ErrInvalidCode)
	}
	return nil
}

// EffectiveUplink is the uplink budget with zero fields defaulted.
func (p *Pop) EffectiveUplink() Budget {
	b := p.Spec.Uplink
	if b.MaxMbps <= 0 {
		b.MaxMbps = DefaultUplinkMbps
	}
	if b.MaxStreams <= 0 {
		b.MaxStreams = DefaultUplinkStreams
	}
	return b
}

// EffectiveLimits is the limits block with zero fields defaulted.
func (p *Pop) EffectiveLimits() Limits {
	l := p.Spec.Limits
	if l.MaxBroadcasts <= 0 {
		l.MaxBroadcasts = DefaultMaxBroadcasts
	}
	if l.MaxDownlinkStreams <= 0 {
		l.MaxDownlinkStreams = DefaultMaxDownlinkStreams
	}
	return l
}

// AutoSwitch reports whether home offers this PoP to matching guests
// (docs/76 item 41); unset means true.
func (p *Pop) AutoSwitch() bool {
	return p.Spec.Guests.AutoSwitch == nil || *p.Spec.Guests.AutoSwitch
}
