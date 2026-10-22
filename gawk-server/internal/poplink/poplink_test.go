package poplink

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// Every record type round-trips through Write/Read as the pointer Read
// returns, with its "t" set.
func TestEveryRecordRoundTrips(t *testing.T) {
	until := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	recs := []any{
		&Hello{Version: "0.44.0", Proto: ProtoMajor, LanAddr: "192.168.1.20", LanCandidates: []string{"192.168.1.20", "10.0.0.5"},
			PinnedHashes: []string{"aa", "bb"}, NamedHost: "p-7k2m9x.pop.gawk.ioio.fi",
			Health: []adminapi.HealthLine{{Key: "firewall", OK: true}}},
		&Reserve{IDs: []string{"ABCDEF", "GHJKMN"}},
		&Release{IDs: []string{"ABCDEF"}},
		&State{ID: "ABCDEF", Live: true, Label: "Tuhis", PublisherLanAddr: "192.168.1.30"},
		&Wall{Op: WallAttach, ID: "ABCDEF", Label: "Tuhis"},
		&Refuse{ID: "ABCDEF", Reason: RefuseBudget},
		&CSR{PEM: "-----BEGIN CERTIFICATE REQUEST-----"},
		&Stats{UplinkMbps: 12.5, UplinkStreams: 2, Uplink: pops.Budget{MaxMbps: 25, MaxStreams: 3},
			Downlink: pops.Budget{MaxMbps: 100}, DownlinkStreams: 1, GuestsConnected: 4,
			Broadcasts: []adminapi.PopBroadcast{{ID: "ABCDEF", LocalViewers: 3}}},
		&Health{Lines: []adminapi.HealthLine{{Key: "lan", OK: false, Detail: "no address"}}},
		&Credential{Credential: "c2VjcmV0"},
		&Config{PopName: "assembly-2026", UID: "0b8c6f4e-1d2a-4c3b-9e8f-7a6b5c4d3e2f", DisplayName: "Assembly 2026",
			HomeOrigin: "https://gawk.ioio.fi", WallCode: "assembly-2026", NamedHost: "p-7k2m9x.pop.gawk.ioio.fi",
			Uplink: pops.Budget{MaxMbps: 25, MaxStreams: 3}, MaxBroadcasts: 40, AutoSwitch: true,
			ScreeningNotice: true, HomeRooms: true, NotAfter: &until},
		&Reserved{ID: "ABCDEF", Result: ReservedLimit},
		&Want{ID: "ABCDEF"},
		&Ban{ID: "ABCDEF", Until: &until},
		&Ban{ID: "GHJKMN"},
		&Certificate{ChainPEM: "-----BEGIN CERTIFICATE-----", ForCSRSHA256: "cc"},
		&Bye{Reason: ByeRepair},
		&RoomOpen{Path: "/room/assembly-2026", RawQuery: "p=1"},
	}
	for _, rec := range recs {
		var buf bytes.Buffer
		if err := Write(&buf, rec); err != nil {
			t.Fatalf("Write(%T): %v", rec, err)
		}
		if !strings.Contains(buf.String()[4:], `"t":"`+TypeOf(rec)+`"`) {
			t.Errorf("%T frame %q has no t=%q", rec, buf.String()[4:], TypeOf(rec))
		}
		got, err := Read(&buf)
		if err != nil {
			t.Fatalf("Read(%T): %v", rec, err)
		}
		if !reflect.DeepEqual(got, rec) {
			t.Errorf("round trip %T:\n got %#v\nwant %#v", rec, got, rec)
		}
	}
}

// A value (not a pointer) is accepted too.
func TestWriteAcceptsValues(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Want{ID: "ABCDEF"}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := got.(*Want); !ok || w.ID != "ABCDEF" {
		t.Fatalf("got %#v", got)
	}
}

// The schema is pinned: the field names a peer of another build reads.
func TestRecordJSONIsPinned(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, &Reserved{ID: "ABCDEF", Result: ReservedOK}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String()[4:], `{"t":"reserved","id":"ABCDEF","result":"ok"}`; got != want {
		t.Errorf("frame = %s, want %s", got, want)
	}
	buf.Reset()
	if err := Write(&buf, &Bye{}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String()[4:], `{"t":"bye"}`; got != want {
		t.Errorf("empty bye = %s, want %s", got, want)
	}
	if n := binary.BigEndian.Uint32(buf.Bytes()[:4]); int(n) != buf.Len()-4 {
		t.Errorf("length prefix %d, body %d", n, buf.Len()-4)
	}
}

func frame(body string) *bytes.Buffer {
	var b bytes.Buffer
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(body)))
	b.Write(h[:])
	b.WriteString(body)
	return &b
}

// Minor additions are additive: an unknown type decodes to (nil, nil) and
// the next record still reads; an unknown field is ignored.
func TestUnknownTypesAndFieldsAreIgnored(t *testing.T) {
	b := frame(`{"t":"future","x":1}`)
	b.Write(frame(`{"t":"want","id":"ABCDEF","newField":true}`).Bytes())
	rec, err := Read(b)
	if err != nil || rec != nil {
		t.Fatalf("unknown type: %#v, %v", rec, err)
	}
	rec, err = Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := rec.(*Want); !ok || w.ID != "ABCDEF" {
		t.Fatalf("got %#v", rec)
	}
	if _, err := Read(b); !errors.Is(err, io.EOF) {
		t.Errorf("end of stream = %v, want io.EOF", err)
	}
}

func TestOversizeAndMalformedRecordsFail(t *testing.T) {
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], MaxRecordBytes+1)
	if _, err := Read(bytes.NewReader(h[:])); !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("oversize read = %v", err)
	}
	if err := Write(io.Discard, &CSR{PEM: strings.Repeat("x", MaxRecordBytes)}); !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("oversize write = %v", err)
	}
	if _, err := Read(frame(`not json`)); err == nil {
		t.Error("malformed JSON read without error")
	}
	if _, err := Read(frame(`{"t":"want","id":7}`)); err == nil {
		t.Error("ill-typed field read without error")
	}
	if _, err := Read(bytes.NewReader([]byte{0, 0, 0, 9, '{'})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated body = %v", err)
	}
	if err := Write(io.Discard, struct{}{}); err == nil {
		t.Error("an unknown record type was written")
	}
}

func TestCheckProto(t *testing.T) {
	cases := []struct {
		q         string
		minor     int
		wantError bool
	}{
		{"proto=1", 0, false},
		{"proto=1.3", 3, false},
		{"", 0, true},
		{"proto=", 0, true},
		{"proto=2", 0, true},
		{"proto=0.9", 0, true},
		{"proto=one", 0, true},
		{"proto=1.x", 0, true},
	}
	for _, c := range cases {
		q, _ := url.ParseQuery(c.q)
		minor, err := CheckProto(q)
		if c.wantError {
			if !errors.Is(err, ErrProtoMajor) {
				t.Errorf("%q: err = %v, want ErrProtoMajor", c.q, err)
			}
			continue
		}
		if err != nil || minor != c.minor {
			t.Errorf("%q: %d, %v; want %d", c.q, minor, err, c.minor)
		}
	}
}

func TestBearer(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"Bearer  abc": "abc",
		"Basic abc":   "",
		"":            "",
		"Bearer":      "",
	} {
		if got := BearerToken(in); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}
