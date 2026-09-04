package kube

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// AnnotationRoomManaged marks a Room CR — or the Secret beside it — as one
// the portal created (R42, docs/44 D20; the Ban twin is AnnotationBanID).
//
// The rule it carries is narrower than the ban one, because rooms have no
// Postgres row to converge against: the portal never deletes an un-annotated
// static Room on its own initiative (a `kubectl apply`'d room is the
// break-glass path, and a sweep that tidied it away would disarm it). The
// ONLY way a static CR leaves the cluster through this package is an explicit
// Delete of that exact name from an operator's own request, annotated or not.
// The annotation decides one thing: whether the Secret a room references is
// ours to delete with it.
const AnnotationRoomManaged = "gawk.ioio.fi/room-managed"

// RoomSecretKey is the key inside a portal-created attach Secret (docs/44
// §4.3's `attachSecretRef.key`).
const RoomSecretKey = "attachSecret"

// AttachSecretLength is the length of a generated attach secret: 24 characters
// of [A-Za-z0-9], about 143 bits. It is shown ONCE — at creation or rotation —
// and never read back by any API route.
const AttachSecretLength = 24

// Sentinel errors. Check with errors.Is; internal/api maps each onto a status.
var (
	// ErrRoomExists: a Room CR with that name already exists.
	ErrRoomExists = errors.New("kube: a room with that code already exists")
	// ErrRoomNotFound: no Room CR with that name.
	ErrRoomNotFound = errors.New("kube: no such room")
	// ErrRoomNotStatic: the operation (secret rotation) only applies to a
	// static room; dynamic rooms have a creator token instead (docs/44 D8).
	ErrRoomNotStatic = errors.New("kube: not a static room")
	// ErrRoomNotDynamic: "end room" addresses a dynamic room; a static room
	// never ends, it is deleted (docs/44 D7).
	ErrRoomNotDynamic = errors.New("kube: not a dynamic room")
)

// RoomSecretName is the deterministic name of the Secret holding a static
// room's attach secret: `room-<code>` (docs/44 §4.3). Deterministic so a
// rotation updates in place and a delete knows what to remove.
func RoomSecretName(code string) string { return "room-" + code }

// RoomObject is one Room CR as the portal sees it.
type RoomObject struct {
	Name string
	// Managed reports the AnnotationRoomManaged stamp: true for a CR the
	// portal created.
	Managed bool
	// Room is the decoded CR. Err is set instead when the object could not be
	// decoded, in which case it is reported by name and never acted on.
	Room rooms.Room
	Err  error
}

// StaticRoom is the portal's create request for a static room, already
// validated by the API layer except for the code, which CreateStatic
// normalizes itself so the CR name and the display form cannot disagree.
type StaticRoom struct {
	// Code is the slug as the operator typed it; it becomes the CR's
	// displayCode, and its normalized form the CR name.
	Code          string
	DisplayName   string
	MaxBroadcasts int
	// WithAttachSecret makes CreateStatic mint a Secret and reference it.
	WithAttachSecret bool
}

// RoomLister is the read surface the reconciler's room sweep needs.
type RoomLister interface {
	List(ctx context.Context) ([]RoomObject, error)
}

// RoomClient manages Room CRs and the attach Secrets beside them through the
// dynamic client (CRs) and the typed clientset (Secrets), in one namespace.
//
// It is the portal's ONLY writer of Secrets, and that is the RBAC posture
// change R42 makes (docs/42 §5 said "no Secrets"): the admin Role gains
// create/get/update/delete on `secrets`, gated in the chart on
// `rooms.enabled`. A portal compromise with rooms on can therefore read any
// Secret in the namespace, not only the room ones — Kubernetes RBAC has no
// per-name grant that survives a create. The chart comment and
// docs/self-hosting.md say so; this comment is where the code admits it.
type RoomClient struct {
	ri      dynamic.ResourceInterface
	secrets secretsInterface
}

// secretsInterface is the slice of the typed clientset RoomClient uses;
// narrowing it keeps the fake in the tests honest about what is exercised.
type secretsInterface interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error)
	Create(ctx context.Context, s *corev1.Secret, opts metav1.CreateOptions) (*corev1.Secret, error)
	Update(ctx context.Context, s *corev1.Secret, opts metav1.UpdateOptions) (*corev1.Secret, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

var _ RoomLister = (*RoomClient)(nil)

// NewRoomClient builds a RoomClient for one namespace from a REST config.
func NewRoomClient(cfg *rest.Config, namespace string) (*RoomClient, error) {
	if namespace == "" {
		return nil, fmt.Errorf("kube: namespace is required")
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: dynamic client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: clientset: %w", err)
	}
	return NewRoomClientFor(dc, cs, namespace), nil
}

// NewRoomClientFor wraps existing clients — the seam the tests use with
// dynamic/fake and kubernetes/fake.
func NewRoomClientFor(dc dynamic.Interface, cs kubernetes.Interface, namespace string) *RoomClient {
	return &RoomClient{
		ri:      dc.Resource(rooms.GroupVersionResource).Namespace(namespace),
		secrets: cs.CoreV1().Secrets(namespace),
	}
}

// List returns every Room CR in the namespace, both kinds. No label selector,
// for the same reason CRClient.List has none: a `kubectl apply`'d static room
// that forgot a label must still be visible here.
func (c *RoomClient) List(ctx context.Context) ([]RoomObject, error) {
	list, err := c.ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("kube: list rooms: %w", err)
	}
	out := make([]RoomObject, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, decodeRoom(&list.Items[i]))
	}
	return out, nil
}

