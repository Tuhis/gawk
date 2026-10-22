package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

// AnnotationPopManaged marks a Pop CR the portal created (R74, docs/76
// §7.2), the AnnotationRoomManaged twin. A `kubectl apply`'d Pop carries
// none; it is listed and managed all the same, because a Pop's CR is its
// whole truth and the portal has nothing to converge it against.
const AnnotationPopManaged = "gawk.ioio.fi/pop-managed"

// AnnotationPopCreatedBy records the `sub` of the identity that created a
// portal PoP. Never served.
const AnnotationPopCreatedBy = "gawk.ioio.fi/pop-created-by"

// Sentinel errors. internal/api maps each onto a status.
var (
	// ErrPopExists: a Pop CR with that name already exists.
	ErrPopExists = errors.New("kube: a PoP with that name already exists")
	// ErrPopNotFound: no Pop CR with that name (or the one read was
	// replaced under the same name before the write landed).
	ErrPopNotFound = errors.New("kube: no such PoP")
	// ErrPopRevoked: a Re-pair of a revoked PoP. A new pairing string
	// would pair nothing — every link is refused while spec.revoked is set
	// (docs/76 §6.9) — so handing one out would be a lie.
	ErrPopRevoked = errors.New("kube: the PoP is revoked")
)

// PopObject is one Pop CR as the portal sees it.
type PopObject struct {
	Name string
	// Managed reports AnnotationPopManaged.
	Managed bool
	// CreatedBy is AnnotationPopCreatedBy, or empty.
	CreatedBy string
	// Pop is the decoded CR. Err is set instead when it could not be
	// decoded, in which case it is reported by name and never acted on
	// except by an explicit delete.
	Pop pops.Pop
	Err error
}

// NewPop is the portal's create request, already validated by the API
// layer. Spec.Pairing carries the pairing string's HASH: the string itself
// never reaches this package, which is what makes "only its SHA-256 is
// stored" (docs/76 item 7) true by construction.
type NewPop struct {
	Name      string
	Spec      pops.PopSpec
	CreatedBy string
}

// PopClient manages Pop CRs through the dynamic client in one namespace
// (R74, docs/76 §9). It writes spec, and of status only the four tls paths
// gawk-admin owns; the relays own credential, lease, link and tls.csr, and
// with the CRD's status subresource a spec write cannot clobber them.
type PopClient struct {
	ri dynamic.ResourceInterface
}

// NewPopClient builds a PopClient for one namespace from a REST config.
func NewPopClient(cfg *rest.Config, namespace string) (*PopClient, error) {
	if namespace == "" {
		return nil, fmt.Errorf("kube: namespace is required")
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: dynamic client: %w", err)
	}
	return NewPopClientFor(dc, namespace), nil
}

// NewPopClientFor wraps an existing dynamic client — the seam the tests use
// with dynamic/fake.
func NewPopClientFor(dc dynamic.Interface, namespace string) *PopClient {
	return &PopClient{ri: dc.Resource(pops.GroupVersionResource).Namespace(namespace)}
}

// Create writes a Pop CR. The create is the reservation: a duplicate name is
// refused by the API server, which is what ErrPopExists reports. The
// returned object is the API server's answer, so it carries the UID a wall
// room's ownerReference needs.
func (c *PopClient) Create(ctx context.Context, req NewPop) (PopObject, error) {
	if err := pops.ValidateName(req.Name); err != nil {
		return PopObject{}, err
	}
	annotations := map[string]string{AnnotationPopManaged: "true"}
	if req.CreatedBy != "" {
		annotations[AnnotationPopCreatedBy] = req.CreatedBy
	}
	p := &pops.Pop{
		TypeMeta:   metav1.TypeMeta{APIVersion: pops.SchemeGroupVersion.String(), Kind: pops.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Annotations: annotations},
	}
	req.Spec.DeepCopyInto(&p.Spec)
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
	if err != nil {
		return PopObject{}, fmt.Errorf("kube: encode pop %s: %w", req.Name, err)
	}
	// A Pop the portal creates has no status yet: the relays write it.
	delete(raw, "status")
	u, err := c.ri.Create(ctx, &unstructured.Unstructured{Object: raw}, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return PopObject{}, ErrPopExists
		}
		return PopObject{}, fmt.Errorf("kube: create pop %s: %w", req.Name, err)
	}
	return decodePop(u), nil
}

