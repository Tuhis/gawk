// Package rooms holds the Room custom resource types and the room-code rules
// shared by the relay and gawk-admin (R42, docs/44 §4.3, RM1).
//
// It is public — outside internal/ — for the same reason moderation is
// (docs/42 D13): gawk-admin compiles it in through its repo-root replace and
// must never mirror it. Keep it dependency-light: apimachinery only, no
// client-go, no hub types. Everything that needs a QUIC session or a hub
// registry lives in internal/roomsrv.
package rooms

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
)

// The Room CRD's group/version/kind (docs/44 §4.3). Namespaced: rooms live
// beside the relay pods that home them, like Bans and origin Leases.
const (
	GroupName = "gawk.ioio.fi"
	Version   = "v1alpha1"
	Kind      = "Room"
	ListKind  = "RoomList"
	// Resource is the plural lowercase resource name — what an informer
	// watches and what `kubectl get rooms` resolves to.
	Resource = "rooms"
)

// Room kinds (docs/44 D2).
const (
	KindStatic  = "static"
	KindDynamic = "dynamic"
)

// Code limits (docs/44 §4.1): a static slug is 3–32 characters of
// [A-Za-z0-9-], case-insensitive, stored lower-case. A dynamic code is
// exactly broadcastid.Length characters of the broadcast alphabet and is
// stored lower-cased under the same rule, so one normalizer serves both.
const (
	MinCodeLen = 3
	MaxCodeLen = 32
)

// ErrInvalidCode is returned (possibly wrapped) for a code that fails the
// slug rules.
var ErrInvalidCode = errors.New("rooms: invalid room code")

// SchemeGroupVersion is the group/version this package registers.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// GroupVersionResource addresses Rooms through a dynamic client.
var GroupVersionResource = SchemeGroupVersion.WithResource(Resource)

// SchemeBuilder / AddToScheme follow the client-go convention so both the
// relay's informer and gawk-admin's client can register the types.
var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion, &Room{}, &RoomList{})
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

// NormalizeCode lower-cases a room code and validates it against the slug
// rules. The result is both the CR name (`metadata.name`, which must be a
// DNS-1123 label: lower-case alphanumerics and '-', not starting or ending
// with '-') and the registry key. A dynamic six-character code passes
// unchanged in shape; use IsDynamicShape to tell the two apart when a
// typed code has to be resolved (docs/44 §4.2).
func NormalizeCode(s string) (string, error) {
	code := strings.ToLower(strings.TrimSpace(s))
	if len(code) < MinCodeLen || len(code) > MaxCodeLen {
		return "", fmt.Errorf("%w: %d characters, want %d-%d", ErrInvalidCode, len(code), MinCodeLen, MaxCodeLen)
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return "", fmt.Errorf("%w: character %q", ErrInvalidCode, c)
		}
	}
	if code[0] == '-' || code[len(code)-1] == '-' {
		return "", fmt.Errorf("%w: must not start or end with '-'", ErrInvalidCode)
	}
	return code, nil
}

// IsDynamicShape reports whether a normalized code has the shape of a
// dynamic room code (and therefore of a broadcast ID): exactly
// broadcastid.Length characters of the broadcast alphabet. Only codes of
// this shape can be typed into the join box (docs/44 §4.2); a static slug
// that happens to have it is fine — the registry resolves by name and the
// CR spec says which kind it is.
func IsDynamicShape(code string) bool {
	_, err := broadcastid.Normalize(code)
	return err == nil
}

// DisplayCode returns the code as shown to people: a dynamic code in the
// broadcast alphabet's upper case, a static slug as configured (or, when
// the CR carries no displayCode, the normalized form).
func DisplayCode(r *Room) string {
	if r == nil {
		return ""
	}
	if r.Spec.DisplayCode != "" {
		return r.Spec.DisplayCode
	}
	if r.Spec.Kind == KindDynamic {
		return strings.ToUpper(r.Name)
	}
	return r.Name
}

