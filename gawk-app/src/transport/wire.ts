// TypeScript mirror of the relay's wire format (gawk-server/internal/wire).
// The Go package is the source of truth; the golden hex vectors in
// wire.test.ts are copied verbatim from wire_test.go and guarantee the two
// implementations stay byte-compatible. If a vector fails, the wire format
// changed — fix the drift, don't regenerate the vector.
//
// Every datagram starts with a 2-byte prefix: byte 0 protocol version, byte
// 1 message type. All multi-byte integers are big-endian (DataView default).
//
//   0x01 VideoChunk (20-byte header, then payload):
//     byte 2       uint8   flags        bit0 = keyframe
//     byte 3       uint8   reserved (0)
//     bytes 4-7    uint32  frameID
//     bytes 8-9    uint16  chunkIndex   0-based
//     bytes 10-11  uint16  chunkCount   total chunks in the frame (>= 1)
//     bytes 12-19  uint64  timestampUs
//
//   0x02 DecoderConfig:
//     byte 2       uint8   reserved (0)
//     byte 3       uint8   codecLen
//     bytes 4..    codecLen bytes of ASCII codec string, then extradata
//
//   0x05 TimeSync (exactly 18 bytes, R5 Q2 — docs/15):
//     bytes 2-9    uint64  clientTimeUs   sender's clock at send; echoed back
//     bytes 10-17  uint64  serverTimeUs   relay monotonic clock; 0 in requests
//
//   0x06 ClockMapping (exactly 10 bytes, R5 Q2 — docs/15):
//     bytes 2-9    int64   offsetUs       relayClockUs = timestampUs + offsetUs
//
// Parse functions never copy: returned payload/extradata are subarray views
// of the input. Callers that retain them past the datagram buffer's
// lifetime must copy.

export const WIRE_VERSION = 0x01;

export const TYPE_VIDEO_CHUNK = 0x01;
export const TYPE_DECODER_CONFIG = 0x02;
export const TYPE_BROADCAST_ANNOUNCE = 0x03;
// TypeStreamFrame (R8): never a datagram — the payload of one unidirectional
// stream carrying exactly one keyframe. See encode/parseStreamFrame below.
export const TYPE_STREAM_FRAME = 0x04;
// TimeSync (R5 Q2): client↔relay clock-sync ping/pong datagram. The client
// sends it with serverTimeUs = 0; the relay echoes clientTimeUs and fills its
// monotonic clock, giving the client an NTP-style offset + RTT sample.
export const TYPE_TIME_SYNC = 0x05;
// ClockMapping (R5 Q2): broadcaster→viewers, relayed + cached by the relay
// like the keyframe prime. Maps frame timestamps onto the relay clock so a
// viewer (with its own TimeSync offset) can compute absolute capture→render.
export const TYPE_CLOCK_MAPPING = 0x06;
// AudioFrame (R15, docs/20 Decision 2): broadcaster→viewers, one complete
// Opus packet per datagram — no chunking, no reassembly, no keyframes. seq is
// audio's own uint32 sequence space (same serial arithmetic as frameIds);
// timestampUs is on the broadcaster's performance.now() µs clock, the same
// clock video capture stamps, so A/V skew is a subtraction.
export const TYPE_AUDIO_FRAME = 0x07;
// AudioConfig (R15, docs/20): broadcaster→viewers, relayed + cached by the
// hub like ClockMapping (join-primed, invalidated with the other caches).
// Re-sent at 1 Hz by the broadcaster — audio has no keyframe to anchor
// config re-emits to; idempotent on the viewer.
export const TYPE_AUDIO_CONFIG = 0x08;
// ResumeToken (R17 W2, docs/22): server→publisher on its own uni stream right
// after the session upgrade. The broadcaster presents it (hex, as the
// `resume` query param) to claim /publish/{id} on any pod — auto-resume,
// manual reclaim, and relay restarts all ride it.
export const TYPE_RESUME_TOKEN = 0x09;
// ReliableCarrier (R19, docs/24 Decision 3): the stream-kind discriminator of
// a relay→subscriber uni stream carrying delta datagrams reliably to an
// opted-in resilient subscriber. Never a datagram. The stream opens with the
// two-byte prologue version‖type (a keyframe stream starts version‖0x04, so a
// bare length prefix would be ambiguous), then records of
// uint16 length (BE) ‖ verbatim datagram bytes — each record a complete
// datagram the relay would otherwise have sent unreliably.
export const TYPE_RELIABLE_CARRIER = 0x0a;
// ViewerCount (R18, docs/23 Decision 2): relay-originated only — the live
// "N watching" number pushed to viewers and the broadcaster (and, in cluster
// mode, an edge's local count reported up to its origin — a leg browsers
// never see). Clients parse it and never send it.
export const TYPE_VIEWER_COUNT = 0x0b;
// DeliveryAck (R21, docs/26 Decision 7a): relay→viewer, once at join. Says
// what this subscriber is ACTUALLY being served — delivery is negotiated by
// query param, and a DVR-replayed GOP is byte-identical on the wire to a live
// one, so without this the viewer cannot tell an honoured request from a
// downgrade or from a relay too old to know the parameter.
export const TYPE_DELIVERY_ACK = 0x0c;
// TelemetryHello (R28, docs/33 D2): relay→client, once per session on its own
// reliable uni stream. Carries this session's telemetry token, the OBFUSCATED
// broadcast key (so a client never reports a joinable ID), and whether the
// fleet collects telemetry at all. Clients parse it and never send it; a relay
// predating R28 sends nothing, which the client treats exactly like
// enabled: false.
export const TYPE_TELEMETRY_HELLO = 0x0d;
// ParityChunk (R29, docs/34): broadcaster→relay→viewers, one RAID-6 P/Q parity
// symbol over a delta frame's data chunks, so a live-edge viewer repairs chunk
// loss without R19's carrier latency. Deltas only — keyframes ride reliable uni
// streams already. The relay computes nothing: it forwards a per-subscriber
// PREFIX of the symbols the producer emitted. Encoding lives in parity.ts.
export const TYPE_PARITY_CHUNK = 0x0e;
// RelayCapabilities (R29, docs/34 §4.4): relay→client, once per session on both
// routes, naming the optional features this fleet supports and at what level.
// Its own message rather than extra BroadcastAnnounce fields because the
// parsers are strict and the WebTransport API exposes no response headers — so
// a producer that never sees it emits no parity, keeping a new broadcaster
// against an old relay byte-identical to pre-R29.
export const TYPE_RELAY_CAPABILITIES = 0x0f;
// StripeState (R30, docs/35 §5.3): client→relay, the one message a striping
// viewer sends on its primary subscribe session to suppress (or restore)
// delta datagrams there while stripe legs carry them. Level state, re-sent at
// 1 Hz while striped; the relay expires stale suppression so a lost message
// converges to duplicates, never holes. Encoding lives beside the other
// fixed-size messages below.
export const TYPE_STRIPE_STATE = 0x10;
// RelayIdentity (R37, docs/40 §4.4): relay→client on a uni stream at /echo
// session start — version + operator display name for the server picker's
// probe. Trailing bytes past the declared fields are TOLERATED (the managed-
// mode extension space, a documented deviation from house strictness); the
// flags byte stays strict. Clients parse it and never send it.
export const TYPE_RELAY_IDENTITY = 0x11;
// TelemetryEndpoint (R37, docs/40 §4.10): relay→client on its own uni stream
// on the media routes — the fleet's advertised telemetry ingest URL, which
// wins over the deployment's configured one (D15). Same trailing-bytes
// tolerance and strict flags as RelayIdentity; never sent by clients.
export const TYPE_TELEMETRY_ENDPOINT = 0x12;

