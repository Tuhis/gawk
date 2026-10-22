package pops

import "k8s.io/apimachinery/pkg/runtime"

// Hand-written deepcopy, as in rooms and moderation: this repository runs
// no Kubernetes code generator. TestDeepCopyDoesNotAlias fails if a new
// pointer or slice field is left aliased.

// DeepCopyInto copies the receiver into out.
func (in *Pairing) DeepCopyInto(out *Pairing) {
	*out = *in
	if in.IssuedAt != nil {
		out.IssuedAt = in.IssuedAt.DeepCopy()
	}
	if in.ExpiresAt != nil {
		out.ExpiresAt = in.ExpiresAt.DeepCopy()
	}
}

// DeepCopyInto copies the receiver into out.
func (in *Guests) DeepCopyInto(out *Guests) {
	*out = *in
	if in.AutoSwitch != nil {
		v := *in.AutoSwitch
		out.AutoSwitch = &v
	}
}

// DeepCopyInto copies the receiver into out.
func (in *PopSpec) DeepCopyInto(out *PopSpec) {
	*out = *in
	if in.NotAfter != nil {
		out.NotAfter = in.NotAfter.DeepCopy()
	}
	in.Pairing.DeepCopyInto(&out.Pairing)
	in.Guests.DeepCopyInto(&out.Guests)
	if in.Wall != nil {
		w := *in.Wall
		out.Wall = &w
	}
}

// DeepCopyInto copies the receiver into out.
func (in *Credential) DeepCopyInto(out *Credential) {
	*out = *in
	if in.PairedAt != nil {
		out.PairedAt = in.PairedAt.DeepCopy()
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
func (in *Link) DeepCopyInto(out *Link) {
	*out = *in
	if in.Since != nil {
		out.Since = in.Since.DeepCopy()
	}
	if in.PublicAddrs != nil {
		out.PublicAddrs = append([]string(nil), in.PublicAddrs...)
	}
	if in.PinnedHashes != nil {
		out.PinnedHashes = append([]string(nil), in.PinnedHashes...)
	}
}

// DeepCopyInto copies the receiver into out.
func (in *TLS) DeepCopyInto(out *TLS) {
	*out = *in
	if in.NotAfter != nil {
		out.NotAfter = in.NotAfter.DeepCopy()
	}
}

// DeepCopyInto copies the receiver into out.
func (in *PopStatus) DeepCopyInto(out *PopStatus) {
	*out = *in
	if in.Credential != nil {
		out.Credential = new(Credential)
		in.Credential.DeepCopyInto(out.Credential)
	}
	if in.Lease != nil {
		out.Lease = new(Lease)
		in.Lease.DeepCopyInto(out.Lease)
	}
	if in.Link != nil {
		out.Link = new(Link)
		in.Link.DeepCopyInto(out.Link)
	}
	if in.TLS != nil {
		out.TLS = new(TLS)
		in.TLS.DeepCopyInto(out.TLS)
	}
}

// DeepCopyInto copies the receiver into out.
func (in *Pop) DeepCopyInto(out *Pop) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy.
func (in *Pop) DeepCopy() *Pop {
	if in == nil {
		return nil
	}
	out := new(Pop)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *Pop) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *PopList) DeepCopyInto(out *PopList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Pop, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy.
func (in *PopList) DeepCopy() *PopList {
	if in == nil {
		return nil
	}
	out := new(PopList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject satisfies runtime.Object.
func (in *PopList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
