// Package kube is gawk-admin's Kubernetes half (R39, docs/42 §4.6): the Ban CR
// projection of the Postgres record store, the reconciler/janitor that keeps
// the two converged, and the Lease-based leader election that makes the
// singleton background work safe at replicaCount 2 (D16).
//
// The direction of authority is fixed and worth stating once: **Postgres is
// the system of record, Ban CRs are its projection** (D2/D3). Relay pods watch
// CRs and never call gawk-admin, so enforcement survives a gawk-admin outage
// and a relay cold-start; gawk-admin writes CRs and never asks a relay what is
// banned.
package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// AnnotationBanID marks a CR as the projection of a specific `bans` row.
//
// It is what separates "a CR this service owns" from "a CR an operator applied
// by hand with gawk-admin down" (§4.2). Only annotated CRs are ever deleted;
// an un-annotated one is ADOPTED into Postgres instead, because deleting an
// operator's emergency ban because we had no record of it would be the worst
// possible failure mode for a break-glass path.
const AnnotationBanID = "gawk.ioio.fi/ban-id"

// AdoptedBy is the created_by recorded for a CR adopted from the cluster.
const AdoptedBy = "kubectl"

// BanObject is one Ban CR as the reconciler sees it.
type BanObject struct {
	Name string
	// BanID is the AnnotationBanID value: empty for an operator-applied CR.
	BanID string
	// Record is the normalized spec. Err is set instead when the spec could
	// not be normalized, in which case the object is reported but never acted
	// on — an unparseable ban is left exactly where it is.
	Record moderation.Record
	Err    error
}

// BanClient is the Ban CR surface the reconciler needs. It is an interface so
// the reconciler's convergence, adoption and no-GC-without-Postgres rules can
// be tested without a cluster, and so a future switch of client mechanism does
// not touch reconciliation logic.
type BanClient interface {
	List(ctx context.Context) ([]BanObject, error)
	// Upsert creates or updates the CR named by moderation.CRName(rec.Target),
	// stamping banID into AnnotationBanID.
	Upsert(ctx context.Context, rec moderation.Record, banID string) error
	Delete(ctx context.Context, name string) error
}

// CRClient talks to Ban CRs through the DYNAMIC client.
//
// Why dynamic rather than a typed client: a typed client for a CRD needs
// generated clientset code, and this repository deliberately runs no
// Kubernetes code generators — gawk-server/moderation's deepcopy methods are
// hand-written for the same reason. The dynamic client needs neither generated
// code nor a build-time toolchain, converts through the very scheme
// moderation.AddToScheme already registers, and has a fake
// (k8s.io/client-go/dynamic/fake) that the tests drive as a real client
// surface. The cost is two conversions per object, on a control path that runs
// once a minute.
type CRClient struct {
	ri dynamic.ResourceInterface
}

var _ BanClient = (*CRClient)(nil)

// NewCRClient builds a Ban CR client for one namespace from a REST config.
func NewCRClient(cfg *rest.Config, namespace string) (*CRClient, error) {
	if namespace == "" {
		return nil, fmt.Errorf("kube: namespace is required")
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: dynamic client: %w", err)
	}
	return NewCRClientFor(dc, namespace), nil
}

// NewCRClientFor wraps an existing dynamic client — the seam the tests use
// with dynamic/fake.
func NewCRClientFor(dc dynamic.Interface, namespace string) *CRClient {
	return &CRClient{ri: dc.Resource(moderation.GroupVersionResource).Namespace(namespace)}
}

