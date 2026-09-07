// Package roomcluster is the cluster-mode half of rooms (R42, docs/44 §4.3,
// §4.5, RM3): the Room custom resource as the fleet's shared truth, and the
// home-pod lease on its status with the R17 origin-lease semantics
// (internal/cluster) — claim, generation CAS, renew, release on drain,
// force-take on a stale lease, a leaderless janitor.
//
// It sits between the room registry (internal/roomsrv, which knows nothing
// about Kubernetes) and the Kubernetes API (a dynamic client, the R39 Ban
// pattern: hand-written types, no code generator, no controller-runtime).
// The registry's cluster seams — Reserve, OnRoomEnded, OnRoomEmpty,
// OnAttachmentsChanged — are implemented here; the transport asks it who
// homes a room (Resolve), whether this pod does (Holding), and to take one
// over (Adopt).
//
// Like internal/cluster, the whole package is dormant unless -cluster-mode
// is on: no Kubernetes client is constructed otherwise, so a single-pod relay
// with rooms is byte-identical to RM2 (docs/44 §4.3, "non-cluster mode").
package roomcluster

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"

	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Defaults. The lease timings mirror internal/cluster (docs/22 Decision 8):
// renew every ~5 s against a 15 s lease, so a dead home is adoptable within
// one lease duration — comfortably inside the client reconnect window. The
// stale window is the janitor's "long window" (docs/44 §4.5): a dynamic
// room whose home has been silent this long AND whose roster emptied
// longer than the empty grace ago is garbage.
const (
	DefaultLeaseDuration   = 15 * time.Second
	DefaultRenewInterval   = 5 * time.Second
	DefaultJanitorInterval = time.Minute
	DefaultStaleWindow     = 5 * time.Minute
	defaultSyncTimeout     = 30 * time.Second
	// claimRetries bounds the CAS loop; a conflict means another claimant
	// is racing and a fresh Get re-evaluates who should win.
	claimRetries = 3
)

// Sentinel errors. Check with errors.Is. roomsrv.ErrUnavailable and
// roomsrv.ErrMaxRooms are reused for the API-unreachable and fleet-limit
// cases so the registry's Reserve contract holds without a second
// vocabulary.
var (
	// ErrHeldElsewhere: the room has a live (renewing) home that is not
	// this pod and the claim was not a force-take — proxy to it.
	ErrHeldElsewhere = errors.New("roomcluster: room lease held by a live home")
	// ErrNotFound: no Room CR for the code.
	ErrNotFound = errors.New("roomcluster: no Room for code")
	errLost     = errors.New("roomcluster: lease lost")
)

// Registry is the store's slice of *roomsrv.Registry.
type Registry interface {
	UpsertStatic(roomsrv.StaticRoom) error
	AdoptDynamic(*rooms.Room) bool
	EndRoom(code string, reason uint8)
}

// Options configures a Store.
type Options struct {
	Client    dynamic.Interface
	Namespace string
	PodName   string
	// AdvertiseAddr is this pod's dialable address ("<podIP>:4433") written
	// into the lease for /internal/room proxying (docs/44 §4.5).
	AdvertiseAddr string
	// MaxRooms caps live dynamic rooms fleet-wide, enforced at CR create
	// (docs/44 §4.10). 0 = unlimited.
	MaxRooms int
	// EmptyGrace is the registry's -room-empty-grace; the janitor requires
	// emptySince to be older than it.
	EmptyGrace time.Duration
	Registry   Registry
	// Obfuscate is the fleet's HMAC (hub.ObfuscateID): status.key is written
	// with it so gawk-admin can name a room without its code (docs/44 D16).
	Obfuscate func(string) string
	Log       *slog.Logger
	// OnLeaseLost fires when a room this pod holds turns out to be held by
	// someone else (force-taken after a stale renew). The transport closes
	// the room's sessions non-terminally and drops the local copy.
	OnLeaseLost func(code string)

	// Test seams. Zero values mean production defaults.
	LeaseDuration   time.Duration
	RenewInterval   time.Duration
	JanitorInterval time.Duration
	StaleWindow     time.Duration
	SyncTimeout     time.Duration
	Now             func() time.Time
}

// Store owns this pod's room leases, their renew loops, the Room informer
// and the janitor.
type Store struct {
	opts     Options
	informer cache.SharedIndexInformer

	mu   sync.Mutex
	held map[string]*heldLease
}