export const CLOSE_CODE_BROADCAST_ENDED = 4000;
// The relay evicted this subscriber because its keyframe stream opens failed
// persistently (R10, docs/14 — typically a zombie session with exhausted
// stream credit). Non-terminal by design: the viewer's normal reconnect
// applies (a fresh session restores stream credit), so no special handling —
// mirrored from Go wire.CloseCodeSubscriberUnresponsive for namespace parity.
export const CLOSE_CODE_SUBSCRIBER_UNRESPONSIVE = 4001;
// The relay pod is shutting down for a planned rollout (R17 W1, docs/22):
// sent while the pod is still Ready, so it reliably reaches the peer.
// Non-terminal and explicitly fast — the client reconnects immediately
// (0 ms first retry); a ready replacement pod is behind the same Service.
export const CLOSE_CODE_SERVER_DRAINING = 4002;
// Internal edge sessions only (R17 W5): the origin lost its Lease and is
// demoting. Browsers never receive it — mirrored for namespace parity.
export const CLOSE_CODE_ORIGIN_MOVED = 4003;
// The relay deposed this publisher session because a newer session claimed
// its broadcast ID with a verified resume token (docs/06 revision
// 2026-07-18: newest publisher wins — the relay can't tell a silently-dead
// publisher from a live one inside the QUIC idle window, and 409ing the
// reclaim orphaned every viewer). In practice it lands on the broadcaster's
// own zombie session; a live session receiving it has been replaced and
// must not resume back. Mirrored from Go wire.CloseCodePublisherSuperseded.
export const CLOSE_CODE_PUBLISHER_SUPERSEDED = 4004;
// The relay reaped this R30 stripe leg as orphaned (docs/35 §14): the
// primary session sharing its ?owner= token ended, or the leg's liveness
// lease expired. NOT terminal — the leg-death fallback applies (unstripe,
// then re-engage through a fresh session set), exactly as for any other leg
// death. Never sent to a primary. Mirrored from Go
// wire.CloseCodeStripeLegOrphaned.
export const CLOSE_CODE_STRIPE_LEG_ORPHANED = 4005;
// The operator (or, later, R40's auto-tier) terminated this broadcast — sent
// to the publisher being killed and to every viewer of it (R39, docs/42 §4.4).
// TERMINAL in both roles: a viewer must not reconnect and a publisher must not
// auto-resume, because the ID is banned for at least the kill cooldown and a
// reclaim would only collect a 451. Distinct from 4000 precisely so the viewer
// can be told a moderator ended the stream. Mirrored from Go
// wire.CloseCodeTerminatedByOperator.
export const CLOSE_CODE_TERMINATED_BY_OPERATOR = 4006;

// Wire frameIds are uint32 and wrap; consumers must compare them with serial
// arithmetic (RFC 1982 flavored), not `<`/`>`. `a` is ahead of `b` when the
// forward distance b→a (mod 2^32) is under half the space — so ids just past
// a rollover count as ahead of ids just before it, while a genuine backwards
// jump (broadcaster restart resetting to 0) still reads as "behind" and is
// handled by keyframe-driven resync instead (reassembler watermark reset +
// reorder-buffer keyframe jump).
export function frameIdAhead(a: number, b: number): boolean {
  return a !== b && ((a - b) >>> 0) < 0x8000_0000;
}

// The wrap-aware successor of a uint32 frameId.
export function nextFrameId(id: number): number {
  return (id + 1) >>> 0;
}

export const MAX_DATAGRAM_SIZE = 1200;
export const VIDEO_CHUNK_HEADER_SIZE = 20;
export const MAX_CHUNK_PAYLOAD = MAX_DATAGRAM_SIZE - VIDEO_CHUNK_HEADER_SIZE;
// Mirrors wire.MaxChunkCount: the relay drops any chunk whose count exceeds
// this (memory-inflation defense), so a frame that needs more chunks can
// never reach viewers — the encoder must fail loudly instead.
export const MAX_CHUNK_COUNT = 3000;

// StreamFrame constants (mirror gawk-server/internal/wire).
export const STREAM_FRAME_HEADER_SIZE = 24;
// Absolute ceiling on one StreamFrame message (header + config + payload); the
// stream analogue of MAX_CHUNK_COUNT. A reader must never allocate beyond it
// from an untrusted length field.
export const MAX_KEYFRAME_BYTES = 8 * 1024 * 1024;

const FLAG_KEYFRAME = 0x01;

export class WireError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'WireError';
  }
}

export interface VideoChunkHeader {
  keyframe: boolean;
  frameId: number; // uint32, monotonic per publisher session
  chunkIndex: number; // uint16, 0-based
  chunkCount: number; // uint16, >= 1 and > chunkIndex
  timestampUs: bigint; // uint64, EncodedVideoChunk.timestamp
}

export interface DecoderConfigMessage {
  codec: string; // WebCodecs codec string, 1-255 ASCII bytes on the wire
  extradata: Uint8Array; // AVCC bytes for H.264; empty for VP8/VP9
}

export function peekType(dgram: Uint8Array): { version: number; msgType: number } {
  if (dgram.length < 2) {
    throw new WireError(`datagram too short: ${dgram.length} bytes, need at least 2`);
  }
  return { version: dgram[0], msgType: dgram[1] };
}

