// Package pwtest starts a private, headless PipeWire daemon for integration
// tests (R35, docs/39 D7).
//
// Unlike the portal/GPU half of this module — structurally invisible to CI, and
// the standing reason docs/19 leans on on-hardware verification — the audio
// control plane runs perfectly well on a runner with no sound card: a PipeWire
// daemon, a WirePlumber session manager and a null sink are enough to reproduce
// the shapes that matter (an application playing, its stream dying and coming
// back, a second stream, a kill -9). So the tests here drive reality rather
// than a fake, and the mechanism they prove is the mechanism that ships.
//
// Everything is per-test: its own XDG_RUNTIME_DIR, its own D-Bus session, its
// own daemon. Nothing touches the developer's own sound server, which matters
// because these tests create sinks and links for a living.
package pwtest

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log sink written by an exec.Cmd's copier goroutine and read
// by a t.Cleanup closure on the test goroutine.
//
// A bare bytes.Buffer here is a data race, and `-race` in CI is what caught it:
// exec.Cmd copies a child's output on its own goroutine, which is still running
// when a failing test reads the buffer to print it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// Daemon is a running private PipeWire instance.
type Daemon struct {
	t *testing.T
	// Env is what a child process needs to talk to this daemon and nothing
	// else. Pass it to exec.Cmd.Env.
	Env []string

	runtimeDir string
	procs      []*exec.Cmd
	// wpLog is the session manager's output, kept so a skip can explain
	// itself.
	wpLog *syncBuffer
}

// Binaries the harness needs. A machine without them skips rather than fails:
// these tests are a bonus on a developer laptop and a requirement in CI, and
// the CI job installs them explicitly.
var required = []string{"pipewire", "wireplumber", "pw-cli", "pw-dump", "pw-link", "dbus-daemon"}

// Start brings up a daemon, a session manager and a stereo null sink standing
// in for the machine's speakers. It registers its own cleanup.
//
// It skips the test when the tools are missing, and when the environment
// cannot run them (a container without /dev/shm, say) — a skipped audio test
// on a developer's machine is a nuisance, but a red one that means "your distro
// is different" is worse.
func Start(t *testing.T) *Daemon {
	t.Helper()
	return StartWithSpeakers(t, "FL,FR")
}

// StartWithSpeakers is Start with an explicit channel layout for the stand-in
// speakers.
//
// The layout is not cosmetic: an application's *stream node* is an adapter, and
// its output ports speak whatever layout it negotiated toward the default sink.
// So a surround machine is modelled by surround speakers, not by a surround
// application — measured 2026-08-01, and the reason docs/39 D3 talks about the
// default sink's layout in the first place.
func StartWithSpeakers(t *testing.T, positions string) *Daemon {
	t.Helper()
	for _, bin := range required {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("pwtest: %s is not installed; skipping the PipeWire integration test", bin)
		}
	}

	// A short path: the runtime dir holds a unix socket, and those are capped
	// near 108 bytes. t.TempDir()'s name is long enough to matter.
	dir, err := os.MkdirTemp("", "pwt")
	if err != nil {
		t.Fatalf("pwtest: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("pwtest: %v", err)
	}

	d := &Daemon{t: t, runtimeDir: dir}
	t.Cleanup(d.stop)

	// WirePlumber needs a session bus (it talks to logind/reserve-device), and
	// without one it exits immediately — the first thing that bit this harness.
	busAddr := d.startDBus()
	d.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+dir,
		"DBUS_SESSION_BUS_ADDRESS="+busAddr,
		"PIPEWIRE_RUNTIME_DIR="+dir,
		// Quiet: a failing test prints the logs it captured, and a passing one
		// should print nothing.
		"PIPEWIRE_DEBUG=0",
	)

	d.spawn("pipewire")
	d.waitForDaemon()
	// WirePlumber runs against a config of our own making — see
	// minimalWirePlumberConfig for why the stock one cannot be relied on.
	if dir := d.minimalWirePlumberConfig(); dir != "" {
		d.Env = append(d.Env, "WIREPLUMBER_CONFIG_DIR="+dir)
	}
	d.wpLog = d.spawnLog("wireplumber")

	// Stand-in speakers. Every machine this feature runs on has a real sink;
	// a runner has none, and an application with nowhere to play never opens
	// ports at all — which would make the whole graph untestable.
	d.CreateNullSink("speakers", "Audio/Sink", positions)
	d.waitFor("the speakers sink", func() bool {
		_, ok := d.FindNode(func(n Node) bool { return n.Name == "speakers" })
		return ok
	})
	// And then for the session manager to have *adopted* it as the default.
	// Waiting only for the node is not enough, and the difference is not
	// theoretical: an emitter started in that window dies with "stream error:
	// no target node available", which reads like a bug in the code under test
	// rather than a race in the harness.
	//
	// A **skip**, not a failure, when it never happens: this is precisely the
	// "the environment cannot run them" case Start promises to skip on, and a
	// session manager that will not start says nothing about the code under
	// test. The message names what was missing so a silently-skipped CI job is
	// diagnosable from the log rather than from a guess.
	if !d.waitUntil(10*time.Second, d.hasDefaultSink) {
		d.t.Skipf("pwtest: wireplumber never published a default sink in this environment "+
			"(no session manager ⇒ no routing ⇒ no ports on an application's stream). "+
			"wireplumber said:\n%s", d.sessionManagerLog())
	}
	return d
}