// FingerprintSize is the byte length of a creator-token fingerprint: the
// first 8 bytes of SHA-256(token), hex-encoded (docs/44 §4.3). The CR
// stores the fingerprint only, never the token.
const FingerprintSize = 8

// Fingerprint returns the stored fingerprint of a creator token.
func Fingerprint(token []byte) string {
	sum := sha256.Sum256(token)
	return hex.EncodeToString(sum[:FingerprintSize])
}

// Room is one room: a static one written by the admin portal or kubectl, or
// a dynamic one the relay creates at mint (docs/44 D4, D5). spec is written
// by whoever creates the room; status is written only by the home pod.
type Room struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RoomSpec   `json:"spec"`
	Status RoomStatus `json:"status,omitempty"`
}

// RoomSpec mirrors the YAML in docs/44 §4.3.
type RoomSpec struct {
	// Kind is KindStatic or KindDynamic.
	Kind string `json:"kind"`
	// DisplayCode is the code as configured ("TuhisRoom"); dynamic rooms
	// omit it and display the upper-cased name.
	DisplayCode string `json:"displayCode,omitempty"`
	// DisplayName is an optional human title.
	DisplayName string `json:"displayName,omitempty"`
	// AttachSecretRef names the Secret key holding a static room's attach
	// secret (docs/44 D8). Absent means anyone may attach.
	AttachSecretRef *SecretKeyRef `json:"attachSecretRef,omitempty"`
	// MaxBroadcasts overrides -max-room-broadcasts for this room; 0 means
	// the fleet default.
	MaxBroadcasts int `json:"maxBroadcasts,omitempty"`
	// Integrations is reserved (docs/44 D18, §4.11); empty in v1.
	Integrations Integrations `json:"integrations,omitempty"`
}

// SecretKeyRef points at one key of a Kubernetes Secret in the room's
// namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Integrations is the reserved hook block (docs/44 §4.11). Nothing reads it
// in v1; it exists so a later milestone adds a field instead of a version.
type Integrations struct {
	// Mumble is the voice-bridge mapping reserved by docs/44 §4.11.
	Mumble *MumbleIntegration `json:"mumble,omitempty"`
}

// MumbleIntegration is the reserved shape of the Mumble mapping.
type MumbleIntegration struct {
	Server        string `json:"server,omitempty"`
	ChannelPrefix string `json:"channelPrefix,omitempty"`
}

