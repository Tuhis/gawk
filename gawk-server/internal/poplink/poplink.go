package poplink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProtoMajor is the link protocol's major version, sent as ?proto= on every
// /pop/* dial. A home that does not speak it answers 426 (docs/22 D16);
// minor additions are additive (unknown records and fields are ignored).
const ProtoMajor = 1

// MaxRecordBytes caps one record's JSON body.
const MaxRecordBytes = 64 << 10

// Link timers (docs/76 item 10). The PoP's dialer sets them: one listener
// serves every route, so home cannot set per-route timers, and the
// effective idle timeout is the lower of the two ends.
const (
	LinkIdleTimeout = 15 * time.Second
	LinkKeepAlive   = 5 * time.Second
	// LeaseRenew is how often the link-home pod renews status.lease.
	LeaseRenew = 5 * time.Second
)

// CloseCodeLinkEnd closes a link home ends for good (repair, revoked,
// expired): wire.CloseCodeTerminatedByOperator, always after a Bye.
const CloseCodeLinkEnd = 4006

// Origin is the Origin header every PoP dial carries; home honours it only
// on /pop/* (transport's popOrigin, pinned equal by a test there).
const Origin = "gawk-relay://pop"

// ReasonHeader names a /pop/* refusal in the HTTP response, beside its
// status, so the console can tell refusals that share one apart (a used
// and an expired pairing string are both 410). Values: the Reason*
// constants and the Bye reasons.
const ReasonHeader = "Gawk-Pop-Reason"

// Refusal reasons a home sends in ReasonHeader (besides ByeRepair,
// ByeRevoked and ByeExpired for a link credential).
const (
	ReasonUnknown        = "unknown"
	ReasonUsed           = "used"
	ReasonPairingExpired = "pairing-expired"
	ReasonLimited        = "limited"
	ReasonProto          = "proto"
)

var (
	// ErrProtoMajor: a /pop/* dial without ?proto= or with another major.
	ErrProtoMajor = errors.New("poplink: unsupported protocol major")
	// ErrRecordTooLarge: a record over MaxRecordBytes.
	ErrRecordTooLarge = errors.New("poplink: record too large")
)

// Write frames one record: a u32 big-endian body length, then the record's
// JSON with its "t" set. rec is one of this package's record types (value
// or pointer).
func Write(w io.Writer, rec any) error {
	t := TypeOf(rec)
	if t == "" {
		return fmt.Errorf("poplink: %T is not a link record", rec)
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("poplink: encode %s: %w", t, err)
	}
	var buf bytes.Buffer
	buf.Grow(4 + len(body) + len(t) + 8)
	buf.Write([]byte{0, 0, 0, 0})
	buf.WriteString(`{"t":`)
	tb, _ := json.Marshal(t)
	buf.Write(tb)
	if len(body) > 2 {
		buf.WriteByte(',')
		buf.Write(body[1:])
	} else {
		buf.WriteByte('}')
	}
	n := buf.Len() - 4
	if n > MaxRecordBytes {
		return fmt.Errorf("%w: %s is %d bytes", ErrRecordTooLarge, t, n)
	}
	out := buf.Bytes()
	binary.BigEndian.PutUint32(out[:4], uint32(n))
	_, err = w.Write(out)
	return err
}

// Read reads one record and returns it as a pointer to its type (*Hello,
// *Config, ...). An unknown type returns (nil, nil): minor additions are
// ignored, and the stream stays in sync. A clean end of stream before a
// record is io.EOF.
func Read(r io.Reader) (any, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n > MaxRecordBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrRecordTooLarge, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("poplink: malformed record: %w", err)
	}
	mk, ok := factories[head.T]
	if !ok {
		return nil, nil
	}
	rec := mk()
	if err := json.Unmarshal(body, rec); err != nil {
		return nil, fmt.Errorf("poplink: malformed %s record: %w", head.T, err)
	}
	return rec, nil
}

// CheckProto reads ?proto=<major>[.<minor>] off a /pop/* dial. A missing
// value or another major is ErrProtoMajor (the handler answers 426).
func CheckProto(q url.Values) (minor int, err error) {
	v := q.Get("proto")
	majorS, minorS, hasMinor := strings.Cut(v, ".")
	major, err := strconv.Atoi(majorS)
	if err != nil || major != ProtoMajor {
		return 0, fmt.Errorf("%w: proto=%q, want %d", ErrProtoMajor, v, ProtoMajor)
	}
	if hasMinor {
		if minor, err = strconv.Atoi(minorS); err != nil || minor < 0 {
			return 0, fmt.Errorf("%w: proto=%q", ErrProtoMajor, v)
		}
	}
	return minor, nil
}

// ProtoQuery is the ?proto= value this build sends.
func ProtoQuery() string { return strconv.Itoa(ProtoMajor) }

// BearerToken extracts the credential from an Authorization header value
// ("" when it is not a bearer token).
func BearerToken(h string) string {
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// Session is a link (or leg) session as both ends see it: the slice of a
// webtransport.Session the link uses, so tests can fake it. The PoP opens
// the control stream; extra bidi streams carry proxied room control.
type Session interface {
	OpenStream(ctx context.Context) (io.ReadWriteCloser, error)
	AcceptStream(ctx context.Context) (io.ReadWriteCloser, error)
	CloseWithError(code uint32, msg string) error
	Context() context.Context
	RemoteAddr() net.Addr
	LocalAddr() net.Addr
}