// List returns every Pop CR in the namespace, with no label selector: a
// `kubectl apply`'d Pop must be visible too.
func (c *PopClient) List(ctx context.Context) ([]PopObject, error) {
	list, err := c.ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("kube: list pops: %w", err)
	}
	out := make([]PopObject, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, decodePop(&list.Items[i]))
	}
	return out, nil
}

// Get returns one Pop CR by name; ErrPopNotFound when there is none.
func (c *PopClient) Get(ctx context.Context, name string) (PopObject, error) {
	u, err := c.ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return PopObject{}, ErrPopNotFound
		}
		return PopObject{}, fmt.Errorf("kube: get pop %s: %w", name, err)
	}
	return decodePop(u), nil
}

func decodePop(u *unstructured.Unstructured) PopObject {
	var p pops.Pop
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p); err != nil {
		return PopObject{Name: u.GetName(), Err: err}
	}
	return PopObject{
		Name:      p.Name,
		Managed:   p.Annotations[AnnotationPopManaged] == "true",
		CreatedBy: p.Annotations[AnnotationPopCreatedBy],
		Pop:       p,
	}
}

// PatchSpec applies edit to a copy of the Pop's spec as read and writes the
// difference as a JSON merge patch, returning the Pop before and after.
//
// The difference, not the whole spec: a merge patch only removes a key it
// names as null, so clearing an end date or resetting a budget field to its
// default has to be spelled out. spec.pairing is never written here, whatever
// edit does to it — a Re-pair is its own verb (Repair) because it is the one
// spec write that ends a credential. The patch carries the UID read, which
// the API server holds as a precondition; like a room's it is not bound by
// resourceVersion, because the link-home pod rewrites status every few
// seconds.
func (c *PopClient) PatchSpec(ctx context.Context, name string, edit func(*pops.PopSpec) error) (PopObject, PopObject, error) {
	before, err := c.Get(ctx, name)
	if err != nil {
		return PopObject{}, PopObject{}, err
	}
	if before.Err != nil {
		return before, PopObject{}, fmt.Errorf("kube: pop %s could not be decoded: %w", name, before.Err)
	}
	spec := &pops.PopSpec{}
	before.Pop.Spec.DeepCopyInto(spec)
	if err := edit(spec); err != nil {
		return before, PopObject{}, err
	}
	spec.Pairing = pops.Pairing{}
	before.Pop.Spec.Pairing.DeepCopyInto(&spec.Pairing)

	oldMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&before.Pop.Spec)
	if err != nil {
		return before, PopObject{}, fmt.Errorf("kube: encode pop spec: %w", err)
	}
	newMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(spec)
	if err != nil {
		return before, PopObject{}, fmt.Errorf("kube: encode pop spec: %w", err)
	}
	diff := mergeDiff(oldMap, newMap)
	if len(diff) == 0 {
		return before, before, nil
	}
	after, err := c.patch(ctx, before, map[string]any{"spec": diff}, "")
	return before, after, err
}

// Repair writes a new spec.pairing and nothing else (docs/76 item 9). The
// relays' status.credential is untouched: it simply stops naming the current
// string, so the credential paired under the old one is refused at once.
// pairing carries the new string's hash; the string itself is the caller's to
// show once.
func (c *PopClient) Repair(ctx context.Context, name string, pairing pops.Pairing) (PopObject, error) {
	before, err := c.Get(ctx, name)
	if err != nil {
		return PopObject{}, err
	}
	if before.Err != nil {
		return before, fmt.Errorf("kube: pop %s could not be decoded: %w", name, before.Err)
	}
	if before.Pop.Spec.Revoked {
		return before, ErrPopRevoked
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pairing)
	if err != nil {
		return before, fmt.Errorf("kube: encode pairing: %w", err)
	}
	// The whole object, not a diff: every field of the old pairing is
	// replaced, and one the new pairing lacks must not survive it.
	body := map[string]any{"tokenSHA256": nil, "issuedAt": nil, "expiresAt": nil}
	for k, v := range raw {
		body[k] = v
	}
	return c.patch(ctx, before, map[string]any{"spec": map[string]any{"pairing": body}}, "")
}

// Revoke sets spec.revoked (docs/76 §6.9). Idempotent. What a revoke does —
// the bye, the released reservations, the DNSEndpoint — is the relays' and
// the reconciler's; this is the record of the decision.
func (c *PopClient) Revoke(ctx context.Context, name string) (PopObject, error) {
	before, err := c.Get(ctx, name)
	if err != nil {
		return PopObject{}, err
	}
	if before.Err == nil && before.Pop.Spec.Revoked {
		return before, nil
	}
	return c.patch(ctx, before, map[string]any{"spec": map[string]any{"revoked": true}}, "")
}