type heldLease struct {
	generation int64
	cancel     context.CancelFunc
	done       chan struct{}
}

// Home is what the transport needs to route a join: who holds the room,
// where, at which generation, and whether that holder is still renewing.
type Home struct {
	Kind       string
	Holder     string
	Addr       string
	Generation int64
	// Live is true when Holder is set and RenewedAt is within the lease
	// duration; false means absent, released (drain) or stale (crash) — all
	// three are adoptable without a force.
	Live bool
}

// New builds a Store. Run must be called for the informer + janitor.
func New(opts Options) (*Store, error) {
	if opts.Client == nil {
		return nil, errors.New("roomcluster: Options.Client is required")
	}
	if opts.Namespace == "" || opts.PodName == "" || opts.AdvertiseAddr == "" {
		return nil, errors.New("roomcluster: Namespace, PodName and AdvertiseAddr are required")
	}
	if opts.Registry == nil {
		return nil, errors.New("roomcluster: Options.Registry is required")
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = DefaultLeaseDuration
	}
	if opts.RenewInterval <= 0 {
		opts.RenewInterval = DefaultRenewInterval
	}
	if opts.JanitorInterval <= 0 {
		opts.JanitorInterval = DefaultJanitorInterval
	}
	if opts.StaleWindow <= 0 {
		opts.StaleWindow = DefaultStaleWindow
	}
	if opts.SyncTimeout <= 0 {
		opts.SyncTimeout = defaultSyncTimeout
	}
	if opts.EmptyGrace <= 0 {
		opts.EmptyGrace = roomsrv.DefaultEmptyGrace
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Obfuscate == nil {
		opts.Obfuscate = func(s string) string { return s }
	}
	s := &Store{opts: opts, held: make(map[string]*heldLease)}
	s.informer = cache.NewSharedIndexInformer(RoomListerWatcher(opts.Client, opts.Namespace),
		&unstructured.Unstructured{}, 0, cache.Indexers{})
	_, _ = s.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { s.observe(nil, obj) },
		UpdateFunc: s.observe,
		DeleteFunc: s.observeDelete,
	})
	return s, nil
}

// secretGVR addresses core Secrets through the same dynamic client, so the
// store needs one client and the chart grants `secrets` get only with rooms
// on (rbac.yaml explains).
var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// RoomListerWatcher lists and watches every Room in a namespace. No label
// selector, for the reason moderationsrc.BanListerWatcher has none: a
// `kubectl apply` that forgot a label must not produce a room nobody homes.
func RoomListerWatcher(client dynamic.Interface, namespace string) cache.ListerWatcher {
	ri := client.Resource(rooms.GroupVersionResource).Namespace(namespace)
	return &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return ri.List(context.Background(), options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return ri.Watch(context.Background(), options)
		},
	}
}

func (s *Store) rooms() dynamic.ResourceInterface {
	return s.opts.Client.Resource(rooms.GroupVersionResource).Namespace(s.opts.Namespace)
}

func (s *Store) now() time.Time { return s.opts.Now() }

// roomFrom converts an informer/API object into a typed Room.
func roomFrom(obj any) (*rooms.Room, error) {
	switch o := obj.(type) {
	case *rooms.Room:
		return o, nil
	case *unstructured.Unstructured:
		var r rooms.Room
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &r); err != nil {
			// No name in the text: the conversion error is logged as-is and
			// the name is the code (docs/44 D16).
			return nil, fmt.Errorf("room CR: %w", err)
		}
		return &r, nil
	default:
		return nil, fmt.Errorf("unexpected object type %T", obj)
	}
}

func toUnstructured(r *rooms.Room) (*unstructured.Unstructured, error) {
	r.TypeMeta = metav1.TypeMeta{APIVersion: rooms.SchemeGroupVersion.String(), Kind: rooms.Kind}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(r)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: raw}, nil
}

// describe renders an API error without the object name Kubernetes quotes
// in its message (`rooms "k7xq2m" is forbidden`, `secrets "room-k7xq2m"
// not found`): the name IS the code, and every store error ends up in a
// log line (docs/44 D16). The reason and status code are what an operator
// needs; the name is what they must not see.
func describe(err error) string {
	var st apierrors.APIStatus
	if errors.As(err, &st) {
		status := st.Status()
		reason := string(status.Reason)
		if reason == "" {
			reason = "error"
		}
		return fmt.Sprintf("kubernetes api: %s (%d)", reason, status.Code)
	}
	return err.Error()
}

