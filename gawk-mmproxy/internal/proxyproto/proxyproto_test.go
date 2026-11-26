package proxyproto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"reflect"
	"testing"
)

type vector struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
	Error  string `json:"error,omitempty"`
	Hex    string `json:"hex"`
}

type vectors struct {
	Encode []vector `json:"encode"`
	Decode []vector `json:"decode"`
	Refuse []vector `json:"refuse"`
}

func load(t *testing.T, path string) vectors {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func raw(t *testing.T, v vector) []byte {
	t.Helper()
	b, err := hex.DecodeString(v.Hex)
	if err != nil {
		t.Fatalf("%s: %v", v.Name, err)
	}
	return b
}

func TestDecodesTheSpecLayouts(t *testing.T) {
	v := load(t, "testdata/vectors.json")
	for _, c := range append(v.Encode, v.Decode...) {
		h, err := Read(bytes.NewReader(raw(t, c)))
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if want := netip.MustParseAddrPort(c.Source); h.Source != want {
			t.Errorf("%s: source %v, want %v", c.Name, h.Source, want)
		}
		if h.Source.Addr().Is4In6() {
			t.Errorf("%s: source %v is still v4-mapped", c.Name, h.Source)
		}
	}
}

var errByName = map[string]error{
	"not_v2": ErrNotV2, "version": ErrVersion, "command": ErrCommand,
	"family": ErrFamily, "length": ErrLength, "source": ErrSource,
}

func TestRefusesEverythingElse(t *testing.T) {
	v := load(t, "testdata/vectors.json")
	if len(v.Refuse) == 0 {
		t.Fatal("no refusal vectors")
	}
	for _, c := range v.Refuse {
		want, ok := errByName[c.Error]
		if !ok {
			t.Fatalf("%s: unknown error %q", c.Name, c.Error)
		}
		if _, err := Read(bytes.NewReader(raw(t, c))); !errors.Is(err, want) {
			t.Errorf("%s: err %v, want %v", c.Name, err, want)
		}
	}
}

// A short read is an I/O error, never a decoded header: the proxy sees it as
// a timeout or a closed peer.
func TestTruncatedHeaderIsAnIOError(t *testing.T) {
	v := load(t, "testdata/vectors.json")
	full := raw(t, v.Encode[0])
	for n := 0; n < len(full); n++ {
		if _, err := Read(bytes.NewReader(full[:n])); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%d of %d bytes: err %v, want EOF", n, len(full), err)
		}
	}
}

// Read must not consume a byte past the header: the ClientHello that
// follows belongs to murmur (docs/92 D3).
func TestNeverReadsPastTheHeader(t *testing.T) {
	v := load(t, "testdata/vectors.json")
	for _, c := range append(v.Encode, v.Decode...) {
		trailing := []byte("\x16\x03\x01 the ClientHello")
		r := bytes.NewReader(append(raw(t, c), trailing...))
		if _, err := Read(r); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		rest, _ := io.ReadAll(r)
		if !bytes.Equal(rest, trailing) {
			t.Errorf("%s: left %q after the header, want %q", c.Name, rest, trailing)
		}
	}
}

// The bridge restates the `encode` block in its own tests (it is a separate
// module and image). In the repository checkout the two copies must be
// identical; in an image build of this module alone the bridge's tree isn't
// there, and the check has nothing to compare.
func TestBridgeCopyOfTheEncodeVectorsIsIdentical(t *testing.T) {
	const bridgeCopy = "../../../gawk-mumble-bridge/internal/bridge/testdata/proxyv2-vectors.json"
	if _, err := os.Stat(bridgeCopy); errors.Is(err, os.ErrNotExist) {
		if os.Getenv("CI") != "" && os.Getenv("GAWK_MMPROXY_ISOLATED") == "" {
			t.Fatalf("%s is missing in a repository checkout", bridgeCopy)
		}
		t.Skip("the bridge's tree is not here")
	}
	ours, theirs := load(t, "testdata/vectors.json"), load(t, bridgeCopy)
	if !reflect.DeepEqual(ours.Encode, theirs.Encode) {
		t.Fatalf("gawk-mumble-bridge's encode vectors differ from gawk-mmproxy's:\n ours   %+v\n theirs %+v", ours.Encode, theirs.Encode)
	}
}

func TestBindable(t *testing.T) {
	for a, want := range map[string]bool{
		"203.0.113.7": true, "127.0.0.5": true, "::1": true, "2001:db8::7": true,
		"0.0.0.0": false, "::": false, "224.0.0.1": false, "ff02::1": false,
		"255.255.255.255": false, "::ffff:0.0.0.0": false,
	} {
		if got := Bindable(netip.MustParseAddr(a)); got != want {
			t.Errorf("Bindable(%s) = %v, want %v", a, got, want)
		}
	}
}

func FuzzRead(f *testing.F) {
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		f.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		f.Fatal(err)
	}
	for _, c := range append(append(v.Encode, v.Decode...), v.Refuse...) {
		h, _ := hex.DecodeString(c.Hex)
		f.Add(h)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		h, err := Read(bytes.NewReader(in))
		if err != nil {
			return
		}
		if !Bindable(h.Source.Addr()) || h.Source.Addr().Is4In6() {
			t.Fatalf("decoded an unbindable or mapped source %v", h.Source)
		}
	})
}