// minimalWirePlumberConfig writes a config directory that loads the session
// manager's *linking* half and nothing else, returning "" if it cannot.
//
// Not tidiness — necessity. The stock configuration enables the ALSA, V4L2,
// libcamera and Bluetooth monitors, and the Bluetooth half pulls in the logind
// plugin. On a container runner with no `/run/systemd` and no system bus that
// path fails hard enough to take WirePlumber down ("failed to start systemd
// logind monitor: -2", then "disconnected from pipewire"), and with no session
// manager nothing routes, so an application's stream never negotiates ports and
// the whole graph is untestable. A developer's machine has logind and never
// sees it; CI has neither, which is exactly the environment this harness exists
// to work in.
//
// None of the dropped monitors could have contributed anything: the tests'
// devices are null sinks the harness creates itself.
func (d *Daemon) minimalWirePlumberConfig() string {
	const stock = "/usr/share/wireplumber"
	src := filepath.Join(stock, "main.lua.d")
	entries, err := os.ReadDir(src)
	if err != nil {
		return "" // an unexpected layout (0.5+, another distro): use the stock config
	}

	dir := filepath.Join(d.runtimeDir, "wireplumber")
	luaDir := filepath.Join(dir, "main.lua.d")
	if err := os.MkdirAll(luaDir, 0o755); err != nil {
		return ""
	}
	// main.conf is copied verbatim: it configures the PipeWire context, not
	// the monitors, and rewriting it would be inventing a second thing to keep
	// in sync with the distro.
	if err := copyFile(filepath.Join(stock, "main.conf"), filepath.Join(dir, "main.conf")); err != nil {
		return ""
	}
	// Everything from main.lua.d except the enable-all script, which is the
	// one that turns the monitors on.
	sawFunctions := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".lua") || name == "90-enable-all.lua" {
			continue
		}
		if name == "00-functions.lua" {
			sawFunctions = true
		}
		if err := copyFile(filepath.Join(src, name), filepath.Join(luaDir, name)); err != nil {
			return ""
		}
	}
	if !sawFunctions {
		return "" // not the layout we understand; better the stock config than a broken one
	}
	// Our replacement: metadata (so `default.audio.sink` exists at all),
	// the access policy (so clients may do anything), and device defaults +
	// the linking policy (so a stream is routed to the default sink). No
	// hardware monitors, no Bluetooth, no logind.
	enable := `-- Written by internal/pwtest: the session manager's linking half only.
load_module("metadata")
default_access.enable()
device_defaults.enable()
stream_defaults.enable()
load_script("suspend-node.lua")
`
	if err := os.WriteFile(filepath.Join(luaDir, "90-enable-linking.lua"), []byte(enable), 0o644); err != nil {
		return ""
	}
	// policy.conf and policy.lua.d carry the linking policy itself; copied as
	// they are.
	for _, name := range []string{"policy.conf"} {
		if err := copyFile(filepath.Join(stock, name), filepath.Join(dir, name)); err != nil {
			return ""
		}
	}
	if err := copyDir(filepath.Join(stock, "policy.lua.d"), filepath.Join(dir, "policy.lua.d")); err != nil {
		return ""
	}
	// wireplumber.conf lists the config "profiles" to load, and one of them is
	// bluetooth.lua — the path to the logind plugin this whole function exists
	// to avoid. Drop that line and keep the rest verbatim.
	conf, err := os.ReadFile(filepath.Join(stock, "wireplumber.conf"))
	if err != nil {
		return ""
	}
	var kept []string
	dropped := false
	for _, line := range strings.Split(string(conf), "\n") {
		if strings.Contains(line, "bluetooth.lua") && strings.Contains(line, "config/lua") {
			dropped = true
			continue
		}
		kept = append(kept, line)
	}
	if !dropped {
		// A layout we do not recognise. The stock config is a better bet than
		// a config we have edited blind.
		return ""
	}
	if err := os.WriteFile(filepath.Join(dir, "wireplumber.conf"), []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		return ""
	}
	return dir
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) startDBus() string {
	d.t.Helper()
	out, err := exec.Command("dbus-daemon", "--session", "--print-address=1",
		"--print-pid=1", "--fork").Output()
	if err != nil {
		d.t.Skipf("pwtest: cannot start a session bus: %v", err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) < 2 {
		d.t.Skipf("pwtest: dbus-daemon said %q", out)
	}
	addr, pid := lines[0], lines[1]
	d.t.Cleanup(func() {
		_ = exec.Command("kill", pid).Run()
	})
	return addr
}

func (d *Daemon) spawn(name string, args ...string) *exec.Cmd {
	d.t.Helper()
	cmd, _ := d.spawnWithLog(name, args...)
	return cmd
}

// spawnLog is spawn, handing back the captured output.
func (d *Daemon) spawnLog(name string, args ...string) *syncBuffer {
	d.t.Helper()
	_, log := d.spawnWithLog(name, args...)
	return log
}

func (d *Daemon) spawnWithLog(name string, args ...string) (*exec.Cmd, *syncBuffer) {
	d.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = d.Env
	var log syncBuffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		d.t.Fatalf("pwtest: starting %s: %v", name, err)
	}
	d.procs = append(d.procs, cmd)
	d.t.Cleanup(func() {
		if d.t.Failed() && log.Len() > 0 {
			d.t.Logf("pwtest: %s log:\n%s", name, log.String())
		}
	})
	return cmd, &log
}