// apiError is an API error whose text is describe(err) and whose chain is
// the original: errors.Is / apierrors.IsConflict still see through it,
// only the message changes.
type apiError struct{ err error }

func (e apiError) Error() string { return describe(e.err) }
func (e apiError) Unwrap() error { return e.err }

// unavailable wraps an API failure as roomsrv.ErrUnavailable (503) without
// the object name.
func unavailable(err error) error {
	return fmt.Errorf("%w: %w", roomsrv.ErrUnavailable, apiError{err})
}

// get reads the CR from the API (never the cache: every write path needs
// the current resourceVersion).
func (s *Store) get(ctx context.Context, code string) (*rooms.Room, error) {
	u, err := s.rooms().Get(ctx, code, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, unavailable(err)
	}
	return roomFrom(u)
}

// updateStatus writes r.Status through the status subresource with r's
// resourceVersion as the CAS fence.
func (s *Store) updateStatus(ctx context.Context, r *rooms.Room) (*rooms.Room, error) {
	u, err := toUnstructured(r)
	if err != nil {
		return nil, err
	}
	out, err := s.rooms().UpdateStatus(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return roomFrom(out)
}

// stale reports whether a lease's renewedAt is older than window.
func (s *Store) stale(l *rooms.Lease, window time.Duration) bool {
	if l == nil || l.RenewedAt == nil {
		return true
	}
	return s.now().Sub(l.RenewedAt.Time) > window
}

func (s *Store) homeOf(r *rooms.Room) Home {
	h := Home{Kind: r.Spec.Kind}
	if l := r.Status.Lease; l != nil {
		h.Holder, h.Addr, h.Generation = l.Holder, l.Addr, l.Generation
		h.Live = l.Holder != "" && !s.stale(l, s.opts.LeaseDuration)
	}
	return h
}

// --- lookups (informer cache) -----------------------------------------

func (s *Store) cached(code string) (*rooms.Room, bool) {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return nil, false
	}
	obj, ok, err := s.informer.GetStore().GetByKey(s.opts.Namespace + "/" + norm)
	if err != nil || !ok {
		return nil, false
	}
	r, err := roomFrom(obj)
	if err != nil {
		return nil, false
	}
	return r, true
}

// Known reports whether a Room CR exists for the code (any home). Chained
// into hub.Options.IDReserved so /publish never mints an ID naming a room
// homed on another pod (docs/44 §4.2 mirror check).
func (s *Store) Known(code string) bool {
	_, ok := s.cached(code)
	return ok
}

// Lookup returns the cached CR for a code.
func (s *Store) Lookup(code string) (*rooms.Room, bool) { return s.cached(code) }

// Resolve returns the room's current home from the informer cache.
func (s *Store) Resolve(code string) (Home, bool) {
	r, ok := s.cached(code)
	if !ok {
		return Home{}, false
	}
	return s.homeOf(r), true
}

// Holding reports whether this pod holds the room's lease and at which
// generation — the /internal/room 404 (not home) vs 409 (stale generation)
// split, the twin of cluster.OriginGeneration.
func (s *Store) Holding(code string) (int64, bool) {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.held[norm]
	if !ok {
		return 0, false
	}
	return h.generation, true
}

func (s *Store) countDynamic() int {
	n := 0
	for _, obj := range s.informer.GetStore().List() {
		if r, err := roomFrom(obj); err == nil && r.Spec.Kind == rooms.KindDynamic {
			n++
		}
	}
	return n
}

// --- registry seams ---------------------------------------------------

