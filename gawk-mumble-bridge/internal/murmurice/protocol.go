package murmurice

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
)

// Message types of the Ice protocol 1.0.
const (
	msgRequest      uint8 = 0
	msgBatchRequest uint8 = 1
	msgReply        uint8 = 2
	msgValidate     uint8 = 3
	msgClose        uint8 = 4
)

const (
	headerSize = 14
	// maxMessage bounds one message. getUsers with a few hundred users is
	// well under it; anything larger is not murmur talking.
	maxMessage = 8 << 20
)

// Operation modes.
const (
	modeNormal     uint8 = 0
	modeIdempotent uint8 = 2
)

// Reply statuses.
const (
	statusOK                    uint8 = 0
	statusUserException         uint8 = 1
	statusObjectNotExist        uint8 = 2
	statusFacetNotExist         uint8 = 3
	statusOperationNotExist     uint8 = 4
	statusUnknownLocalException uint8 = 5
	statusUnknownUserException  uint8 = 6
	statusUnknownException      uint8 = 7
)

var magic = [4]byte{'I', 'c', 'e', 'P'}

func appendHeader(b []byte, typ uint8) []byte {
	b = append(b, magic[:]...)
	// protocol 1.0, protocol encoding 1.0, type, no compression, size
	b = append(b, 1, 0, 1, 0, typ, 0)
	return binary.LittleEndian.AppendUint32(b, headerSize)
}

// finish writes the message's final size into its header.
func finish(b []byte) []byte {
	binary.LittleEndian.PutUint32(b[10:], uint32(len(b)))
	return b
}

func control(typ uint8) []byte { return appendHeader(nil, typ) }