func (d *Daemon) waitForDaemon() {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(d.runtimeDir, "pipewire-0")); err == nil {
			// The socket exists; make sure it answers.
			if err := d.run("pw-cli", "info", "0"); err == nil {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Skipf("pwtest: the PipeWire daemon never came up in this environment")
}

// hasDefaultSink reports whether the session manager has published a default
// audio sink, which is the thing an application's autoconnect actually needs.
func (d *Daemon) hasDefaultSink() bool {
	out, err := d.Command("pw-metadata", "-n", "default").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "default.audio.sink")
}

func (d *Daemon) stop() {
	for i := len(d.procs) - 1; i >= 0; i-- {
		p := d.procs[i]
		if p.Process != nil {
			_ = p.Process.Kill()
			_ = p.Wait()
		}
	}
	_ = os.RemoveAll(d.runtimeDir)
}

// Command builds a command wired to this daemon.
func (d *Daemon) Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = d.Env
	return cmd
}

func (d *Daemon) run(name string, args ...string) error {
	return d.Command(name, args...).Run()
}

// CreateNullSink adds a sink to the graph. object.linger keeps it alive after
// the creating pw-cli exits — the opposite of what the helper does, and
// deliberately so: this one stands in for hardware.
func (d *Daemon) CreateNullSink(name, class, positions string) {
	d.t.Helper()
	spec := fmt.Sprintf(
		`{ factory.name=support.null-audio-sink node.name=%s node.description=%s `+
			`media.class=%s object.linger=true audio.position=[%s] monitor.channel-volumes=true }`,
		name, name, class, positions)
	cmd := d.Command("pw-cli", "-m", "create-node", "adapter", spec)
	if err := cmd.Start(); err != nil {
		d.t.Fatalf("pwtest: creating null sink %s: %v", name, err)
	}
	// -m keeps pw-cli alive (monitoring); the object lingers regardless, and
	// killing the process is how the sink stops being *owned* by it.
	go func() { _ = cmd.Wait() }()
	d.t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
}