export function encodeVideoChunk(header: VideoChunkHeader, payload: Uint8Array): Uint8Array<ArrayBuffer> {
  if (payload.length > MAX_CHUNK_PAYLOAD) {
    throw new WireError(`payload ${payload.length} bytes exceeds MAX_CHUNK_PAYLOAD ${MAX_CHUNK_PAYLOAD}`);
  }
  if (header.chunkCount === 0 || header.chunkIndex >= header.chunkCount) {
    throw new WireError(`invalid chunk index ${header.chunkIndex} / count ${header.chunkCount}`);
  }
  if (header.chunkCount > MAX_CHUNK_COUNT) {
    throw new WireError(`chunk count ${header.chunkCount} exceeds MAX_CHUNK_COUNT ${MAX_CHUNK_COUNT}`);
  }
  const dgram = new Uint8Array(VIDEO_CHUNK_HEADER_SIZE + payload.length);
  const view = new DataView(dgram.buffer);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_VIDEO_CHUNK;
  dgram[2] = header.keyframe ? FLAG_KEYFRAME : 0;
  dgram[3] = 0;
  view.setUint32(4, header.frameId);
  view.setUint16(8, header.chunkIndex);
  view.setUint16(10, header.chunkCount);
  view.setBigUint64(12, header.timestampUs);
  dgram.set(payload, VIDEO_CHUNK_HEADER_SIZE);
  return dgram;
}

export function parseVideoChunk(dgram: Uint8Array): { header: VideoChunkHeader; payload: Uint8Array } {
  if (dgram.length < VIDEO_CHUNK_HEADER_SIZE) {
    throw new WireError(
      `datagram too short: ${dgram.length} bytes, need at least ${VIDEO_CHUNK_HEADER_SIZE} for video chunk`,
    );
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_VIDEO_CHUNK) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want video chunk`);
  }
  const view = new DataView(dgram.buffer, dgram.byteOffset, dgram.byteLength);
  const header: VideoChunkHeader = {
    keyframe: (dgram[2] & FLAG_KEYFRAME) !== 0,
    frameId: view.getUint32(4),
    chunkIndex: view.getUint16(8),
    chunkCount: view.getUint16(10),
    timestampUs: view.getBigUint64(12),
  };
  if (header.chunkCount === 0 || header.chunkIndex >= header.chunkCount) {
    throw new WireError(`invalid chunk index ${header.chunkIndex} / count ${header.chunkCount}`);
  }
  if (header.chunkCount > MAX_CHUNK_COUNT) {
    throw new WireError(`chunk count ${header.chunkCount} exceeds MAX_CHUNK_COUNT ${MAX_CHUNK_COUNT}`);
  }
  return { header, payload: dgram.subarray(VIDEO_CHUNK_HEADER_SIZE) };
}

export function encodeDecoderConfig(config: DecoderConfigMessage): Uint8Array<ArrayBuffer> {
  const codecBytes = new TextEncoder().encode(config.codec);
  if (codecBytes.length === 0 || codecBytes.length > 255) {
    throw new WireError(`invalid codec string: ${codecBytes.length} bytes, want 1-255`);
  }
  const total = 4 + codecBytes.length + config.extradata.length;
  if (total > MAX_DATAGRAM_SIZE) {
    throw new WireError(`datagram ${total} bytes exceeds MAX_DATAGRAM_SIZE ${MAX_DATAGRAM_SIZE}`);
  }
  const dgram = new Uint8Array(total);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_DECODER_CONFIG;
  dgram[2] = 0;
  dgram[3] = codecBytes.length;
  dgram.set(codecBytes, 4);
  dgram.set(config.extradata, 4 + codecBytes.length);
  return dgram;
}

export function parseDecoderConfig(dgram: Uint8Array): DecoderConfigMessage {
  if (dgram.length < 4) {
    throw new WireError(`datagram too short: ${dgram.length} bytes, need at least 4 for decoder config`);
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_DECODER_CONFIG) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want decoder config`);
  }
  const codecLen = dgram[3];
  if (codecLen === 0) {
    throw new WireError('invalid codec string: empty');
  }
  if (4 + codecLen > dgram.length) {
    throw new WireError(`codecLen ${codecLen} overruns ${dgram.length}-byte datagram`);
  }
  return {
    codec: new TextDecoder().decode(dgram.subarray(4, 4 + codecLen)),
    extradata: dgram.subarray(4 + codecLen),
  };
}

export interface StreamFrameHeader {
  keyframe: boolean;
  frameId: number; // uint32, shared numbering with datagram VideoChunks
  timestampUs: bigint; // uint64
  configLen: number; // uint32, byte length of the embedded DecoderConfig datagram (0 = none)
  payloadLen: number; // uint32, byte length of the encoded keyframe
}

// Encodes the 24-byte StreamFrame header (R8). Rejects a declared total
// exceeding MAX_KEYFRAME_BYTES.
export function encodeStreamFrameHeader(header: StreamFrameHeader): Uint8Array<ArrayBuffer> {
  if (STREAM_FRAME_HEADER_SIZE + header.configLen + header.payloadLen > MAX_KEYFRAME_BYTES) {
    throw new WireError(
      `stream frame ${header.configLen + header.payloadLen} body bytes exceeds MAX_KEYFRAME_BYTES ${MAX_KEYFRAME_BYTES}`,
    );
  }
  const buf = new Uint8Array(STREAM_FRAME_HEADER_SIZE);
  const view = new DataView(buf.buffer);
  buf[0] = WIRE_VERSION;
  buf[1] = TYPE_STREAM_FRAME;
  buf[2] = header.keyframe ? FLAG_KEYFRAME : 0;
  buf[3] = 0;
  view.setUint32(4, header.frameId);
  view.setBigUint64(8, header.timestampUs);
  view.setUint32(16, header.configLen);
  view.setUint32(20, header.payloadLen);
  return buf;
}

// Builds a complete StreamFrame message: header + optional config block + the
// encoded keyframe payload. config is a full DecoderConfig datagram (its
// 0x01/0x02 prefix included) or empty.
export function encodeStreamFrame(
  meta: { keyframe: boolean; frameId: number; timestampUs: bigint },
  config: Uint8Array,
  payload: Uint8Array,
): Uint8Array<ArrayBuffer> {
  const header = encodeStreamFrameHeader({
    ...meta,
    configLen: config.length,
    payloadLen: payload.length,
  });
  const out = new Uint8Array(header.length + config.length + payload.length);
  out.set(header, 0);
  out.set(config, header.length);
  out.set(payload, header.length + config.length);
  return out;
}