// readMessage reads one message: its type and its body.
func readMessage(r io.Reader) (uint8, []byte, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if [4]byte(h[:4]) != magic {
		return 0, nil, protoErr("bad magic", nil)
	}
	if h[4] != 1 || h[6] != 1 {
		return 0, nil, protoErr(fmt.Sprintf("protocol %d.%d, encoding %d.%d", h[4], h[5], h[6], h[7]), nil)
	}
	if h[9] == 2 {
		return 0, nil, protoErr("compressed message, never offered", nil)
	}
	size := int(int32(binary.LittleEndian.Uint32(h[10:])))
	if size < headerSize || size > maxMessage {
		return 0, nil, protoErr(fmt.Sprintf("message size %d", size), nil)
	}
	typ := h[8]
	switch typ {
	case msgValidate, msgClose:
		if size != headerSize {
			return 0, nil, protoErr(fmt.Sprintf("message type %d with a body", typ), nil)
		}
	case msgRequest, msgBatchRequest, msgReply:
	default:
		return 0, nil, protoErr(fmt.Sprintf("message type %d", typ), nil)
	}
	body := make([]byte, size-headerSize)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

// request is one outgoing call.
type request struct {
	target    Identity
	operation string
	mode      uint8
	// reqCtx is the request context: murmur reads the secret from it.
	reqCtx map[string]string
	params []byte
}

func encodeRequest(id int32, r request) []byte {
	e := encoder{b: appendHeader(make([]byte, 0, 64+len(r.params)), msgRequest)}
	e.i32(id)
	e.identity(r.target)
	e.size(0) // facet
	e.str(r.operation)
	e.u8(r.mode)
	keys := slices.Sorted(maps.Keys(r.reqCtx))
	e.size(len(keys))
	for _, k := range keys {
		e.str(k)
		e.str(r.reqCtx[k])
	}
	e.encaps(r.params)
	return finish(e.b)
}

// incoming is a decoded request murmur sent to the callback endpoint.
type incoming struct {
	id        int32 // 0 for a oneway request
	target    Identity
	facet     []string
	operation string
	params    []byte
}

func decodeIncoming(body []byte) (incoming, error) {
	d := newDecoder(body)
	r := incoming{id: d.i32(), target: d.identity()}
	if n := d.size(); n > 1 {
		d.fail(fmt.Errorf("%d facets", n))
	} else if n == 1 {
		r.facet = []string{d.str()}
	}
	r.operation = d.str()
	d.u8() // mode
	for n := d.size(); n > 0 && d.err() == nil; n-- {
		d.skipStr() // request context key
		d.skipStr() // and value
	}
	r.params = d.encaps()
	if err := d.end(); err != nil {
		return incoming{}, protoErr("request", err)
	}
	return r, nil
}

// encodeReply builds a reply with status OK and an encapsulated result, or
// with a request-failed status for the target.
func encodeReply(r incoming, status uint8, result []byte) []byte {
	e := encoder{b: appendHeader(nil, msgReply)}
	e.i32(r.id)
	e.u8(status)
	switch status {
	case statusOK:
		e.encaps(result)
	default: // object, facet or operation does not exist
		e.identity(r.target)
		e.size(len(r.facet))
		for _, f := range r.facet {
			e.str(f)
		}
		e.str(r.operation)
	}
	return finish(e.b)
}

// Errors.
var (
	// ErrClosed is returned once the client is closed.
	ErrClosed = errors.New("murmurice: client closed")
	// ErrConnectionLost is a connection that broke before the reply came.
	ErrConnectionLost = errors.New("murmurice: connection lost")
	// ErrObjectNotExist, ErrOperationNotExist match a *RequestFailedError.
	ErrObjectNotExist    = errors.New("murmurice: object does not exist")
	ErrOperationNotExist = errors.New("murmurice: operation does not exist")
)

// Type IDs of the murmur exceptions the bridge tells apart. Only the most
// derived type ID is matched: 1.5 derives them from MurmurException, 1.6
// from ServerException (F9).
const (
	ExInvalidSecret   = "::MumbleServer::InvalidSecretException"
	ExInvalidCallback = "::MumbleServer::InvalidCallbackException"
	ExServerBooted    = "::MumbleServer::ServerBootedException"
	ExInvalidServer   = "::MumbleServer::InvalidServerException"
)

// UserException is a Slice exception murmur raised.
type UserException struct{ TypeID string }

func (e *UserException) Error() string { return "murmurice: " + e.TypeID }

// IsUserException reports whether err is the user exception typeID.
func IsUserException(err error, typeID string) bool {
	var ue *UserException
	return errors.As(err, &ue) && ue.TypeID == typeID
}

// RequestFailedError is a reply saying the object, facet or operation does
// not exist.
type RequestFailedError struct {
	Status    uint8
	Operation string
}

func (e *RequestFailedError) Error() string {
	switch e.Status {
	case statusObjectNotExist:
		return "murmurice: object does not exist: " + e.Operation
	case statusFacetNotExist:
		return "murmurice: facet does not exist: " + e.Operation
	}
	return "murmurice: operation does not exist: " + e.Operation
}

func (e *RequestFailedError) Is(target error) bool {
	return (target == ErrObjectNotExist && e.Status == statusObjectNotExist) ||
		(target == ErrOperationNotExist && e.Status == statusOperationNotExist)
}

// UnknownError is a failure murmur could not express as a Slice exception.
type UnknownError struct{ Reason string }

func (e *UnknownError) Error() string { return "murmurice: unknown exception: " + e.Reason }

// ProtocolError is data that is not the Ice protocol or not the layout the
// bridge expects (D7: a changed User struct fails loudly).
type ProtocolError struct {
	What string
	Err  error
}

func (e *ProtocolError) Error() string {
	if e.Err == nil {
		return "murmurice: protocol error: " + e.What
	}
	return "murmurice: protocol error: " + e.What + ": " + e.Err.Error()
}

func (e *ProtocolError) Unwrap() error { return e.Err }

func protoErr(what string, err error) *ProtocolError { return &ProtocolError{What: what, Err: err} }

// decodeReply turns a reply's status and payload into the result payload
// or the error it carries.
func decodeReply(status uint8, payload []byte) ([]byte, error) {
	d := newDecoder(payload)
	switch status {
	case statusOK:
		out := d.encaps()
		if err := d.end(); err != nil {
			return nil, protoErr("reply", err)
		}
		return out, nil
	case statusUserException:
		data := d.encaps()
		if err := d.end(); err != nil {
			return nil, protoErr("user exception", err)
		}
		// Every exception slice starts with a flags byte and its type ID.
		x := newDecoder(data)
		x.u8()
		typeID := x.str()
		if x.err() != nil || typeID == "" {
			return nil, protoErr("user exception without a type ID", x.err())
		}
		return nil, &UserException{TypeID: typeID}
	case statusObjectNotExist, statusFacetNotExist, statusOperationNotExist:
		d.identity()
		for n := d.size(); n > 0 && d.err() == nil; n-- {
			d.skipStr()
		}
		op := d.str()
		if err := d.end(); err != nil {
			return nil, protoErr("request-failed reply", err)
		}
		return nil, &RequestFailedError{Status: status, Operation: op}
	case statusUnknownLocalException, statusUnknownUserException, statusUnknownException:
		reason := d.str()
		if err := d.end(); err != nil {
			return nil, protoErr("unknown-exception reply", err)
		}
		return nil, &UnknownError{Reason: reason}
	}
	return nil, protoErr(fmt.Sprintf("reply status %d", status), nil)
}