// Get returns one Room CR by name (the normalized code). ErrRoomNotFound when
// there is none.
func (c *RoomClient) Get(ctx context.Context, name string) (RoomObject, error) {
	obj, err := c.ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return RoomObject{}, ErrRoomNotFound
		}
		return RoomObject{}, fmt.Errorf("kube: get room %s: %w", name, err)
	}
	return decodeRoom(obj), nil
}

func decodeRoom(u *unstructured.Unstructured) RoomObject {
	var room rooms.Room
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &room); err != nil {
		return RoomObject{Name: u.GetName(), Err: err}
	}
	return RoomObject{
		Name:    room.Name,
		Managed: room.Annotations[AnnotationRoomManaged] == "true",
		Room:    room,
	}
}

// CreateStatic writes a static Room CR — and, when asked, the attach Secret it
// references — returning the one-time secret (empty when none was requested).
//
// Order matters: the Secret lands FIRST, so a relay pod whose informer sees the
// new CR never resolves a reference to a Secret that does not exist yet; and a
// CR write that fails releases the Secret it acquired, so a refused create
// leaves nothing behind. The CR create is the reservation — a duplicate name is
// rejected by the API server, which is what ErrRoomExists reports.
func (c *RoomClient) CreateStatic(ctx context.Context, req StaticRoom) (string, error) {
	code, err := rooms.NormalizeCode(req.Code)
	if err != nil {
		return "", err
	}
	room := &rooms.Room{
		TypeMeta: metav1.TypeMeta{APIVersion: rooms.SchemeGroupVersion.String(), Kind: rooms.Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name:        code,
			Annotations: map[string]string{AnnotationRoomManaged: "true"},
		},
		Spec: rooms.RoomSpec{
			Kind:          rooms.KindStatic,
			DisplayCode:   strings.TrimSpace(req.Code),
			DisplayName:   strings.TrimSpace(req.DisplayName),
			MaxBroadcasts: req.MaxBroadcasts,
		},
	}

	secret := ""
	if req.WithAttachSecret {
		secret, err = newAttachSecret()
		if err != nil {
			return "", err
		}
		if err := c.writeSecret(ctx, RoomSecretName(code), secret); err != nil {
			return "", err
		}
		room.Spec.AttachSecretRef = &rooms.SecretKeyRef{Name: RoomSecretName(code), Key: RoomSecretKey}
	}

	obj, err := roomToUnstructured(room)
	if err != nil {
		c.releaseSecret(ctx, room)
		return "", err
	}
	if _, err := c.ri.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		c.releaseSecret(ctx, room)
		if apierrors.IsAlreadyExists(err) {
			return "", ErrRoomExists
		}
		return "", fmt.Errorf("kube: create room %s: %w", code, err)
	}
	return secret, nil
}

// releaseSecret is the error path's undo for CreateStatic: the Secret was
// written for a CR that never landed. Best-effort — the next successful
// create of the same code overwrites it anyway.
func (c *RoomClient) releaseSecret(ctx context.Context, room *rooms.Room) {
	if room.Spec.AttachSecretRef == nil {
		return
	}
	_ = c.secrets.Delete(ctx, room.Spec.AttachSecretRef.Name, metav1.DeleteOptions{})
}

// RotateSecret mints a fresh attach secret for a static room and returns it
// once. Every client holding the previous value is refused at its next attach
// (docs/44 D8): the relay reads the referenced Secret per join, so writing
// the Secret in place is the whole rotation — no CR bump is needed, and none
// is made (a spec edit would only re-home nothing and wake every informer).
//
// The Secret written is the one the CR REFERENCES — not necessarily
// `room-<code>` — because a `kubectl apply`'d room may point anywhere in the
// namespace, and rotating a different Secret than the relay reads would change
// nothing while telling the operator it had. A room with no reference yet gets
// `room-<code>` and a merge patch adding the reference, which is how a
// secret-less static room grows a gate without being recreated.
func (c *RoomClient) RotateSecret(ctx context.Context, name string) (string, error) {
	obj, err := c.Get(ctx, name)
	if err != nil {
		return "", err
	}
	if obj.Err != nil {
		return "", fmt.Errorf("kube: room %s could not be decoded: %w", name, obj.Err)
	}
	if obj.Room.Spec.Kind != rooms.KindStatic {
		return "", ErrRoomNotStatic
	}
	secret, err := newAttachSecret()
	if err != nil {
		return "", err
	}
	ref := obj.Room.Spec.AttachSecretRef
	if ref != nil && ref.Name != "" && ref.Key != "" {
		if err := c.writeSecretKey(ctx, ref.Name, ref.Key, secret); err != nil {
			return "", err
		}
		return secret, nil
	}
	secretName := RoomSecretName(obj.Name)
	if err := c.writeSecret(ctx, secretName, secret); err != nil {
		return "", err
	}
	patch := fmt.Appendf(nil, `{"spec":{"attachSecretRef":{"name":%q,"key":%q}}}`, secretName, RoomSecretKey)
	if _, err := c.ri.Patch(ctx, obj.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		// The Secret without a reference is inert; leaving it costs nothing
		// and a retry reuses the name.
		return "", fmt.Errorf("kube: reference the new attach secret on room %s: %w", obj.Name, err)
	}
	return secret, nil
}