// Reserve is roomsrv.Options.Reserve: the CR create is the atomic
// reservation of a dynamic code (docs/44 §4.2). The fleet-wide -max-rooms
// binds here, counted from the informer cache. AlreadyExists means the code
// is taken (any error other than the two pass-throughs makes Mint re-mint);
// an unreachable API is roomsrv.ErrUnavailable (503).
func (s *Store) Reserve(ctx context.Context, room *rooms.Room) error {
	if s.opts.MaxRooms > 0 && s.countDynamic() >= s.opts.MaxRooms {
		return roomsrv.ErrMaxRooms
	}
	now := metav1.NewTime(s.now())
	cr := room.DeepCopy()
	cr.Namespace = s.opts.Namespace
	cr.Status.Key = s.opts.Obfuscate(cr.Name)
	if cr.Status.CreatedAt == nil {
		cr.Status.CreatedAt = &now
	}
	// Mint starts the empty grace (docs/44 §4.4 step 1), so the janitor's
	// clock starts here too.
	cr.Status.EmptySince = &now
	cr.Status.Lease = &rooms.Lease{Holder: s.opts.PodName, Addr: s.opts.AdvertiseAddr, Generation: 1, RenewedAt: &now}
	u, err := toUnstructured(cr)
	if err != nil {
		return err
	}
	created, err := s.rooms().Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("roomcluster: code taken: %w", apiError{err})
		}
		return unavailable(err)
	}
	// The status subresource drops status on create, so the lease is
	// written in a second request against the created object's version.
	cr.ResourceVersion = created.GetResourceVersion()
	if _, err := s.updateStatus(ctx, cr); err != nil {
		_ = s.rooms().Delete(ctx, cr.Name, metav1.DeleteOptions{})
		return unavailable(err)
	}
	s.startRenew(cr.Name, 1)
	s.opts.Log.Info("room reserved", "room_key", cr.Status.Key)
	return nil
}

// RoomEnded is roomsrv.Options.OnRoomEnded: stop renewing and, for a
// dynamic room this pod holds (or whose lease was released), delete the CR
// so every pod's informer sees the end. Holder-gated for the reason
// cluster.Delete is: a stale home reaching its grace expiry must not end a
// room that lives on under a new home.
func (s *Store) RoomEnded(code string, _ uint8) {
	s.stopRenew(code)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.get(ctx, code)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.opts.Log.Warn("room end: get failed", "room_key", s.opts.Obfuscate(code), "err", err)
		}
		return
	}
	if r.Spec.Kind != rooms.KindDynamic {
		return
	}
	if l := r.Status.Lease; l != nil && l.Holder != "" && l.Holder != s.opts.PodName {
		return
	}
	err = s.rooms().Delete(ctx, r.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{ResourceVersion: &r.ResourceVersion},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		s.opts.Log.Warn("room end: delete failed", "room_key", s.opts.Obfuscate(code), "err", err)
	}
}

// Unreserve is roomsrv.Options.Unreserve: Reserve created the CR and
// started renewing its lease, then Mint found the code taken locally (an
// adoption raced the mint). Give both back — the CR would otherwise count
// against -max-rooms until the janitor's long window. The delete is fenced
// on the version this pod wrote; a CR that moved since belongs to whoever
// moved it, and one already gone is fine.
func (s *Store) Unreserve(ctx context.Context, code string) {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return
	}
	s.stopRenew(norm)
	r, err := s.get(ctx, norm)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.opts.Log.Warn("room unreserve: get failed", "room_key", s.opts.Obfuscate(norm), "err", err)
		}
		return
	}
	if l := r.Status.Lease; l == nil || l.Holder != s.opts.PodName {
		return
	}
	err = s.rooms().Delete(ctx, r.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{ResourceVersion: &r.ResourceVersion},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		s.opts.Log.Warn("room unreserve: delete failed", "room_key", s.opts.Obfuscate(norm), "err", describe(err))
	}
}

// RoomEmpty is roomsrv.Options.OnRoomEmpty: stamp or clear
// status.emptySince (docs/44 §4.4 step 3).
func (s *Store) RoomEmpty(code string, empty bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.patchStatus(ctx, code, func(st *rooms.RoomStatus) {
		if empty {
			now := metav1.NewTime(s.now())
			st.EmptySince = &now
		} else {
			st.EmptySince = nil
		}
	})
	if err != nil && !errors.Is(err, errLost) && !errors.Is(err, ErrNotFound) {
		s.opts.Log.Warn("room emptySince write failed", "room_key", s.opts.Obfuscate(code), "err", err)
	}
}

// AttachmentsChanged is roomsrv.Options.OnAttachmentsChanged: rewrite
// status.attachments so the next home can rebuild them (docs/44 D5).
func (s *Store) AttachmentsChanged(code string, list []rooms.Attachment) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.patchStatus(ctx, code, func(st *rooms.RoomStatus) {
		st.Attachments = append([]rooms.Attachment(nil), list...)
	})
	if err != nil && !errors.Is(err, errLost) && !errors.Is(err, ErrNotFound) {
		s.opts.Log.Warn("room attachments write failed", "room_key", s.opts.Obfuscate(code), "err", err)
	}
}

