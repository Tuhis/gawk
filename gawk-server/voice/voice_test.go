package voice

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

var testSecret = []byte("0123456789abcdef-voice-secret")

func testTicket(t *testing.T) Ticket {
	t.Helper()
	return Ticket{
		Code: "devroom", Participant: 7, Nickname: "tuhis",
		Nonce:  [NonceSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Epoch:  42,
		Expiry: time.UnixMilli(1_800_000_030_000),
	}
}

func TestVoiceTicketVerifiesWithSharedSecret(t *testing.T) {
	want := testTicket(t)
	raw, err := MintTicket(testSecret, want)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > wire.MaxVoiceTicketLen {
		t.Fatalf("ticket is %d bytes", len(raw))
	}
	got, err := VerifyTicket(testSecret, raw, want.Expiry.Add(-time.Second))
	if err != nil {
		t.Fatalf("VerifyTicket: %v", err)
	}
	if !reflect.DeepEqual(got.Expiry.UnixMilli(), want.Expiry.UnixMilli()) {
		t.Fatalf("expiry %v, want %v", got.Expiry, want.Expiry)
	}
	got.Expiry, want.Expiry = time.Time{}, time.Time{}
	if got != want {
		t.Fatalf("VerifyTicket = %+v, want %+v", got, want)
	}
	// The longest legal fields still fit a VoiceHello.
	long := testTicket(t)
	long.Code = strings.Repeat("a", wire.MaxRoomCodeLen)
	long.Nickname = strings.Repeat("ä", wire.MaxRoomNicknameLen/2)
	if raw, err := MintTicket(testSecret, long); err != nil || len(raw) > wire.MaxVoiceTicketLen {
		t.Fatalf("longest ticket: %d bytes, %v", len(raw), err)
	}
}

func TestVoiceTicketRefusesForgeries(t *testing.T) {
	tk := testTicket(t)
	raw, _ := MintTicket(testSecret, tk)
	now := tk.Expiry.Add(-time.Second)
	if _, err := VerifyTicket([]byte("another-secret-of-16+"), raw, now); !errors.Is(err, ErrBadTicket) {
		t.Fatalf("wrong secret: %v", err)
	}
	for i := range raw {
		bad := append([]byte(nil), raw...)
		bad[i] ^= 0x01
		if _, err := VerifyTicket(testSecret, bad, now); !errors.Is(err, ErrBadTicket) {
			t.Fatalf("flipped byte %d: %v", i, err)
		}
	}
	for _, n := range []int{0, 1, len(raw) - 1} {
		if _, err := VerifyTicket(testSecret, raw[:n], now); !errors.Is(err, ErrBadTicket) {
			t.Fatalf("truncated to %d: %v", n, err)
		}
	}
	if _, err := VerifyTicket(testSecret, append(append([]byte(nil), raw...), 0), now); !errors.Is(err, ErrBadTicket) {
		t.Fatal("trailing byte verified")
	}
	if _, err := VerifyTicket([]byte("short"), raw, now); !errors.Is(err, ErrBadTicket) {
		t.Fatal("short secret verified")
	}
}

func TestVoiceTicketExpires(t *testing.T) {
	tk := testTicket(t)
	raw, _ := MintTicket(testSecret, tk)
	got, err := VerifyTicket(testSecret, raw, tk.Expiry)
	if !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("at expiry: %v", err)
	}
	if got.Code != "devroom" {
		t.Fatal("an expired ticket still reports what it was for (logging)")
	}
}