// List returns every Ban CR in the namespace.
//
// No label selector, deliberately (§4.2 and the rejected-alternatives list):
// every Ban in the namespace is relevant by definition, and requiring a label
// would turn a break-glass `kubectl apply` that forgot it into a ban this
// reconciler cannot see — and would therefore never adopt.
func (c *CRClient) List(ctx context.Context) ([]BanObject, error) {
	list, err := c.ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("kube: list bans: %w", err)
	}
	out := make([]BanObject, 0, len(list.Items))
	for i := range list.Items {
		var ban moderation.Ban
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &ban); err != nil {
			out = append(out, BanObject{Name: list.Items[i].GetName(), Err: err})
			continue
		}
		obj := BanObject{Name: ban.Name, BanID: ban.Annotations[AnnotationBanID]}
		rec, err := moderation.RecordFromBan(&ban)
		if err != nil {
			obj.Err = err
		} else {
			obj.Record = rec
		}
		out = append(out, obj)
	}
	return out, nil
}

// Upsert creates or updates the CR for a record.
//
// The name comes from moderation.CRName — never from a caller — which is what
// makes a re-ban update the existing object instead of accumulating duplicates
// (§4.2) and what makes two gawk-admin replicas writing concurrently converge
// on one object rather than two.
func (c *CRClient) Upsert(ctx context.Context, rec moderation.Record, banID string) error {
	rec, err := moderation.Normalize(rec)
	if err != nil {
		return err
	}
	name, err := moderation.CRName(rec.Target)
	if err != nil {
		return err
	}
	desired := banFor(name, rec, banID)

	existing, err := c.ri.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		obj, err := toUnstructured(desired)
		if err != nil {
			return err
		}
		if _, err := c.ri.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Another replica created it between our Get and Create. The
				// name is deterministic, so their object is the one we wanted.
				return c.update(ctx, name, desired)
			}
			return fmt.Errorf("kube: create ban %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("kube: get ban %s: %w", name, err)
	default:
		desired.ResourceVersion = existing.GetResourceVersion()
		obj, err := toUnstructured(desired)
		if err != nil {
			return err
		}
		if _, err := c.ri.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("kube: update ban %s: %w", name, err)
		}
		return nil
	}
}

func (c *CRClient) update(ctx context.Context, name string, desired *moderation.Ban) error {
	existing, err := c.ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("kube: get ban %s: %w", name, err)
	}
	desired.ResourceVersion = existing.GetResourceVersion()
	obj, err := toUnstructured(desired)
	if err != nil {
		return err
	}
	if _, err := c.ri.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("kube: update ban %s: %w", name, err)
	}
	return nil
}

// Delete removes a Ban CR. A CR that is already gone is success: the caller's
// intent ("this must not be enforced") is satisfied either way.
func (c *CRClient) Delete(ctx context.Context, name string) error {
	err := c.ri.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: delete ban %s: %w", name, err)
	}
	return nil
}

func banFor(name string, rec moderation.Record, banID string) *moderation.Ban {
	ban := &moderation.Ban{
		TypeMeta: metav1.TypeMeta{
			APIVersion: moderation.SchemeGroupVersion.String(),
			Kind:       moderation.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: moderation.BanSpec{
			Target:    rec.Target,
			Reason:    rec.Reason,
			CreatedBy: rec.CreatedBy,
		},
	}
	if rec.ExpiresAt != nil {
		t := metav1.NewTime(*rec.ExpiresAt)
		ban.Spec.ExpiresAt = &t
	}
	if banID != "" {
		ban.Annotations = map[string]string{AnnotationBanID: banID}
	}
	return ban
}

func toUnstructured(ban *moderation.Ban) (*unstructured.Unstructured, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ban)
	if err != nil {
		return nil, fmt.Errorf("kube: encode ban %s: %w", ban.Name, err)
	}
	return &unstructured.Unstructured{Object: raw}, nil
}

// InClusterConfig is the REST config for a pod in the cluster — the only
// deployment shape gawk-admin supports (§4.14: it requires Kubernetes for CRs).
func InClusterConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kube: in-cluster config: %w", err)
	}
	return cfg, nil
}

// NewClientset builds the typed clientset used for the leader-election Lease.
func NewClientset(cfg *rest.Config) (kubernetes.Interface, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: clientset: %w", err)
	}
	return cs, nil
}