// RoomStatus is written only by the home pod (docs/44 §4.3).
type RoomStatus struct {
	// CreatorTokenFingerprint is Fingerprint(creator token) for a dynamic
	// room; never the token.
	CreatorTokenFingerprint string `json:"creatorTokenFingerprint,omitempty"`
	// Key is the fleet's HMAC'd handle for the code — the same
	// -stats-key digest /statusz, metrics and telemetry use (docs/44 D16).
	// Written by the home pod so gawk-admin, which never holds the stats
	// key, can name the room in webhooks and deep links without the code.
	Key string `json:"key,omitempty"`
	// CreatedAt is when the room was minted (dynamic) or first adopted.
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`
	// Attachments is rebuilt by the home pod on adoption. It carries raw
	// broadcast IDs: the Kubernetes API is internal (docs/44 §4.3).
	Attachments []Attachment `json:"attachments,omitempty"`
	// Lease is the home-pod lease with the R17 generation-CAS semantics.
	Lease *Lease `json:"lease,omitempty"`
	// EmptySince is set by the home pod when the roster empties (dynamic
	// rooms only) and cleared by the next join.
	EmptySince *metav1.Time `json:"emptySince,omitempty"`
}

// Attachment is one broadcast bound to the room.
type Attachment struct {
	BroadcastID string       `json:"broadcastID"`
	Label       string       `json:"label,omitempty"`
	AttachedAt  *metav1.Time `json:"attachedAt,omitempty"`
}

// Lease is the home-pod ownership record (docs/44 D6). Holder empty with a
// non-zero Generation is a released lease: the next join claims it without
// waiting for staleness.
type Lease struct {
	Holder     string       `json:"holder,omitempty"`
	Addr       string       `json:"addr,omitempty"`
	Generation int64        `json:"generation"`
	RenewedAt  *metav1.Time `json:"renewedAt,omitempty"`
}

// RoomList is the list type for the Room kind.
type RoomList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Room `json:"items"`
}

// FileRoom is one static room definition in the -rooms-file source
// (docs/44 §4.3, "non-cluster mode"): the JSON shape an operator writes by
// hand where there is no Kubernetes API. The attach secret is inline here
// because there is no Secret object to point at; the file's own mode is the
// protection, exactly as -publish-secret's is.
type FileRoom struct {
	Code          string `json:"code"`
	DisplayName   string `json:"displayName,omitempty"`
	AttachSecret  string `json:"attachSecret,omitempty"`
	MaxBroadcasts int    `json:"maxBroadcasts,omitempty"`
}

// --- deepcopy ---------------------------------------------------------
//
// Hand-written, as in gawk-server/moderation: this repository runs no
// Kubernetes code generator. The round-trip test in rooms_test.go fails if a
// new pointer or slice field is left aliased.

// DeepCopyInto copies the receiver into out.
func (in *SecretKeyRef) DeepCopyInto(out *SecretKeyRef) { *out = *in }

// DeepCopyInto copies the receiver into out.
func (in *MumbleIntegration) DeepCopyInto(out *MumbleIntegration) { *out = *in }

// DeepCopyInto copies the receiver into out.
func (in *Integrations) DeepCopyInto(out *Integrations) {
	*out = *in
	if in.Mumble != nil {
		out.Mumble = new(MumbleIntegration)
		in.Mumble.DeepCopyInto(out.Mumble)
	}
}

// DeepCopyInto copies the receiver into out.
func (in *RoomSpec) DeepCopyInto(out *RoomSpec) {
	*out = *in
	if in.AttachSecretRef != nil {
		out.AttachSecretRef = new(SecretKeyRef)
		in.AttachSecretRef.DeepCopyInto(out.AttachSecretRef)
	}
	in.Integrations.DeepCopyInto(&out.Integrations)
}

// DeepCopyInto copies the receiver into out.
func (in *Attachment) DeepCopyInto(out *Attachment) {
	*out = *in
	if in.AttachedAt != nil {
		out.AttachedAt = in.AttachedAt.DeepCopy()
	}
}

// DeepCopyInto copies the receiver into out.
func (in *Lease) DeepCopyInto(out *Lease) {
	*out = *in
	if in.RenewedAt != nil {
		out.RenewedAt = in.RenewedAt.DeepCopy()
	}
}

// DeepCopyInto copies the receiver into out.
func (in *RoomStatus) DeepCopyInto(out *RoomStatus) {
	*out = *in
	if in.CreatedAt != nil {
		out.CreatedAt = in.CreatedAt.DeepCopy()
	}
	if in.Attachments != nil {
		out.Attachments = make([]Attachment, len(in.Attachments))
		for i := range in.Attachments {
			in.Attachments[i].DeepCopyInto(&out.Attachments[i])
		}
	}
	if in.Lease != nil {
		out.Lease = new(Lease)
		in.Lease.DeepCopyInto(out.Lease)
	}
	if in.EmptySince != nil {
		out.EmptySince = in.EmptySince.DeepCopy()
	}
}

// DeepCopyInto copies the receiver into out.
func (in *Room) DeepCopyInto(out *Room) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy.
func (in *Room) DeepCopy() *Room {
	if in == nil {
		return nil
	}
	out := new(Room)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *Room) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *RoomList) DeepCopyInto(out *RoomList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Room, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy.
func (in *RoomList) DeepCopy() *RoomList {
	if in == nil {
		return nil
	}
	out := new(RoomList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *RoomList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