func TestMintTicketValidates(t *testing.T) {
	for name, mut := range map[string]func(*Ticket){
		"unnormalized code": func(t *Ticket) { t.Code = "DevRoom" },
		"empty code":        func(t *Ticket) { t.Code = "" },
		"long code":         func(t *Ticket) { t.Code = strings.Repeat("a", 33) },
		"long nickname":     func(t *Ticket) { t.Nickname = strings.Repeat("x", 33) },
		"bad nickname":      func(t *Ticket) { t.Nickname = "\xff" },
	} {
		tk := testTicket(t)
		mut(&tk)
		if _, err := MintTicket(testSecret, tk); !errors.Is(err, ErrBadTicket) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := MintTicket([]byte("short"), testTicket(t)); !errors.Is(err, ErrBadTicket) {
		t.Error("short secret minted")
	}
}

func TestNewNonceIsRandom(t *testing.T) {
	a, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewNonce()
	if a == b {
		t.Fatal("two nonces equal")
	}
}

// The temporary channel name must be the same on every pod for one code
// (D11): pinned by a golden value so a change to the derivation, which
// would rename every live temporary channel on the next rollout, fails here.
func TestChannelKeyIsStable(t *testing.T) {
	// Computed independently: base32(HMAC-SHA256(secret, "mumble-channel:abcdef"))[:6].
	const golden = "4EOGEN"
	if got := ChannelKey(testSecret, "abcdef"); got != golden {
		t.Fatalf("ChannelKey = %q, want %q", got, golden)
	}
	if ChannelKey(testSecret, "abcdef") == ChannelKey(testSecret, "abcdeg") {
		t.Fatal("two codes share a key")
	}
	if ChannelKey(testSecret, "abcdef") == ChannelKey([]byte("another-secret-of-16+"), "abcdef") {
		t.Fatal("the key does not depend on the secret")
	}
	if got := TempChannelName("", testSecret, "abcdef", "ABCDEF"); got != "gawk "+golden {
		t.Fatalf("default template = %q", got)
	}
	if got := TempChannelName("room {code} ({key})", testSecret, "abcdef", "ABCDEF"); got != "room ABCDEF ("+golden+")" {
		t.Fatalf("custom template = %q", got)
	}
	if strings.Contains(TempChannelName("", testSecret, "abcdef", "ABCDEF"), "ABCDEF") {
		t.Fatal("the default name contains the code")
	}
}

func TestChannelPath(t *testing.T) {
	for in, want := range map[string][]string{
		"":                 nil,
		"/":                nil,
		"Friends":          {"Friends"},
		"Friends/Gaming":   {"Friends", "Gaming"},
		" /Friends//Game/": {"Friends", "Game"},
	} {
		if got := ChannelPath(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ChannelPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinkMessagesRoundTrip(t *testing.T) {
	msgs := []Message{
		{Type: TypeObserve, Code: "devroom", DisplayCode: "DevRoom", Channel: "Friends/Gaming", Epoch: 3},
		{Type: TypeRoster, Available: true, Users: []User{{Session: 1, Name: "bob", Flags: wire.RoomVoiceUserSelfMute}}},
		{Type: TypeRoster, Reason: "murmur unreachable"},
		{Type: TypeRegister, Ticket: []byte{1, 2, 3}, Remote: "192.0.2.1:5000"},
		{Type: TypeAccepted},
		{Type: TypeJoined, Session: 9, Name: "tuhis [web]"},
		{Type: TypeState, Session: 9, Flags: wire.RoomVoiceUserMute},
		{Type: TypeClose, CloseCode: wire.CloseCodeVoiceRemoved, Reason: "removed"},
		{Type: TypeLeft, CloseCode: wire.CloseCodeVoiceUnavailable, Reason: "murmur full"},
	}
	var buf bytes.Buffer
	for _, m := range msgs {
		if err := WriteMessage(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range msgs {
		got, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ReadMessage = %+v, want %+v", got, want)
		}
	}
}

func TestReadMessageBounds(t *testing.T) {
	for name, raw := range map[string][]byte{
		"zero length": {0, 0, 0, 0},
		"oversize":    {0xff, 0xff, 0xff, 0xff},
		"not json":    append([]byte{0, 0, 0, 1}, 'x'),
		"no type":     append([]byte{0, 0, 0, 2}, '{', '}'),
	} {
		if _, err := ReadMessage(bytes.NewReader(raw)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	if err := WriteMessage(&bytes.Buffer{}, Message{Type: TypeRoster, Reason: strings.Repeat("x", MaxMessageSize)}); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversize write: %v", err)
	}
}
