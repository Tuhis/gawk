package serve

import (
	"context"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// Once the store is installed every seam reaches it, read late-bound (the
// registry is built before the store exists): Reserve writes the Room CR,
// AttachSecret answers from the store's cache instead of failing closed,
// and Unreserve deletes the CR again.
func TestRoomClusterSeamsReachTheInstalledStore(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{rooms.GroupVersionResource: rooms.ListKind})
	var st *roomcluster.Store
	var ro roomsrv.Options
	wireRoomClusterSeams(&ro, func() *roomcluster.Store { return st })

	var err error
	st, err = roomcluster.New(roomcluster.Options{
		Client:        client,
		Namespace:     "gawk",
		PodName:       "relay-0",
		AdvertiseAddr: "10.0.0.1:4433",
		Registry:      roomsrv.NewRegistry(roomsrv.Options{}),
		Log:           slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	crs := client.Resource(rooms.GroupVersionResource).Namespace("gawk")
	room := &rooms.Room{ObjectMeta: metav1.ObjectMeta{Name: "k7xq2m"}, Spec: rooms.RoomSpec{Kind: rooms.KindDynamic}}
	if err := ro.Reserve(ctx, room); err != nil {
		t.Fatalf("Reserve through the seam = %v, want the store's nil", err)
	}
	if _, err := crs.Get(ctx, "k7xq2m", metav1.GetOptions{}); err != nil {
		t.Fatalf("Reserve never reached the store: %v", err)
	}
	if secret, found, err := ro.AttachSecret("unknownroom"); err != nil || found || secret != "" {
		t.Errorf("AttachSecret through the seam = %q %v %v, want the store's not-found", secret, found, err)
	}
	// R79: the voice epoch is the lease generation Reserve took (1), and the
	// fleet voice count reads the store.
	if gen, held := ro.VoiceEpoch("k7xq2m"); !held || gen != 1 {
		t.Errorf("VoiceEpoch through the seam = %d %v, want the lease generation 1", gen, held)
	}
	if n := ro.VoiceRoomsInUse(); n != 0 {
		t.Errorf("VoiceRoomsInUse = %d with an unsynced cache", n)
	}
	ro.OnRoomEmpty("k7xq2m", false)
	ro.OnAttachmentsChanged("k7xq2m", nil)
	ro.Unreserve(ctx, "k7xq2m")
	if _, err := crs.Get(ctx, "k7xq2m", metav1.GetOptions{}); err == nil {
		t.Error("Unreserve through the seam left the Room CR")
	}
	ro.OnRoomEnded("k7xq2m", 0)
}

// Before the store exists the voice seams fail closed: no epoch (VoiceJoin
// answers Unavailable, a registration is refused) and no fleet count.
func TestVoiceSeamsBeforeTheStore(t *testing.T) {
	var ro roomsrv.Options
	wireRoomClusterSeams(&ro, func() *roomcluster.Store { return nil })
	if _, held := ro.VoiceEpoch("k7xq2m"); held {
		t.Error("an epoch without a store")
	}
	if n := ro.VoiceRoomsInUse(); n != 0 {
		t.Errorf("VoiceRoomsInUse = %d", n)
	}
}