// Emitter is a synthetic application playing a tone — CI's stand-in for a game.
type Emitter struct {
	t    *testing.T
	cmd  *exec.Cmd
	log  *syncBuffer
	Name string
}

// countStreams counts the audio-output streams a binary currently has in the
// graph, which is how the harness knows an emitter has really arrived.
func (d *Daemon) countStreams(binary string) int {
	n := 0
	for _, node := range d.Nodes() {
		if node.MediaClass == "Stream/Output/Audio" && node.Name == binary {
			n++
		}
	}
	return n
}

// StartEmitter plays a tone into the default sink from a process named binary.
//
// The name matters: `application.process.binary` is the identity the helper
// links against (AD2), and it comes from the *executable's* name — so the
// emitter is a copy of gst-launch-1.0 under the name the test wants, which is
// as close to a real application as a runner can get.
func (d *Daemon) StartEmitter(binary string, freq int) *Emitter {
	return d.StartEmitterChannels(binary, freq, 2)
}

// StartEmitterChannels is StartEmitter with an explicit channel count, for the
// case docs/39 D3 exists to prevent: a surround application whose centre
// channel a stereo capture sink would silently drop.
func (d *Daemon) StartEmitterChannels(binary string, freq, channels int) *Emitter {
	d.t.Helper()
	src, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		d.t.Skipf("pwtest: gst-launch-1.0 is not installed; skipping")
	}
	fake := filepath.Join(d.runtimeDir, binary)
	if _, err := os.Stat(fake); err != nil {
		data, err := os.ReadFile(src)
		if err != nil {
			d.t.Fatalf("pwtest: %v", err)
		}
		if err := os.WriteFile(fake, data, 0o755); err != nil {
			d.t.Fatalf("pwtest: %v", err)
		}
	}
	before := d.countStreams(binary)
	cmd := d.Command(fake, "-q",
		"audiotestsrc", "is-live=true", fmt.Sprintf("freq=%d", freq),
		"!", "audioconvert", "!", fmt.Sprintf("audio/x-raw,channels=%d", channels), "!", "pipewiresink")
	var log syncBuffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		d.t.Fatalf("pwtest: starting emitter %s: %v", binary, err)
	}
	e := &Emitter{t: d.t, cmd: cmd, Name: binary, log: &log}
	d.t.Cleanup(func() {
		e.Kill()
		if d.t.Failed() && log.Len() > 0 {
			d.t.Logf("pwtest: emitter %s log:\n%s", binary, log.String())
		}
	})
	// Wait until it is actually in the graph. An emitter that failed to start
	// must fail *here*, naming itself, rather than downstream as an assertion
	// about links that were never going to appear.
	d.waitFor("the emitter "+binary+" to reach the graph", func() bool {
		return d.countStreams(binary) > before
	})
	return e
}

// Kill stops the emitter the way an application crashing does.
func (e *Emitter) Kill() {
	if e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
		_ = e.cmd.Wait()
	}
}

// Log returns whatever the emitter printed, for a test that wants to explain
// itself.
func (e *Emitter) Log() string { return e.log.String() }

