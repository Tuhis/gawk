package moderation

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The Ban CRD's group/version/kind (docs/42 §4.2). Namespaced: bans live
// beside the relay pods that enforce them.
const (
	GroupName = "gawk.ioio.fi"
	Version   = "v1alpha1"
	Kind      = "Ban"
	ListKind  = "BanList"
	// Resource is the plural lowercase resource name — what an informer
	// watches and what `kubectl get bans` resolves to.
	Resource = "bans"
)

// SchemeGroupVersion is the group/version this package registers.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// GroupVersionResource addresses Bans through a dynamic client.
var GroupVersionResource = SchemeGroupVersion.WithResource(Resource)

// SchemeBuilder / AddToScheme follow the client-go convention so both the
// relay's informer and gawk-admin's client can register the types.
var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion, &Ban{}, &BanList{})
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

// Ban is one enforcement object: exactly one target, optionally time-boxed.
// v1alpha1 has no status subresource — relays only ever read (docs/42 §4.2),
// which keeps relay RBAC to get/list/watch.
type Ban struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BanSpec `json:"spec"`
}

// BanSpec mirrors the YAML in docs/42 §4.2 exactly.
type BanSpec struct {
	// Target is the handle being banned.
	Target Target `json:"target"`
	// ExpiresAt is RFC 3339; ABSENT means permanent. Relays compare it
	// against their own clock at check time, so enforcement ends on schedule
	// whether or not the janitor has deleted the object.
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// Reason is informational operator text. It may carry operator-private
	// context, which is why the relay logs it at Debug only (docs/42 §4.3).
	Reason string `json:"reason,omitempty"`
	// CreatedBy is informational: an OIDC email, "system", or "kubectl" for
	// a break-glass ban the reconciler later adopts.
	CreatedBy string `json:"createdBy,omitempty"`
}

// BanList is the list type for the Ban kind.
type BanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Ban `json:"items"`
}

// RecordFromBan converts a CR into the evaluation record, normalizing the
// target. An unparseable target is an error, not a silently-ignored object:
// the caller logs and skips it rather than enforcing something it did not
// understand.
func RecordFromBan(b *Ban) (Record, error) {
	if b == nil {
		return Record{}, fmt.Errorf("%w: nil Ban", ErrInvalidTarget)
	}
	rec := Record{
		Target:    b.Spec.Target,
		Reason:    b.Spec.Reason,
		CreatedBy: b.Spec.CreatedBy,
	}
	if b.Spec.ExpiresAt != nil {
		t := b.Spec.ExpiresAt.Time
		rec.ExpiresAt = &t
	}
	return Normalize(rec)
}

// --- deepcopy ---------------------------------------------------------
//
// Hand-written: this repository runs no Kubernetes code generator
// (deepcopy-gen / controller-gen), and adding one for four small structs
// would buy a build-time toolchain nobody else needs. Keep these in sync by
// hand when a field is added — the round-trip test in crd_test.go fails if a
// new pointer or slice field is left aliased.

// DeepCopyInto copies the receiver into out.
func (in *Target) DeepCopyInto(out *Target) { *out = *in }

// DeepCopy returns a deep copy.
func (in *Target) DeepCopy() *Target {
	if in == nil {
		return nil
	}
	out := new(Target)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *BanSpec) DeepCopyInto(out *BanSpec) {
	*out = *in
	in.Target.DeepCopyInto(&out.Target)
	if in.ExpiresAt != nil {
		out.ExpiresAt = in.ExpiresAt.DeepCopy()
	}
}

// DeepCopy returns a deep copy.
func (in *BanSpec) DeepCopy() *BanSpec {
	if in == nil {
		return nil
	}
	out := new(BanSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *Ban) DeepCopyInto(out *Ban) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

// DeepCopy returns a deep copy.
func (in *Ban) DeepCopy() *Ban {
	if in == nil {
		return nil
	}
	out := new(Ban)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *Ban) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *BanList) DeepCopyInto(out *BanList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Ban, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy.
func (in *BanList) DeepCopy() *BanList {
	if in == nil {
		return nil
	}
	out := new(BanList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *BanList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