// Parses the 24-byte StreamFrame header at the start of buf. Does not require
// buf to hold the whole message (the reader consumes configLen + payloadLen
// further bytes from the stream). Rejects a declared total exceeding
// MAX_KEYFRAME_BYTES.
export function parseStreamFrameHeader(buf: Uint8Array): StreamFrameHeader {
  if (buf.length < STREAM_FRAME_HEADER_SIZE) {
    throw new WireError(
      `stream frame header too short: ${buf.length} bytes, need at least ${STREAM_FRAME_HEADER_SIZE}`,
    );
  }
  if (buf[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${buf[0].toString(16)}`);
  }
  if (buf[1] !== TYPE_STREAM_FRAME) {
    throw new WireError(`unexpected message type 0x${buf[1].toString(16)}, want stream frame`);
  }
  const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const header: StreamFrameHeader = {
    keyframe: (buf[2] & FLAG_KEYFRAME) !== 0,
    frameId: view.getUint32(4),
    timestampUs: view.getBigUint64(8),
    configLen: view.getUint32(16),
    payloadLen: view.getUint32(20),
  };
  if (STREAM_FRAME_HEADER_SIZE + header.configLen + header.payloadLen > MAX_KEYFRAME_BYTES) {
    throw new WireError(
      `stream frame ${header.configLen + header.payloadLen} body bytes exceeds MAX_KEYFRAME_BYTES ${MAX_KEYFRAME_BYTES}`,
    );
  }
  return header;
}

// TimeSync + ClockMapping (R5 Q2, docs/15). Both are tiny fixed-size
// datagrams, parsed strictly (exact length) per the R2 defensive-parsing
// discipline — a malformed datagram is dropped, never trusted partially.

export const TIME_SYNC_SIZE = 18;
export const CLOCK_MAPPING_SIZE = 10;

export interface TimeSyncMessage {
  clientTimeUs: bigint; // uint64, sender clock at send; the relay echoes it
  serverTimeUs: bigint; // uint64, relay monotonic clock at reply; 0 in requests
}

export function encodeTimeSync(msg: TimeSyncMessage): Uint8Array<ArrayBuffer> {
  const dgram = new Uint8Array(TIME_SYNC_SIZE);
  const view = new DataView(dgram.buffer);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_TIME_SYNC;
  view.setBigUint64(2, msg.clientTimeUs);
  view.setBigUint64(10, msg.serverTimeUs);
  return dgram;
}

export function parseTimeSync(dgram: Uint8Array): TimeSyncMessage {
  if (dgram.length !== TIME_SYNC_SIZE) {
    throw new WireError(`time sync must be exactly ${TIME_SYNC_SIZE} bytes, got ${dgram.length}`);
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_TIME_SYNC) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want time sync`);
  }
  const view = new DataView(dgram.buffer, dgram.byteOffset, dgram.byteLength);
  return {
    clientTimeUs: view.getBigUint64(2),
    serverTimeUs: view.getBigUint64(10),
  };
}

// offsetUs is signed: relayClockUs = frame.timestampUs + offsetUs (two's
// complement, uint64 wraparound intended on both sides).
export function encodeClockMapping(offsetUs: bigint): Uint8Array<ArrayBuffer> {
  const dgram = new Uint8Array(CLOCK_MAPPING_SIZE);
  const view = new DataView(dgram.buffer);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_CLOCK_MAPPING;
  view.setBigInt64(2, offsetUs);
  return dgram;
}

export function parseClockMapping(dgram: Uint8Array): bigint {
  if (dgram.length !== CLOCK_MAPPING_SIZE) {
    throw new WireError(
      `clock mapping must be exactly ${CLOCK_MAPPING_SIZE} bytes, got ${dgram.length}`,
    );
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_CLOCK_MAPPING) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want clock mapping`);
  }
  const view = new DataView(dgram.buffer, dgram.byteOffset, dgram.byteLength);
  return view.getBigInt64(2);
}

// DeliveryAck (R21, docs/26): exactly 5 bytes — version, type 0x0c, a mode
// byte, then the accepted buffer in ms (uint16). The encoder exists for tests
// and golden vectors; browsers only ever parse it. An unknown mode throws
// rather than defaulting: a viewer that cannot name what it got is the gap
// this message closes.

export const DELIVERY_ACK_SIZE = 5;

export type DeliveryServedMode = 'datagrams' | 'reliable' | 'dvr';

const DELIVERY_MODES: Record<number, DeliveryServedMode> = {
  0: 'datagrams',
  1: 'reliable',
  2: 'dvr',
};

export interface DeliveryAckMessage {
  mode: DeliveryServedMode;
  bufferMs: number;
}

export function encodeDeliveryAck(mode: DeliveryServedMode, bufferMs: number): Uint8Array<ArrayBuffer> {
  const code = Number(Object.keys(DELIVERY_MODES).find((k) => DELIVERY_MODES[Number(k)] === mode));
  const dgram = new Uint8Array(DELIVERY_ACK_SIZE);
  const view = new DataView(dgram.buffer);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_DELIVERY_ACK;
  dgram[2] = code;
  view.setUint16(3, bufferMs);
  return dgram;
}

export function parseDeliveryAck(dgram: Uint8Array): DeliveryAckMessage {
  if (dgram.length !== DELIVERY_ACK_SIZE) {
    throw new WireError(`delivery ack must be exactly ${DELIVERY_ACK_SIZE} bytes, got ${dgram.length}`);
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_DELIVERY_ACK) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want delivery ack`);
  }
  const mode = DELIVERY_MODES[dgram[2]];
  if (mode === undefined) {
    throw new WireError(`unknown delivery mode ${dgram[2]}`);
  }
  const view = new DataView(dgram.buffer, dgram.byteOffset, dgram.byteLength);
  return { mode, bufferMs: view.getUint16(3) };
}

// ViewerCount (R18, docs/23): exactly 6 bytes — version, type 0x0b, then a
// uint32 count. Strict fixed-length parse like TimeSync/ClockMapping. The
// encoder exists for tests and golden vectors; browsers only ever parse it.

export const VIEWER_COUNT_SIZE = 6;

export function encodeViewerCount(count: number): Uint8Array<ArrayBuffer> {
  const dgram = new Uint8Array(VIEWER_COUNT_SIZE);
  const view = new DataView(dgram.buffer);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_VIEWER_COUNT;
  view.setUint32(2, count);
  return dgram;
}