// BuildHelper builds the real gawk-pw-helper and returns its path, so a test
// outside cmd/gawk-pw-helper can drive the shipping binary rather than a
// stand-in. Built once per process.
func BuildHelper(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gawk-pw-helper")
		if err != nil {
			helperErr = err
			return
		}
		helperPath = filepath.Join(dir, "gawk-pw-helper")
		out, err := exec.Command("go", "build", "-o", helperPath,
			"github.com/Tuhis/gawk/gawk-broadcast/cmd/gawk-pw-helper").CombinedOutput()
		if err != nil {
			helperErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if helperErr != nil {
		t.Fatalf("pwtest: building the helper: %v", helperErr)
	}
	return helperPath
}

var (
	helperOnce sync.Once
	helperPath string
	helperErr  error
)

// Node is the slice of a pw-dump node this package's assertions need.
type Node struct {
	ID         uint32
	Serial     uint32
	Name       string
	MediaClass string
}

// Link is the slice of a pw-dump link the assertions need.
type Link struct {
	ID                 uint32
	OutputNode, InNode uint32
	OutputPort, InPort uint32
}

type dumpObject struct {
	ID   uint32 `json:"id"`
	Type string `json:"type"`
	Info struct {
		Props        map[string]any `json:"props"`
		OutputNodeID uint32         `json:"output-node-id"`
		OutputPortID uint32         `json:"output-port-id"`
		InputNodeID  uint32         `json:"input-node-id"`
		InputPortID  uint32         `json:"input-port-id"`
	} `json:"info"`
}

// Dump runs pw-dump and decodes it.
func (d *Daemon) Dump() []dumpObject {
	d.t.Helper()
	out, err := d.Command("pw-dump").Output()
	if err != nil {
		d.t.Fatalf("pwtest: pw-dump: %v", err)
	}
	objs, err := decodeDump(out)
	if err != nil {
		d.t.Fatalf("pwtest: decoding pw-dump: %v\noutput was:\n%s", err, out)
	}
	return objs
}

// decodeDump reads pw-dump's output as a STREAM of arrays, not one.
//
// pw-dump prints its snapshot and then, when registry events arrive before it
// exits, prints a second array after it — so a single json.Unmarshal fails on
// the trailing bytes with "invalid character '[' after top-level value", which
// is a harness failure wearing an unrelated test's name.
//
// The later array is the newer state of objects the snapshot already listed, so
// the batches are folded by id with the last write winning rather than
// concatenated: two entries for one id would make FindNode answer with the
// stale one and LinksInto count the same link twice.
func decodeDump(out []byte) ([]dumpObject, error) {
	var objs []dumpObject
	at := map[uint32]int{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var batch []dumpObject
		if err := dec.Decode(&batch); errors.Is(err, io.EOF) {
			return objs, nil
		} else if err != nil {
			return nil, err
		}
		for _, o := range batch {
			if i, seen := at[o.ID]; seen {
				objs[i] = o
				continue
			}
			at[o.ID] = len(objs)
			objs = append(objs, o)
		}
	}
}

// Nodes lists the graph's nodes.
func (d *Daemon) Nodes() []Node {
	var out []Node
	for _, o := range d.Dump() {
		if !strings.HasSuffix(o.Type, "Node") {
			continue
		}
		out = append(out, Node{
			ID:         o.ID,
			Serial:     propU32(o.Info.Props, "object.serial"),
			Name:       propStr(o.Info.Props, "node.name"),
			MediaClass: propStr(o.Info.Props, "media.class"),
		})
	}
	return out
}

// Links lists the graph's links.
func (d *Daemon) Links() []Link {
	var out []Link
	for _, o := range d.Dump() {
		if !strings.HasSuffix(o.Type, "Link") {
			continue
		}
		out = append(out, Link{
			ID:         o.ID,
			OutputNode: o.Info.OutputNodeID,
			OutputPort: o.Info.OutputPortID,
			InNode:     o.Info.InputNodeID,
			InPort:     o.Info.InputPortID,
		})
	}
	return out
}

// FindNode returns the first node matching pred.
func (d *Daemon) FindNode(pred func(Node) bool) (Node, bool) {
	for _, n := range d.Nodes() {
		if pred(n) {
			return n, true
		}
	}
	return Node{}, false
}

// LinksInto counts the links terminating on a node.
func (d *Daemon) LinksInto(nodeID uint32) int {
	n := 0
	for _, l := range d.Links() {
		if l.InNode == nodeID {
			n++
		}
	}
	return n
}

// AssertNoGawkObjects is AG5's assertion: after every kill path, the sound
// server must contain nothing of ours. It looks for the name prefix the helper
// stamps on its sink, which is the only trace it could possibly leave.
func (d *Daemon) AssertNoGawkObjects() {
	d.t.Helper()
	for _, n := range d.Nodes() {
		if strings.HasPrefix(n.Name, "gawk-app-capture") {
			d.t.Errorf("pwtest: a gawk sink survived: %+v", n)
		}
	}
}

// WaitFor polls until pred is true, and fails the test if it never is. Polling
// rather than sleeping keeps the tests fast when things work and honest when
// they do not.
func (d *Daemon) WaitFor(what string, pred func() bool) {
	d.t.Helper()
	d.waitFor(what, pred)
}

func (d *Daemon) waitFor(what string, pred func() bool) {
	d.t.Helper()
	if !d.waitUntil(10*time.Second, pred) {
		d.t.Fatalf("pwtest: timed out waiting for %s", what)
	}
}

// waitUntil is waitFor without a verdict, for the caller that wants to skip
// rather than fail.
func (d *Daemon) waitUntil(limit time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// sessionManagerLog returns what wireplumber printed, for a skip message that
// says why rather than just that.
func (d *Daemon) sessionManagerLog() string {
	if d.wpLog == nil {
		return "(nothing captured)"
	}
	return d.wpLog.String()
}

// Capture records the monitor of a node and reports the peak sample seen, so a
// test can distinguish "linked" from "actually carrying audio" — which is the
// difference between a graph that looks right and one that works.
func (d *Daemon) Capture(serial uint32, dur time.Duration) float64 {
	d.t.Helper()
	// A .wav, because pw-record picks its output format from the extension and
	// has no raw mode. The header is skipped below.
	path := filepath.Join(d.runtimeDir, fmt.Sprintf("cap-%d.wav", serial))
	var log syncBuffer
	cmd := d.Command("pw-record", "--target", fmt.Sprint(serial),
		"-P", "{ stream.capture.sink=true }",
		"--format", "s16", "--rate", "48000", "--channels", "2",
		path)
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		d.t.Skipf("pwtest: pw-record is unavailable: %v", err)
	}
	time.Sleep(dur)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		d.t.Fatalf("pwtest: reading capture: %v (pw-record said: %s)", err, log.String())
	}
	data = wavSamples(data)
	peak := 0
	for i := 0; i+1 < len(data); i += 2 {
		v := int(int16(uint16(data[i]) | uint16(data[i+1])<<8))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return float64(peak) / 32768.0
}

// wavSamples returns the payload of a RIFF/WAVE file by walking its chunks —
// the header is not always 44 bytes, and guessing produces a "peak" computed
// partly from ASCII chunk names.
func wavSamples(b []byte) []byte {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return b
	}
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if id == "data" {
			end := body + size
			if end > len(b) || size == 0 {
				end = len(b)
			}
			return b[body:end]
		}
		off = body + size
		if size%2 == 1 {
			off++ // chunks are word-aligned
		}
	}
	return nil
}

func propStr(props map[string]any, key string) string {
	if v, ok := props[key].(string); ok {
		return v
	}
	return ""
}

func propU32(props map[string]any, key string) uint32 {
	switch v := props[key].(type) {
	case float64:
		return uint32(v)
	case string:
		var n uint32
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n
	}
	return 0
}