// patchStatus applies fn to the CR's status under the generation fence: a
// pod that no longer holds the lease at the generation it believes writes
// nothing (errLost). Conflicts are retried from a fresh Get.
func (s *Store) patchStatus(ctx context.Context, code string, fn func(*rooms.RoomStatus)) error {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return ErrNotFound
	}
	var lastErr error
	for range claimRetries {
		gen, held := s.Holding(norm)
		if !held {
			return errLost
		}
		r, err := s.get(ctx, norm)
		if err != nil {
			return err
		}
		if l := r.Status.Lease; l == nil || l.Holder != s.opts.PodName || l.Generation != gen {
			return errLost
		}
		fn(&r.Status)
		if _, err := s.updateStatus(ctx, r); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue
			}
			return unavailable(err)
		}
		return nil
	}
	return fmt.Errorf("roomcluster: status write retries exhausted: %w", apiError{lastErr})
}

// --- lease ------------------------------------------------------------

// Claim acquires the room's lease for this pod (docs/44 §4.5): takeable
// when the holder is empty (released on drain), this pod, stale (crash),
// or when force is set. Returns the new generation; a live foreign holder
// is ErrHeldElsewhere.
func (s *Store) Claim(ctx context.Context, code string, force bool) (int64, error) {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return 0, ErrNotFound
	}
	var lastErr error
	for range claimRetries {
		r, err := s.get(ctx, norm)
		if err != nil {
			return 0, err
		}
		home := s.homeOf(r)
		if home.Live && home.Holder != s.opts.PodName && !force {
			return 0, fmt.Errorf("%w: %s", ErrHeldElsewhere, home.Holder)
		}
		gen := home.Generation + 1
		now := metav1.NewTime(s.now())
		r.Status.Lease = &rooms.Lease{Holder: s.opts.PodName, Addr: s.opts.AdvertiseAddr, Generation: gen, RenewedAt: &now}
		r.Status.Key = s.opts.Obfuscate(norm)
		if r.Status.CreatedAt == nil {
			r.Status.CreatedAt = &now
		}
		if r.Spec.Kind == rooms.KindDynamic {
			// The adopting pod starts with an empty roster (docs/44 §4.5);
			// the first join clears this through OnRoomEmpty.
			r.Status.EmptySince = &now
		}
		if _, err := s.updateStatus(ctx, r); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue
			}
			return 0, unavailable(err)
		}
		s.startRenew(norm, gen)
		return gen, nil
	}
	return 0, fmt.Errorf("roomcluster: claim retries exhausted for room: %w", apiError{lastErr})
}

// Adopt takes the room over (docs/44 §4.5 "Adoption"): claim the lease
// (force only when the current one is stale, never against a live home)
// and rebuild the room locally — a dynamic room from the CR's attachments,
// a static one from its spec (attach secret read from the referenced
// Secret). A live foreign home is ErrHeldElsewhere; the caller proxies.
func (s *Store) Adopt(ctx context.Context, code string) error {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return ErrNotFound
	}
	r, err := s.get(ctx, norm)
	if err != nil {
		return err
	}
	home := s.homeOf(r)
	if home.Live && home.Holder != s.opts.PodName {
		return fmt.Errorf("%w: %s", ErrHeldElsewhere, home.Holder)
	}
	def, err := s.staticDef(ctx, r)
	if err != nil {
		return err
	}
	gen, err := s.Claim(ctx, norm, home.Holder != "" && !home.Live)
	if err != nil {
		return err
	}
	// Re-read: the CR's attachments may have moved since the first Get.
	if r, err = s.get(ctx, norm); err != nil {
		s.stopRenew(norm)
		return err
	}
	switch r.Spec.Kind {
	case rooms.KindDynamic:
		if !s.opts.Registry.AdoptDynamic(r) {
			s.opts.Log.Debug("room adopted with a local copy already present", "room_key", r.Status.Key)
		}
	default:
		// The re-read CR's attachments seed the rebuilt room (docs/44 §4.3
		// "rebuilt by the home pod on adoption"), as AdoptDynamic's do; the
		// registry ignores them on a plain refresh of a room it holds.
		def.Attachments = append([]rooms.Attachment(nil), r.Status.Attachments...)
		if err := s.opts.Registry.UpsertStatic(def); err != nil {
			s.stopRenew(norm)
			return err
		}
	}
	s.opts.Log.Info("room adopted", "room_key", s.opts.Obfuscate(norm), "kind", r.Spec.Kind, "generation", gen, "previous_holder", home.Holder)
	return nil
}