// Delete removes a Pop CR, returning what it was, or ErrPopNotFound. check,
// when non-nil, decides on the object as read; the delete is then bound to
// that object by a UID precondition (the RoomClient.DeleteExisting rule).
// A wall room carrying an ownerReference to the Pop is garbage-collected
// with it.
func (c *PopClient) Delete(ctx context.Context, name string, check func(PopObject) error) (PopObject, error) {
	obj, err := c.Get(ctx, name)
	if err != nil {
		return PopObject{}, err
	}
	if check != nil {
		if err := check(obj); err != nil {
			return obj, err
		}
	}
	opts := metav1.DeleteOptions{}
	if uid := obj.Pop.UID; uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := c.ri.Delete(ctx, name, opts); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return PopObject{}, ErrPopNotFound
		}
		return PopObject{}, fmt.Errorf("kube: delete pop %s: %w", name, err)
	}
	return obj, nil
}

// PatchTLSStatus writes the named tier's certificate state through the
// status subresource: tls.host, tls.certificate, tls.issuedForCSR and
// tls.notAfter, the four paths gawk-admin owns (docs/76 §9). An empty field
// is left as it is. tls.csr is the relays' and is never written, whatever
// the argument carries. The reconciler that calls this is W3-named's
// (popcerts.go).
func (c *PopClient) PatchTLSStatus(ctx context.Context, name string, tls pops.TLS) error {
	body := map[string]any{}
	if tls.Host != "" {
		body["host"] = tls.Host
	}
	if tls.Certificate != "" {
		body["certificate"] = tls.Certificate
	}
	if tls.IssuedForCSR != "" {
		body["issuedForCSR"] = tls.IssuedForCSR
	}
	if tls.NotAfter != nil {
		body["notAfter"] = tls.NotAfter.UTC().Format(time.RFC3339)
	}
	if len(body) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"status": map[string]any{"tls": body}})
	if err != nil {
		return fmt.Errorf("kube: encode pop tls status: %w", err)
	}
	if _, err := c.ri.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}, "status"); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrPopNotFound
		}
		return fmt.Errorf("kube: patch pop %s tls status: %w", name, err)
	}
	return nil
}

// patch sends one merge patch bound to the UID read and decodes the answer.
func (c *PopClient) patch(ctx context.Context, before PopObject, body map[string]any, subresource string) (PopObject, error) {
	if uid := before.Pop.UID; uid != "" {
		body["metadata"] = map[string]any{"uid": string(uid)}
	}
	patch, err := json.Marshal(body)
	if err != nil {
		return before, fmt.Errorf("kube: encode pop patch: %w", err)
	}
	var subs []string
	if subresource != "" {
		subs = []string{subresource}
	}
	u, err := c.ri.Patch(ctx, before.Name, types.MergePatchType, patch, metav1.PatchOptions{}, subs...)
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return before, ErrPopNotFound
		}
		return before, fmt.Errorf("kube: patch pop %s: %w", before.Name, err)
	}
	return decodePop(u), nil
}

// mergeDiff is the RFC 7386 merge patch that turns old into new: a key new
// lacks is null, a nested object recurses, anything else that differs is new's
// value.
func mergeDiff(old, new map[string]any) map[string]any {
	out := map[string]any{}
	for k, ov := range old {
		nv, ok := new[k]
		if !ok {
			out[k] = nil
			continue
		}
		om, oIsMap := ov.(map[string]any)
		nm, nIsMap := nv.(map[string]any)
		if oIsMap && nIsMap {
			if d := mergeDiff(om, nm); len(d) > 0 {
				out[k] = d
			}
			continue
		}
		if !reflect.DeepEqual(ov, nv) {
			out[k] = nv
		}
	}
	for k, nv := range new {
		if _, ok := old[k]; !ok {
			out[k] = nv
		}
	}
	return out
}

// PopOwnerReference is the ownerReference a Pop's wall room carries, so
// deleting the Pop garbage-collects the room (docs/76 item 33). ok is false
// for a Pop with no UID yet, which no API server returns. BlockOwnerDeletion
// stays unset: setting it needs update on the owner's finalizers, which the
// admin Role does not grant.
func PopOwnerReference(p pops.Pop) (metav1.OwnerReference, bool) {
	if p.UID == "" {
		return metav1.OwnerReference{}, false
	}
	return metav1.OwnerReference{
		APIVersion: pops.SchemeGroupVersion.String(),
		Kind:       pops.Kind,
		Name:       p.Name,
		UID:        p.UID,
	}, true
}