export function parseViewerCount(dgram: Uint8Array): number {
  if (dgram.length !== VIEWER_COUNT_SIZE) {
    throw new WireError(`viewer count must be exactly ${VIEWER_COUNT_SIZE} bytes, got ${dgram.length}`);
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_VIEWER_COUNT) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want viewer count`);
  }
  const view = new DataView(dgram.buffer, dgram.byteOffset, dgram.byteLength);
  return view.getUint32(2);
}

// ResumeToken (R17 W2): version, type 0x09, uint8 tokenLen, token bytes.
// The token is opaque on the wire (the server mints truncated HMACs).

export function encodeResumeToken(token: Uint8Array): Uint8Array<ArrayBuffer> {
  if (token.length === 0 || token.length > 255) {
    throw new WireError(`invalid resume token: ${token.length} bytes, want 1-255`);
  }
  const msg = new Uint8Array(3 + token.length);
  msg[0] = WIRE_VERSION;
  msg[1] = TYPE_RESUME_TOKEN;
  msg[2] = token.length;
  msg.set(token, 3);
  return msg;
}

export function parseResumeToken(msg: Uint8Array): Uint8Array {
  if (msg.length < 3) {
    throw new WireError(`message too short: ${msg.length} bytes, need at least 3 for resume token`);
  }
  if (msg[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${msg[0].toString(16)}`);
  }
  if (msg[1] !== TYPE_RESUME_TOKEN) {
    throw new WireError(`unexpected message type 0x${msg[1].toString(16)}, want resume token`);
  }
  const tokenLen = msg[2];
  if (tokenLen === 0 || 3 + tokenLen !== msg.length) {
    throw new WireError(`invalid resume token: declared ${tokenLen} bytes in ${msg.length}-byte message`);
  }
  return msg.subarray(3);
}

// TelemetryHello (R28, docs/33 §4.1): exactly 35 bytes — version, type 0x0d,
// a flags byte (bit 0 = enabled), uint16 reportIntervalMs, a 24-byte session
// token, and the 6-byte obfuscated broadcast key. Strict fixed-length parse
// like TimeSync/ClockMapping/ViewerCount, including reserved flag bits, which
// must be clear: a set bit means a field this build would misread. The
// encoder exists for tests and golden vectors; browsers only ever parse it.

export const TELEMETRY_HELLO_SIZE = 35;
export const TELEMETRY_SESSION_TOKEN_SIZE = 24;
export const TELEMETRY_BROADCAST_KEY_SIZE = 6;
const TELEMETRY_FLAG_ENABLED = 0x01;

export interface TelemetryHelloMessage {
  enabled: boolean;
  reportIntervalMs: number;
  // Hex, ready for the ingest envelope. The token is a bearer credential:
  // it is held in memory by the collector, sent only to the ingest endpoint,
  // and deliberately never placed in ViewerStats or a Copy-diagnostics blob.
  token: string;
  broadcastKey: string;
}

export function encodeTelemetryHello(msg: TelemetryHelloMessage): Uint8Array<ArrayBuffer> {
  const token = hexToBytes(msg.token, TELEMETRY_SESSION_TOKEN_SIZE, 'telemetry token');
  const key = hexToBytes(msg.broadcastKey, TELEMETRY_BROADCAST_KEY_SIZE, 'telemetry broadcast key');
  const out = new Uint8Array(TELEMETRY_HELLO_SIZE);
  const view = new DataView(out.buffer);
  out[0] = WIRE_VERSION;
  out[1] = TYPE_TELEMETRY_HELLO;
  out[2] = msg.enabled ? TELEMETRY_FLAG_ENABLED : 0;
  view.setUint16(3, msg.reportIntervalMs);
  out.set(token, 5);
  out.set(key, 5 + TELEMETRY_SESSION_TOKEN_SIZE);
  return out;
}