// staticDef resolves a static CR's registry definition, including the
// attach secret. Fails closed: a room whose secret cannot be read is not
// homed here, rather than homed with attach open.
func (s *Store) staticDef(ctx context.Context, r *rooms.Room) (roomsrv.StaticRoom, error) {
	def := roomsrv.StaticRoom{Code: r.Name, DisplayCode: r.Spec.DisplayCode, DisplayName: r.Spec.DisplayName, MaxBroadcasts: r.Spec.MaxBroadcasts}
	if r.Spec.Kind == rooms.KindDynamic || r.Spec.AttachSecretRef == nil {
		return def, nil
	}
	secret, err := s.readAttachSecret(ctx, r.Spec.AttachSecretRef)
	if err != nil {
		return def, err
	}
	def.AttachSecret = secret
	return def, nil
}

// readAttachSecret reads one key of the referenced Secret. Errors name
// neither the Secret (conventionally `room-<code>`) nor the key: they go to
// logs and to the join's 503 (docs/44 D16).
func (s *Store) readAttachSecret(ctx context.Context, ref *rooms.SecretKeyRef) (string, error) {
	u, err := s.opts.Client.Resource(secretGVR).Namespace(s.opts.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("%w: attach secret: %w", roomsrv.ErrUnavailable, apiError{err})
	}
	data, _, _ := unstructured.NestedMap(u.Object, "data")
	enc, ok := data[ref.Key].(string)
	if !ok {
		return "", fmt.Errorf("%w: attach secret: referenced key missing", roomsrv.ErrUnavailable)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("%w: attach secret: referenced key is not base64", roomsrv.ErrUnavailable)
	}
	return string(raw), nil
}

// AttachSecret is roomsrv.Options.AttachSecret: the attach secret of a
// static room resolved AT JOIN TIME from its Secret (review finding A). The
// portal rotates the Secret in place and never touches the CR, so a copy
// taken at adoption would keep admitting the old value; reading per join
// makes a rotation bind at the next join with no CR bump. A code without a
// CR in the cache (a file-defined room) or a CR without a reference is
// found=false — the registry's inline definition rules; a reference that
// cannot be honoured is an error, and the join fails closed (503).
func (s *Store) AttachSecret(code string) (string, bool, error) {
	r, ok := s.cached(code)
	if !ok || r.Spec.Kind == rooms.KindDynamic || r.Spec.AttachSecretRef == nil {
		return "", false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	secret, err := s.readAttachSecret(ctx, r.Spec.AttachSecretRef)
	if err != nil {
		return "", false, err
	}
	return secret, true, nil
}

func (s *Store) startRenew(code string, generation int64) {
	s.mu.Lock()
	if prev, ok := s.held[code]; ok {
		prev.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &heldLease{generation: generation, cancel: cancel, done: make(chan struct{})}
	s.held[code] = h
	s.mu.Unlock()
	go s.renewLoop(ctx, code, h)
}

func (s *Store) renewLoop(ctx context.Context, code string, h *heldLease) {
	defer close(h.done)
	ticker := time.NewTicker(s.opts.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := s.renewOnce(ctx, code, h); err != nil {
			if errors.Is(err, errLost) {
				return
			}
			s.opts.Log.Warn("room lease renew failed", "room_key", s.opts.Obfuscate(code), "err", err)
		}
	}
}

func (s *Store) renewOnce(ctx context.Context, code string, h *heldLease) error {
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := s.get(opCtx, code)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The CR is gone (ended elsewhere); the informer's delete
			// handler ends the room locally. Nothing left to renew.
			s.forgetHeld(code, h)
			return errLost
		}
		return err
	}
	if l := r.Status.Lease; l == nil || l.Holder != s.opts.PodName || l.Generation != h.generation {
		// Force-taken from under us: the stale home must never fight the
		// new one (docs/44 §4.5 "Fencing").
		if s.forgetHeld(code, h) {
			s.lost(code)
		}
		return errLost
	}
	now := metav1.NewTime(s.now())
	r.Status.Lease.RenewedAt = &now
	if _, err := s.updateStatus(opCtx, r); err != nil {
		return unavailable(err)
	}
	return nil
}

