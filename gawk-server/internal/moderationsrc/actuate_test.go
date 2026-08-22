package moderationsrc

// R39 AP3 (docs/42 §4.3, §9): the actuation callback must run on EVERY
// source — the k8s informer's events AND a file reload — because a relay that
// enforces on reconnect but never kills leaves the offending stream on air
// until the broadcaster happens to disconnect.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/watch"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// actuations records what OnBanAdded was called with.
type actuations struct {
	mu   sync.Mutex
	recs []moderation.Record
}

func (a *actuations) add(rec moderation.Record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, rec)
}

func (a *actuations) targets() []moderation.Target {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]moderation.Target, 0, len(a.recs))
	for _, r := range a.recs {
		out = append(out, r.Target)
	}
	return out
}

func (a *actuations) has(t moderation.Target) bool {
	for _, got := range a.targets() {
		if got == t {
			return true
		}
	}
	return false
}

// The file source actuates at startup and on every reload — the dev/compose
// lane (docs/42 §4.14) has to be able to kill, or a developer cannot exercise
// the flow at all without a cluster.
func TestFileSourceActuatesOnLoadAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	write(t, path, `[{"target":{"type":"broadcastId","value":"abc23z"},"reason":"fraud"}]`)

	var got actuations
	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx, Options{
		Source: "file:" + path, Set: set, Log: discardLog,
		PollInterval: 2 * time.Millisecond, OnBanAdded: got.add,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Startup load is synchronous, so the actuation has already happened.
	idTarget := moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}
	if !got.has(idTarget) {
		t.Fatalf("the startup load did not actuate: %v", got.targets())
	}
	// The record handed to the callback is NORMALIZED (uppercased), so the
	// kill path can look the ID up without re-normalizing.
	if v := got.targets()[0].Value; v != "ABC23Z" {
		t.Errorf("actuated target value = %q, want the normalized ABC23Z", v)
	}

	write(t, path, `[
	  {"target":{"type":"broadcastId","value":"abc23z"},"reason":"fraud"},
	  {"target":{"type":"ip","value":"203.0.113.7"},"reason":"abuse"}
	]`)
	ipTarget := moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.7/32"}
	waitFor(t, "the reload to actuate the new CIDR ban", func() bool { return got.has(ipTarget) })
}

// A ban REMOVED from the file actuates nothing: lifting a ban does not
// resurrect a broadcast, so there is nothing to kill.
func TestFileSourceDoesNotActuateRemovals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	write(t, path, `[{"target":{"type":"broadcastId","value":"abc23z"},"reason":"fraud"}]`)

	var got actuations
	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx, Options{
		Source: "file:" + path, Set: set, Log: discardLog,
		PollInterval: 2 * time.Millisecond, OnBanAdded: got.add,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	before := len(got.targets())

	write(t, path, `[]`)
	waitFor(t, "the ban to lift", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return !ok
	})
	// Give any spurious actuation a chance to land before asserting absence.
	time.Sleep(20 * time.Millisecond)
	for _, target := range got.targets()[before:] {
		t.Errorf("an empty ban file actuated %v", target)
	}
}

// The informer actuates on add and on update. Delete drives the Set only.
func TestInformerActuatesAddsAndUpdates(t *testing.T) {
	withListWatchReflector(t)
	fw := watch.NewFake()
	lw := &recordingListerWatcher{watcher: fw}
	set := moderation.NewSet()
	var got actuations

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunInformer(ctx, lw, Sink{Set: set, OnAdded: got.add}, discardLog, time.Second, time.Hour)

	idTarget := moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}
	fw.Add(banObject(t, "ban-id-abc23z", idTarget, "kubectl break-glass", nil))
	waitSet(t, "the add to actuate", func() bool { return got.has(idTarget) })

	// An UPDATE actuates too: re-banning an ID whose broadcaster came back
	// under a lapsed ban has to kill it again.
	ipTarget := moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.7/32"}
	fw.Modify(banObject(t, "ban-ip-deadbeef1234",
		moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.7"}, "abuse", nil))
	waitSet(t, "the update to actuate", func() bool { return got.has(ipTarget) })

	before := len(got.targets())
	fw.Delete(banObject(t, "ban-id-abc23z", idTarget, "lifted", nil))
	waitSet(t, "the delete to reach the Set", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return !ok
	})
	time.Sleep(20 * time.Millisecond)
	for _, target := range got.targets()[before:] {
		t.Errorf("a ban DELETE actuated %v — lifting a ban must kill nothing", target)
	}
}

// ORDER: the Set is updated before the callback fires. Otherwise a
// broadcaster whose session the kill just closed could win the race to
// reclaim its own ID before the gate can reject it (docs/42 §4.1 step 4).
func TestSinkClosesTheGateBeforeActuating(t *testing.T) {
	set := moderation.NewSet()
	var bannedAtCallback bool
	sink := Sink{Set: set, OnAdded: func(rec moderation.Record) {
		_, bannedAtCallback = set.BannedID(rec.Target.Value, time.Now())
	}}
	if err := sink.apply(moderation.Record{
		Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !bannedAtCallback {
		t.Error("the actuation callback ran before the ban was evaluable — a killed broadcaster could reclaim its ID")
	}
}

// A nil callback is the shape a relay wired for gate-only enforcement has,
// and every source must tolerate it.
func TestNilCallbackIsSafe(t *testing.T) {
	set := moderation.NewSet()
	sink := Sink{Set: set}
	rec := moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}}
	if err := sink.apply(rec); err != nil {
		t.Fatalf("apply with a nil callback: %v", err)
	}
	sink.replace([]moderation.Record{rec})
	if _, ok := set.BannedID("ABC23Z", time.Now()); !ok {
		t.Error("the Set was not updated")
	}
}