export function parseTelemetryHello(msg: Uint8Array): TelemetryHelloMessage {
  if (msg.length !== TELEMETRY_HELLO_SIZE) {
    throw new WireError(`telemetry hello must be exactly ${TELEMETRY_HELLO_SIZE} bytes, got ${msg.length}`);
  }
  if (msg[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${msg[0].toString(16)}`);
  }
  if (msg[1] !== TYPE_TELEMETRY_HELLO) {
    throw new WireError(`unexpected message type 0x${msg[1].toString(16)}, want telemetry hello`);
  }
  if ((msg[2] & ~TELEMETRY_FLAG_ENABLED) !== 0) {
    throw new WireError(`telemetry hello sets reserved flag bits (0x${msg[2].toString(16)})`);
  }
  const view = new DataView(msg.buffer, msg.byteOffset, msg.byteLength);
  return {
    enabled: (msg[2] & TELEMETRY_FLAG_ENABLED) !== 0,
    reportIntervalMs: view.getUint16(3),
    token: bytesToHex(msg.subarray(5, 5 + TELEMETRY_SESSION_TOKEN_SIZE)),
    broadcastKey: bytesToHex(msg.subarray(5 + TELEMETRY_SESSION_TOKEN_SIZE)),
  };
}

// The sessionId a token names (R28, docs/33 §4.2): hex of the 12-byte nonce
// sitting between the 4-byte expiry hour and the 8-byte tag. Mirrors Go's
// `wire.TelemetrySessionID` — the token's layout is a wire fact, so it gets
// exactly one definition per language (CODE-REVIEW.md).
//
// This is the only part of a token that is ever stored, shown or logged. It
// NAMES a session on both sides of the join (the relay records the same value
// as `/statusz subscriberDetails[].sessionId`, the ingest re-derives it from
// the verified token); it does not authorize writing to one — that is the tag,
// which stays behind. Which is exactly why a sessionId can be put on screen
// while the token it came from cannot.
export const TELEMETRY_NONCE_SIZE = 12;
export const TELEMETRY_SESSION_ID_LEN = TELEMETRY_NONCE_SIZE * 2;

export function telemetrySessionId(tokenHex: string): string {
  if (tokenHex.length !== TELEMETRY_SESSION_TOKEN_SIZE * 2 || !/^[0-9a-fA-F]*$/.test(tokenHex)) {
    throw new WireError(
      `invalid telemetry token: want ${TELEMETRY_SESSION_TOKEN_SIZE * 2} hex chars, got ${JSON.stringify(tokenHex)}`,
    );
  }
  return tokenHex.slice(8, 8 + TELEMETRY_SESSION_ID_LEN);
}

function bytesToHex(b: Uint8Array): string {
  let s = '';
  for (const byte of b) s += byte.toString(16).padStart(2, '0');
  return s;
}

function hexToBytes(hex: string, want: number, what: string): Uint8Array {
  if (hex.length !== want * 2 || !/^[0-9a-fA-F]*$/.test(hex)) {
    throw new WireError(`invalid ${what}: want ${want * 2} hex chars, got ${JSON.stringify(hex)}`);
  }
  const out = new Uint8Array(want);
  for (let i = 0; i < want; i++) out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  return out;
}

// --- StripeState (R30, docs/35 §5.3) ---------------------------------------
// Mirrors gawk-server/wire/stripe.go byte for byte.

export const STRIPE_STATE_SIZE = 5;
// Evidence-bound cap on stripe width (docs/34 finding 5 measured coexistence
// at 4 connections; 4 legs × target 6 covers every observed delta frame).
// A constant, not a knob — mirrors wire.MaxStripeLegs.
export const MAX_STRIPE_LEGS = 4;

const STRIPE_FLAG_STRIPED = 0x01;

export interface StripeState {
  // Delta datagrams should be suppressed on the session this rides.
  striped: boolean;
  // Current stripe width, informational (relay /statusz + metrics only).
  // In [1, MAX_STRIPE_LEGS] when striped, 0 otherwise.
  stripeN: number;
}

function validateStripeState(s: StripeState): void {
  if (s.striped) {
    if (s.stripeN < 1 || s.stripeN > MAX_STRIPE_LEGS) {
      throw new WireError(`bad stripeN ${s.stripeN}, want [1, ${MAX_STRIPE_LEGS}] while striped`);
    }
    return;
  }
  if (s.stripeN !== 0) {
    throw new WireError(`bad stripeN ${s.stripeN}, want 0 while not striped`);
  }
}

export function encodeStripeState(s: StripeState): Uint8Array<ArrayBuffer> {
  validateStripeState(s);
  const out = new Uint8Array(STRIPE_STATE_SIZE);
  out[0] = WIRE_VERSION;
  out[1] = TYPE_STRIPE_STATE;
  out[2] = s.striped ? STRIPE_FLAG_STRIPED : 0;
  out[3] = s.stripeN;
  out[4] = 0;
  return out;
}

// Strict, matching the Go parser: unknown flag bits reject — a future
// revision of this message gates on a new RelayCapabilities bit, never on
// old relays guessing.
export function parseStripeState(b: Uint8Array): StripeState {
  if (b.length !== STRIPE_STATE_SIZE) {
    throw new WireError(`stripe state must be exactly ${STRIPE_STATE_SIZE} bytes, got ${b.length}`);
  }
  if (b[0] !== WIRE_VERSION) throw new WireError(`bad wire version 0x${b[0].toString(16)}`);
  if (b[1] !== TYPE_STRIPE_STATE) {
    throw new WireError(`bad type 0x${b[1].toString(16)}, want stripe state 0x10`);
  }
  if ((b[2] & ~STRIPE_FLAG_STRIPED) !== 0) {
    throw new WireError(`unknown stripe flags 0x${b[2].toString(16)}`);
  }
  const s: StripeState = { striped: (b[2] & STRIPE_FLAG_STRIPED) !== 0, stripeN: b[3] };
  validateStripeState(s);
  return s;
}

// The stripe ordinal of a delta datagram: data chunk i has ordinal i, parity
// symbol r over an n-chunk frame has ordinal n+r — so parity keeps its
// measured tail-of-burst position on every leg. Leg j of stripe N carries
// the datagrams with stripeOrdinal(...) % N === j (docs/35 §5.2).
export function stripeOrdinal(chunkIndex: number, chunkCount: number, parityIndex: number | null): number {
  return parityIndex != null ? chunkCount + parityIndex : chunkIndex;
}

// AudioFrame + AudioConfig (R15, docs/20). Mirrors gawk-server/wire; the
// golden vectors in wire.test.ts pin byte compatibility.

export const AUDIO_FRAME_HEADER_SIZE = 16;
// One Opus packet per datagram; 20 ms at 128 kbps is ~320 bytes, so a
// conforming encoder never comes near this ceiling.
export const MAX_AUDIO_PAYLOAD = MAX_DATAGRAM_SIZE - AUDIO_FRAME_HEADER_SIZE;

export interface AudioFrameHeader {
  seq: number; // uint32, audio's own sequence space, monotonic per session
  timestampUs: bigint; // uint64, broadcaster performance.now() µs clock
}

export interface AudioConfigMessage {
  codec: string; // WebCodecs codec string ("opus"), 1-255 ASCII bytes
  sampleRate: number; // uint32, Hz (48000 for opus)
  channels: number; // uint8, >= 1 (2 for the stereo default)
  description: Uint8Array; // codec-specific config; empty for opus
}

export function encodeAudioFrame(header: AudioFrameHeader, payload: Uint8Array): Uint8Array<ArrayBuffer> {
  if (payload.length === 0 || payload.length > MAX_AUDIO_PAYLOAD) {
    throw new WireError(`invalid audio payload: ${payload.length} bytes, want 1-${MAX_AUDIO_PAYLOAD}`);
  }
  const dgram = new Uint8Array(AUDIO_FRAME_HEADER_SIZE + payload.length);
  const view = new DataView(dgram.buffer);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_AUDIO_FRAME;
  dgram[2] = 0;
  dgram[3] = 0;
  view.setUint32(4, header.seq);
  view.setBigUint64(8, header.timestampUs);
  dgram.set(payload, AUDIO_FRAME_HEADER_SIZE);
  return dgram;
}

export function parseAudioFrame(dgram: Uint8Array): { header: AudioFrameHeader; payload: Uint8Array } {
  if (dgram.length < AUDIO_FRAME_HEADER_SIZE) {
    throw new WireError(
      `datagram too short: ${dgram.length} bytes, need at least ${AUDIO_FRAME_HEADER_SIZE} for audio frame`,
    );
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_AUDIO_FRAME) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want audio frame`);
  }
  if (dgram.length === AUDIO_FRAME_HEADER_SIZE) {
    throw new WireError('invalid audio payload: empty');
  }
  const view = new DataView(dgram.buffer, dgram.byteOffset, dgram.byteLength);
  return {
    header: { seq: view.getUint32(4), timestampUs: view.getBigUint64(8) },
    payload: dgram.subarray(AUDIO_FRAME_HEADER_SIZE),
  };
}

export function encodeAudioConfig(config: AudioConfigMessage): Uint8Array<ArrayBuffer> {
  const codecBytes = new TextEncoder().encode(config.codec);
  if (codecBytes.length === 0 || codecBytes.length > 255) {
    throw new WireError(`invalid codec string: ${codecBytes.length} bytes, want 1-255`);
  }
  if (config.sampleRate === 0 || config.channels === 0) {
    throw new WireError(`invalid audio config: sampleRate ${config.sampleRate}, channels ${config.channels}`);
  }
  const total = 4 + codecBytes.length + 5 + config.description.length;
  if (total > MAX_DATAGRAM_SIZE) {
    throw new WireError(`datagram ${total} bytes exceeds MAX_DATAGRAM_SIZE ${MAX_DATAGRAM_SIZE}`);
  }
  const dgram = new Uint8Array(total);
  const view = new DataView(dgram.buffer);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_AUDIO_CONFIG;
  dgram[2] = 0;
  dgram[3] = codecBytes.length;
  dgram.set(codecBytes, 4);
  view.setUint32(4 + codecBytes.length, config.sampleRate);
  dgram[4 + codecBytes.length + 4] = config.channels;
  dgram.set(config.description, 4 + codecBytes.length + 5);
  return dgram;
}