// forgetHeld drops h if it is still the held record for code; reports
// whether it was.
func (s *Store) forgetHeld(code string, h *heldLease) bool {
	s.mu.Lock()
	cur, ok := s.held[code]
	if ok && cur == h {
		delete(s.held, code)
	}
	s.mu.Unlock()
	h.cancel()
	return ok && cur == h
}

func (s *Store) lost(code string) {
	s.opts.Log.Info("room lease lost", "room_key", s.opts.Obfuscate(code))
	if s.opts.OnLeaseLost != nil {
		s.opts.OnLeaseLost(code)
	}
}

// stopRenew halts the renew loop for a room (if any).
func (s *Store) stopRenew(code string) *heldLease {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	h := s.held[norm]
	delete(s.held, norm)
	s.mu.Unlock()
	if h != nil {
		h.cancel()
		<-h.done
	}
	return h
}

// Release clears this pod's holdership without touching the generation
// (the drain, docs/44 §4.5): the next join claims without waiting for
// staleness.
func (s *Store) Release(ctx context.Context, code string) error {
	s.stopRenew(code)
	// A Conflict is retried from a fresh Get, as patchStatus does (PR #302):
	// the drain closes this pod's sessions first, and the RoomEmpty stamp
	// their leaving triggers is a status write racing this one. Losing that
	// CAS and giving up left the holder in place, so the reconnect had to
	// wait out the staleness window instead of adopting at once.
	var lastErr error
	for range claimRetries {
		r, err := s.get(ctx, code)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		l := r.Status.Lease
		if l == nil || l.Holder != s.opts.PodName {
			return nil
		}
		l.Holder, l.Addr = "", ""
		if _, err := s.updateStatus(ctx, r); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue
			}
			return unavailable(err)
		}
		return nil
	}
	return fmt.Errorf("roomcluster: release retries exhausted: %w", apiError{lastErr})
}

// ReleaseAll releases every room lease this pod holds (drain).
func (s *Store) ReleaseAll(ctx context.Context) {
	s.mu.Lock()
	codes := make([]string, 0, len(s.held))
	for code := range s.held {
		codes = append(codes, code)
	}
	s.mu.Unlock()
	for _, code := range codes {
		if err := s.Release(ctx, code); err != nil {
			s.opts.Log.Warn("room lease release failed during drain", "room_key", s.opts.Obfuscate(code), "err", err)
		}
	}
}

// --- informer ---------------------------------------------------------

// observe handles an added or updated CR: a held room whose lease now
// names someone else is lost (the watch usually reports it before the
// renew loop does), and a held static room whose SPEC changed is refreshed
// in the registry (display name, limits, a changed Secret reference).
// Secret ROTATION is not a spec change and needs none: the secret is read
// per join (AttachSecret). Status-only updates — every renew is one —
// refresh nothing: a Secret read per renew per room would be the
// informer's hot path.
func (s *Store) observe(oldObj, obj any) {
	r, err := roomFrom(obj)
	if err != nil {
		s.opts.Log.Warn("room CR ignored", "err", err)
		return
	}
	specChanged := false
	if oldObj != nil {
		if old, err := roomFrom(oldObj); err == nil {
			specChanged = !reflect.DeepEqual(old.Spec, r.Spec)
		}
	}
	s.mu.Lock()
	h, held := s.held[r.Name]
	lost := held && (r.Status.Lease == nil || r.Status.Lease.Holder != s.opts.PodName || r.Status.Lease.Generation != h.generation)
	if lost {
		delete(s.held, r.Name)
	}
	s.mu.Unlock()
	if lost {
		h.cancel()
		s.lost(r.Name)
		return
	}
	if held && specChanged && r.Spec.Kind == rooms.KindStatic {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			def, err := s.staticDef(ctx, r)
			if err != nil {
				s.opts.Log.Warn("static room spec refresh failed", "room_key", s.opts.Obfuscate(r.Name), "err", err)
				return
			}
			if err := s.opts.Registry.UpsertStatic(def); err != nil {
				s.opts.Log.Warn("static room refresh rejected", "room_key", s.opts.Obfuscate(r.Name), "err", err)
			}
		}()
	}
}

