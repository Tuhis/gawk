package moderationsrc

import (
	"context"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

func TestParse(t *testing.T) {
	valid := []struct {
		in       string
		wantKind Kind
		wantPath string
	}{
		{"", KindOff, ""}, // an explicitly-empty env var reads as off, not as broken
		{"off", KindOff, ""},
		{"OFF", KindOff, ""},
		{"  off  ", KindOff, ""},
		{"k8s", KindK8s, ""},
		{"K8s", KindK8s, ""},
		{"file:/etc/gawk/bans.json", KindFile, "/etc/gawk/bans.json"},
		{"FILE:/etc/gawk/bans.json", KindFile, "/etc/gawk/bans.json"},
		{"file: /tmp/bans.json ", KindFile, "/tmp/bans.json"},
		{"file:relative/bans.json", KindFile, "relative/bans.json"},
	}
	for _, tt := range valid {
		kind, path, err := Parse(tt.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.in, err)
			continue
		}
		if kind != tt.wantKind || path != tt.wantPath {
			t.Errorf("Parse(%q) = %q/%q, want %q/%q", tt.in, kind, path, tt.wantKind, tt.wantPath)
		}
	}

	for _, in := range []string{"postgres", "file", "file:", "file:  ", "k8s:default", "off:", "configmap:bans"} {
		if _, _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", in)
		}
	}
}

// "off" constructs nothing at all: no goroutine, no watcher, no file handle —
// and the Set it was handed stays exactly as empty as it started.
func TestStartOffConstructsNothing(t *testing.T) {
	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, source := range []string{"", "off"} {
		if err := Start(ctx, Options{Source: source, Set: set, Log: discardLog}); err != nil {
			t.Fatalf("Start(%q): %v", source, err)
		}
	}
	if got := set.ActiveCounts(time.Now()); got["broadcastId"] != 0 || got["ip"] != 0 {
		t.Errorf("ActiveCounts = %v, want empty", got)
	}
}

func TestStartRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	if err := Start(ctx, Options{Source: "nonsense", Set: moderation.NewSet(), Log: discardLog}); err == nil {
		t.Error("Start accepted an unparseable source")
	}
	if err := Start(ctx, Options{Source: "off", Log: discardLog}); err == nil {
		t.Error("Start accepted a nil Set")
	}
	if err := Start(ctx, Options{Source: "off", Set: moderation.NewSet()}); err == nil {
		t.Error("Start accepted a nil Log")
	}
}

// The k8s source needs a namespace and an in-cluster config; failing to
// construct it is a STARTUP error, so a misconfigured relay never comes up
// quietly enforcing nothing. (Losing contact with an already-built informer
// is a different thing entirely — that one only warns, docs/42 §6.)
func TestStartK8sOutsideAClusterFailsStartup(t *testing.T) {
	// Blanked explicitly: gawk's own CI runners are pods, where these would
	// otherwise be set and rest.InClusterConfig would succeed.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("POD_NAMESPACE", "")

	ctx := context.Background()
	err := Start(ctx, Options{Source: "k8s", Namespace: "production",
		Set: moderation.NewSet(), Log: discardLog})
	if err == nil {
		t.Fatal("Start(k8s) succeeded outside a cluster")
	}

	// ...and with no namespace to watch, the failure names that instead.
	err = Start(ctx, Options{Source: "k8s", Set: moderation.NewSet(), Log: discardLog})
	if err == nil {
		t.Fatal("Start(k8s) succeeded with no POD_NAMESPACE")
	}
}