// Delete removes a Room CR of either kind. For a dynamic room this IS "end
// room" (docs/44 D20): every relay pod's informer sees the deletion and the
// home pod closes the sessions with 4007. For a static room it also removes
// the attach Secret, but only one the portal created (annotated) — an
// operator's own Secret referenced from a `kubectl apply`'d room is theirs.
//
// A CR that is already gone is success, as with bans. ErrRoomNotFound is for
// callers that need to distinguish "ended" from "was not there"; see
// DeleteExisting.
func (c *RoomClient) Delete(ctx context.Context, name string) error {
	_, err := c.DeleteExisting(ctx, name)
	if errors.Is(err, ErrRoomNotFound) {
		return nil
	}
	return err
}

// DeleteExisting deletes the named room and returns what it was, or
// ErrRoomNotFound. The API uses it: a 404 for a room that was never there is
// more honest than a 204, and the room's kind decides which event to record.
func (c *RoomClient) DeleteExisting(ctx context.Context, name string) (RoomObject, error) {
	obj, err := c.Get(ctx, name)
	if err != nil {
		return RoomObject{}, err
	}
	if err := c.ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return RoomObject{}, ErrRoomNotFound
		}
		return RoomObject{}, fmt.Errorf("kube: delete room %s: %w", name, err)
	}
	if obj.Err != nil || obj.Room.Spec.Kind != rooms.KindStatic || obj.Room.Spec.AttachSecretRef == nil {
		return obj, nil
	}
	// The CR is gone; the Secret is cleanup, not part of the answer. Only a
	// Secret this portal stamped is deleted — see the type comment.
	secretName := obj.Room.Spec.AttachSecretRef.Name
	existing, err := c.secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return obj, nil
		}
		return obj, fmt.Errorf("kube: room %s deleted, but its attach secret %s could not be read: %w", name, secretName, err)
	}
	if existing.Annotations[AnnotationRoomManaged] != "true" {
		return obj, nil
	}
	if err := c.secrets.Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return obj, fmt.Errorf("kube: room %s deleted, but its attach secret %s was not: %w", name, secretName, err)
	}
	return obj, nil
}

// writeSecret creates or replaces the portal-named Secret with one key.
func (c *RoomClient) writeSecret(ctx context.Context, name, value string) error {
	return c.writeSecretKey(ctx, name, RoomSecretKey, value)
}

// writeSecretKey creates the Secret if absent, otherwise sets ONE key on it —
// a Secret an operator pointed a room at may carry other keys, and rotating an
// attach secret must not erase them. Portal-created Secrets are stamped with
// AnnotationRoomManaged at creation, which is what Delete checks.
func (c *RoomClient) writeSecretKey(ctx context.Context, name, key, value string) error {
	existing, err := c.secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Annotations: map[string]string{AnnotationRoomManaged: "true"},
			},
			Type:       corev1.SecretTypeOpaque,
			StringData: map[string]string{key: value},
		}
		if _, err := c.secrets.Create(ctx, s, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("kube: create attach secret %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("kube: get attach secret %s: %w", name, err)
	}
	if existing.Data == nil {
		existing.Data = map[string][]byte{}
	}
	existing.Data[key] = []byte(value)
	// StringData is write-only merge input; the fake clientset stores it
	// verbatim, the real API server folds it into Data. Setting Data directly
	// behaves identically on both.
	existing.StringData = nil
	if _, err := c.secrets.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("kube: update attach secret %s: %w", name, err)
	}
	return nil
}

// attachSecretAlphabet is unambiguous in a chat message and needs no escaping
// in a URL query (`?attach=`) or a shell profile.
const attachSecretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// newAttachSecret draws AttachSecretLength characters uniformly from the
// alphabet with crypto/rand — rand.Int rather than a byte modulus, so no
// character is more likely than another.
func newAttachSecret() (string, error) {
	var sb strings.Builder
	sb.Grow(AttachSecretLength)
	max := big.NewInt(int64(len(attachSecretAlphabet)))
	for range AttachSecretLength {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("kube: generate attach secret: %w", err)
		}
		sb.WriteByte(attachSecretAlphabet[n.Int64()])
	}
	return sb.String(), nil
}

func roomToUnstructured(room *rooms.Room) (*unstructured.Unstructured, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(room)
	if err != nil {
		return nil, fmt.Errorf("kube: encode room %s: %w", room.Name, err)
	}
	return &unstructured.Unstructured{Object: raw}, nil
}