// observeDelete ends the room locally on CR deletion (docs/44 D20: a room
// ends fleet-wide by CR deletion, which every pod's informer sees). For a
// room this pod ended itself the registry entry is already gone and
// EndRoom is a no-op.
func (s *Store) observeDelete(obj any) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	r, err := roomFrom(obj)
	if err != nil {
		s.opts.Log.Warn("room CR delete ignored", "err", err)
		return
	}
	s.mu.Lock()
	h := s.held[r.Name]
	delete(s.held, r.Name)
	s.mu.Unlock()
	if h != nil {
		h.cancel()
	}
	s.opts.Registry.EndRoom(r.Name, wire.RoomEndReasonOperator)
}

// Run starts the informer and the janitor, blocking until ctx ends. Start
// it LAST in the process wiring, like the ban informer: its first events
// reach the registry and the transport within milliseconds.
func (s *Store) Run(ctx context.Context) {
	go s.janitorLoop(ctx)
	go func() {
		syncCtx, cancel := context.WithTimeout(ctx, s.opts.SyncTimeout)
		defer cancel()
		if cache.WaitForCacheSync(syncCtx.Done(), s.informer.HasSynced) {
			s.opts.Log.Info("room informer synced", "rooms", len(s.informer.GetStore().List()))
		} else if ctx.Err() == nil {
			s.opts.Log.Warn("room informer has not synced: rooms homed elsewhere are not yet resolvable", "timeout", s.opts.SyncTimeout)
		}
	}()
	s.informer.RunWithContext(ctx)
}

// HasSynced reports whether the informer's first list has landed (tests).
func (s *Store) HasSynced() bool { return s.informer.HasSynced() }

// --- janitor ----------------------------------------------------------

func (s *Store) janitorLoop(ctx context.Context) {
	ticker := time.NewTicker(s.opts.JanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.JanitorSweep(ctx)
		}
	}
}

// JanitorSweep runs one pass (docs/44 §4.5 "Janitor") over dynamic Room
// CRs whose lease is stale past the long window; static CRs are never
// janitored. One whose emptySince is older than the empty grace is
// deleted. One with NO emptySince — its home crashed while the room was
// populated, so nobody stamped it — is stamped now (review finding C):
// without the stamp it would never qualify while still counting against
// -max-rooms. The stale lease alone is the evidence; a later pass deletes
// it once the stamp has aged past the grace, unless an adoption revives it
// first (Claim rewrites emptySince, the first join clears it). Both writes
// are fenced on resourceVersion so a concurrent adoption wins. Exported for
// tests.
func (s *Store) JanitorSweep(ctx context.Context) {
	now := s.now()
	for _, obj := range s.informer.GetStore().List() {
		r, err := roomFrom(obj)
		if err != nil || r.Spec.Kind != rooms.KindDynamic {
			continue
		}
		if !s.stale(r.Status.Lease, s.opts.StaleWindow) {
			continue
		}
		if r.Status.EmptySince == nil {
			s.stampOrphan(ctx, r, now)
			continue
		}
		if now.Sub(r.Status.EmptySince.Time) <= s.opts.EmptyGrace {
			continue
		}
		err = s.rooms().Delete(ctx, r.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{ResourceVersion: &r.ResourceVersion},
		})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			s.opts.Log.Warn("room janitor delete failed", "room_key", r.Status.Key, "err", describe(err))
			continue
		}
		if err == nil {
			s.opts.Log.Info("room janitor deleted a stale empty room", "room_key", r.Status.Key)
		}
	}
}

// stampOrphan writes emptySince=now on a stale, unstamped dynamic CR from
// the cached copy, so the fence is the version the janitor saw.
func (s *Store) stampOrphan(ctx context.Context, r *rooms.Room, now time.Time) {
	cr := r.DeepCopy()
	stamp := metav1.NewTime(now)
	cr.Status.EmptySince = &stamp
	if _, err := s.updateStatus(ctx, cr); err != nil {
		if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			s.opts.Log.Warn("room janitor stamp failed", "room_key", r.Status.Key, "err", describe(err))
		}
		return
	}
	s.opts.Log.Info("room janitor stamped an orphaned room", "room_key", r.Status.Key)
}