export function parseAudioConfig(dgram: Uint8Array): AudioConfigMessage {
  if (dgram.length < 4) {
    throw new WireError(`datagram too short: ${dgram.length} bytes, need at least 4 for audio config`);
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_AUDIO_CONFIG) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want audio config`);
  }
  const codecLen = dgram[3];
  if (codecLen === 0) {
    throw new WireError('invalid codec string: empty');
  }
  if (4 + codecLen + 5 > dgram.length) {
    throw new WireError(`codecLen ${codecLen} overruns ${dgram.length}-byte datagram`);
  }
  const view = new DataView(dgram.buffer, dgram.byteOffset, dgram.byteLength);
  const config: AudioConfigMessage = {
    codec: new TextDecoder().decode(dgram.subarray(4, 4 + codecLen)),
    sampleRate: view.getUint32(4 + codecLen),
    channels: dgram[4 + codecLen + 4],
    description: dgram.subarray(4 + codecLen + 5),
  };
  if (config.sampleRate === 0 || config.channels === 0) {
    throw new WireError(`invalid audio config: sampleRate ${config.sampleRate}, channels ${config.channels}`);
  }
  return config;
}

// Reliable-carrier framing (R19). The viewer only ever parses carriers (the
// relay writes them), but the encoders exist for tests and golden vectors.

export const CARRIER_PROLOGUE_SIZE = 2;
export const CARRIER_RECORD_HEADER_SIZE = 2;

export function encodeCarrierPrologue(): Uint8Array<ArrayBuffer> {
  return new Uint8Array([WIRE_VERSION, TYPE_RELIABLE_CARRIER]);
}

export function encodeCarrierRecord(dgram: Uint8Array): Uint8Array<ArrayBuffer> {
  if (dgram.length === 0 || dgram.length > MAX_DATAGRAM_SIZE) {
    throw new WireError(`invalid carrier record: ${dgram.length} bytes, want 1-${MAX_DATAGRAM_SIZE}`);
  }
  const out = new Uint8Array(CARRIER_RECORD_HEADER_SIZE + dgram.length);
  new DataView(out.buffer).setUint16(0, dgram.length);
  out.set(dgram, CARRIER_RECORD_HEADER_SIZE);
  return out;
}

export function parseCarrierPrologue(buf: Uint8Array): void {
  if (buf.length < CARRIER_PROLOGUE_SIZE) {
    throw new WireError(`carrier prologue too short: ${buf.length} bytes, need ${CARRIER_PROLOGUE_SIZE}`);
  }
  if (buf[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${buf[0].toString(16)}`);
  }
  if (buf[1] !== TYPE_RELIABLE_CARRIER) {
    throw new WireError(`unexpected message type 0x${buf[1].toString(16)}, want reliable carrier`);
  }
}

// Incremental record framing for a carrier stream: push() arbitrary read
// chunks (records span chunk boundaries), get back complete records. Each
// returned record is a fresh copy owning its buffer — safe to retain in the
// reassembler and to transfer across a worker boundary. Throws WireError on a
// zero or oversize declared length (framing corruption — the caller must
// abandon the stream; there is no way to resynchronize a length-prefixed
// stream after a bad length).
export class CarrierRecordParser {
  private pending: Uint8Array = new Uint8Array(0);

  // True while a partial record is buffered — clean EOF with hasPartial()
  // means the stream was truncated mid-record.
  hasPartial(): boolean {
    return this.pending.length > 0;
  }

  push(chunk: Uint8Array): Uint8Array<ArrayBuffer>[] {
    let buf: Uint8Array;
    if (this.pending.length === 0) {
      buf = chunk;
    } else {
      buf = new Uint8Array(this.pending.length + chunk.length);
      buf.set(this.pending, 0);
      buf.set(chunk, this.pending.length);
    }

    const records: Uint8Array<ArrayBuffer>[] = [];
    let offset = 0;
    while (buf.length - offset >= CARRIER_RECORD_HEADER_SIZE) {
      const len = (buf[offset] << 8) | buf[offset + 1];
      if (len === 0 || len > MAX_DATAGRAM_SIZE) {
        throw new WireError(`invalid carrier record: declared ${len} bytes, want 1-${MAX_DATAGRAM_SIZE}`);
      }
      if (buf.length - offset < CARRIER_RECORD_HEADER_SIZE + len) break;
      records.push(new Uint8Array(buf.subarray(offset + CARRIER_RECORD_HEADER_SIZE, offset + CARRIER_RECORD_HEADER_SIZE + len)));
      offset += CARRIER_RECORD_HEADER_SIZE + len;
    }
    // Copy the remainder: `chunk` may be reused by the caller's reader.
    this.pending = new Uint8Array(buf.subarray(offset));
    return records;
  }
}

// The 31 allowed broadcast-ID symbols (no 0/O, 1/I/L) — mirrors
// gawk-server/internal/broadcastid.
export const BROADCAST_ID_ALPHABET = '23456789ABCDEFGHJKMNPQRSTUVWXYZ';

export function parseBroadcastAnnounce(dgram: Uint8Array): string {
  if (dgram.length < 3) {
    throw new WireError(`message too short: ${dgram.length} bytes, need at least 3 for broadcast announce`);
  }
  if (dgram[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${dgram[0].toString(16)}`);
  }
  if (dgram[1] !== TYPE_BROADCAST_ANNOUNCE) {
    throw new WireError(`unexpected message type 0x${dgram[1].toString(16)}, want broadcast announce`);
  }
  const idLen = dgram[2];
  if (3 + idLen !== dgram.length) {
    throw new WireError(`expected ${3 + idLen} bytes for ID length ${idLen}, got ${dgram.length}`);
  }
  const id = new TextDecoder().decode(dgram.subarray(3));
  for (let i = 0; i < id.length; i++) {
    if (BROADCAST_ID_ALPHABET.indexOf(id[i]) === -1) {
      throw new WireError(`invalid character "${id[i]}" in broadcast ID`);
    }
  }
  return id;
}

// RelayIdentity (R37, docs/40 §4.4): version, type 0x11, strict-zero flags,
// uint8 versionLen (1–32) + printable-ASCII release version, uint8 nameLen
// (0–64) + valid-UTF-8 operator name, then IGNORED trailing extension bytes —
// the one message family whose parser deliberately tolerates a longer buffer
// (docs/40 §4.9; appended fields are the extension mechanism, flags are not).
// The encoder exists for tests and golden vectors; browsers only ever parse.

export const RELAY_IDENTITY_MAX_VERSION_LEN = 32;
export const RELAY_IDENTITY_MAX_NAME_LEN = 64;

export interface RelayIdentityMessage {
  serverVersion: string;
  // Attacker-influenced trust UI: render sanitized, never in place of the
  // host (docs/40 §4.4 F6).
  name: string;
}

export function encodeRelayIdentity(msg: RelayIdentityMessage): Uint8Array<ArrayBuffer> {
  const version = new TextEncoder().encode(msg.serverVersion);
  const name = new TextEncoder().encode(msg.name);
  if (version.length === 0 || version.length > RELAY_IDENTITY_MAX_VERSION_LEN) {
    throw new WireError(`relay identity version must be 1-${RELAY_IDENTITY_MAX_VERSION_LEN} bytes`);
  }
  if (name.length > RELAY_IDENTITY_MAX_NAME_LEN) {
    throw new WireError(`relay identity name exceeds ${RELAY_IDENTITY_MAX_NAME_LEN} bytes`);
  }
  const out = new Uint8Array(4 + version.length + 1 + name.length);
  out[0] = WIRE_VERSION;
  out[1] = TYPE_RELAY_IDENTITY;
  out[2] = 0;
  out[3] = version.length;
  out.set(version, 4);
  out[4 + version.length] = name.length;
  out.set(name, 4 + version.length + 1);
  return out;
}

export function parseRelayIdentity(msg: Uint8Array): RelayIdentityMessage {
  if (msg.length < 5) {
    throw new WireError(`relay identity needs at least 5 bytes, got ${msg.length}`);
  }
  if (msg[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${msg[0].toString(16)}`);
  }
  if (msg[1] !== TYPE_RELAY_IDENTITY) {
    throw new WireError(`unexpected message type 0x${msg[1].toString(16)}, want relay identity`);
  }
  if (msg[2] !== 0) {
    throw new WireError(`relay identity sets reserved flag bits (0x${msg[2].toString(16)})`);
  }
  const versionLen = msg[3];
  if (versionLen === 0 || versionLen > RELAY_IDENTITY_MAX_VERSION_LEN) {
    throw new WireError(`relay identity versionLen ${versionLen} out of range`);
  }
  if (4 + versionLen + 1 > msg.length) {
    throw new WireError('relay identity versionLen overruns the message');
  }
  const versionBytes = msg.subarray(4, 4 + versionLen);
  for (const b of versionBytes) {
    if (b < 0x20 || b > 0x7e) throw new WireError('relay identity version is not printable ASCII');
  }
  const nameLen = msg[4 + versionLen];
  if (nameLen > RELAY_IDENTITY_MAX_NAME_LEN) {
    throw new WireError(`relay identity nameLen ${nameLen} out of range`);
  }
  const nameStart = 4 + versionLen + 1;
  if (nameStart + nameLen > msg.length) {
    throw new WireError('relay identity nameLen overruns the message');
  }
  let name: string;
  try {
    name = new TextDecoder('utf-8', { fatal: true }).decode(msg.subarray(nameStart, nameStart + nameLen));
  } catch {
    throw new WireError('relay identity name is not valid UTF-8');
  }
  // Bytes past the name are the reserved extension space — ignored.
  return { serverVersion: new TextDecoder().decode(versionBytes), name };
}

// TelemetryEndpoint (R37, docs/40 §4.10): version, type 0x12, strict-zero
// flags, uint16 urlLen (1–512, big-endian) + an absolute https URL, then
// ignored trailing extension bytes. A message failing any rule here degrades
// caller-side to "no advertised URL" — never to a failed session.

export const TELEMETRY_ENDPOINT_MAX_URL_LEN = 512;

export function encodeTelemetryEndpoint(url: string): Uint8Array<ArrayBuffer> {
  validateTelemetryEndpointUrl(url);
  const bytes = new TextEncoder().encode(url);
  const out = new Uint8Array(5 + bytes.length);
  const view = new DataView(out.buffer);
  out[0] = WIRE_VERSION;
  out[1] = TYPE_TELEMETRY_ENDPOINT;
  out[2] = 0;
  view.setUint16(3, bytes.length);
  out.set(bytes, 5);
  return out;
}

export function parseTelemetryEndpoint(msg: Uint8Array): string {
  if (msg.length < 6) {
    throw new WireError(`telemetry endpoint needs at least 6 bytes, got ${msg.length}`);
  }
  if (msg[0] !== WIRE_VERSION) {
    throw new WireError(`unsupported version 0x${msg[0].toString(16)}`);
  }
  if (msg[1] !== TYPE_TELEMETRY_ENDPOINT) {
    throw new WireError(`unexpected message type 0x${msg[1].toString(16)}, want telemetry endpoint`);
  }
  if (msg[2] !== 0) {
    throw new WireError(`telemetry endpoint sets reserved flag bits (0x${msg[2].toString(16)})`);
  }
  const view = new DataView(msg.buffer, msg.byteOffset, msg.byteLength);
  const urlLen = view.getUint16(3);
  if (urlLen === 0 || urlLen > TELEMETRY_ENDPOINT_MAX_URL_LEN) {
    throw new WireError(`telemetry endpoint urlLen ${urlLen} out of range`);
  }
  if (5 + urlLen > msg.length) {
    throw new WireError('telemetry endpoint urlLen overruns the message');
  }
  const url = new TextDecoder().decode(msg.subarray(5, 5 + urlLen));
  validateTelemetryEndpointUrl(url);
  // Bytes past the URL are the reserved extension space — ignored.
  return url;
}

function validateTelemetryEndpointUrl(url: string): void {
  if (url.length === 0 || url.length > TELEMETRY_ENDPOINT_MAX_URL_LEN) {
    throw new WireError(`telemetry endpoint URL must be 1-${TELEMETRY_ENDPOINT_MAX_URL_LEN} bytes`);
  }
  for (let i = 0; i < url.length; i++) {
    const c = url.charCodeAt(i);
    if (c <= 0x20 || c > 0x7e) throw new WireError('telemetry endpoint URL contains a non-URL byte');
  }
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    throw new WireError('telemetry endpoint URL does not parse');
  }
  if (parsed.protocol !== 'https:' || parsed.host === '') {
    throw new WireError('telemetry endpoint URL is not absolute https');
  }
}
