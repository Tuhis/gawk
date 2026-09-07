// R42 (docs/44 §4.6, D14): the room CONTROL session. One WebTransport
// connection to `CONNECT /room/{code}` (or `/room/new` to mint), one
// bidirectional stream carrying length-prefixed wire records: RoomHello out,
// RoomState / RoomEvent in, RoomCommand out. No media ever rides this
// connection — a participant's tiles are ordinary /subscribe sessions
// (viewer-session.ts) that know nothing about rooms.
//
// Reconnect policy (docs/44 §4.5, reconnect.ts): 4007 RoomEnded is the ONLY
// terminal code — the room is gone, the participant's media sessions have
// their own lifecycle. A 4002 drain reconnects immediately, an abrupt drop
// (home-pod death, proxy upstream loss) follows the shared ladder, and every
// reconnect re-sends the hello with the remembered nickname and re-attaches
// whatever this session attached, because a reconnected broadcaster must
// re-attach before it can detach again (the RM2 contract).
//
// Sequence gaps: RoomEvent.seq is monotonic per room; a delta with
// seq > last + 1 means one was missed (a proxy re-establishment, an adoption).
// The session sends Resync and ignores deltas until the next RoomState, which
// the client replaces wholesale — never merges.
//
// The WebTransport API hides HTTP statuses, so a pre-upgrade refusal (404
// unknown code, 403 wrong grant, 429 full, 451 banned) is one opaque
// "handshake failed" to us: surfaced as 'refused', worded "not found or
// refused". Post-upgrade the relay closes with the status as the close code,
// which IS visible and maps to a specific failure.

import { log } from '../lib/logger';
import { hexToBytes, webTransportInit } from './connection';
import {
  RECONNECT_MAX_ATTEMPTS,
  isTerminalRoomClose,
  reconnectDelayMs,
  type ReconnectInfo,
} from './reconnect';
import {
  CLOSE_CODE_ROOM_ENDED,
  ROOM_COMMAND_ATTACH,
  ROOM_COMMAND_DETACH,
  ROOM_COMMAND_END_ROOM,
  ROOM_COMMAND_RESYNC,
  ROOM_COMMAND_SET_NICKNAME,
  ROOM_EVENT_COMMAND_REJECTED,
  ROOM_EVENT_ROOM_ENDING,
  ROOM_PROTOCOL_VERSION,
  RoomRecordReader,
  RoomUnknownKindError,
  TYPE_ROOM_EVENT,
  TYPE_ROOM_STATE,
  WireError,
  encodeRoomCommand,
  encodeRoomHello,
  encodeRoomRecord,
  parseRoomEvent,
  parseRoomState,
  type RoomCommand,
  type RoomEvent,
  type RoomState,
} from './wire';

// Where the session dials. A mint becomes a join once the first RoomState
// hands back the code and the creator token.
export type RoomTarget =
  | { kind: 'join'; code: string }
  | {
      kind: 'mint';
      broadcastId: string;
      resumeTokenHex: string;
      label: string;
      createSecret?: string;
    };

// The grant presented on join: a creator token (dynamic rooms) or an attach
// secret (static rooms). Shape shared with features/room/grantHandoff.ts.
export type RoomSessionGrant = { kind: 'creator'; tokenHex: string } | { kind: 'attach'; secret: string };

export interface RoomSessionOptions {
  serverUrl: string;
  certHashHex?: string;
  target: RoomTarget;
  nickname: string;
  clientKind: number; // ROOM_CLIENT_*
  grant?: RoomSessionGrant | null;
}

// How a session failed for good. Structural, never sniffed from messages.
// - 'refused': the first dial never completed — the relay answered a status
//   the browser hides (unknown code, wrong grant, full, banned, rooms off).
// - 'notFound' / 'full' / 'forbidden': the relay upgraded and then closed
//   with the status as the close code before sending any RoomState.
// - 'lost': the reconnect budget ran out.
export type RoomFailureKind = 'refused' | 'notFound' | 'full' | 'forbidden' | 'lost';

export class RoomConnectError extends Error {
  readonly kind: RoomFailureKind;
  readonly closeCode: number | null;
  constructor(kind: RoomFailureKind, message: string, closeCode: number | null = null) {
    super(message);
    this.name = 'RoomConnectError';
    this.kind = kind;
    this.closeCode = closeCode;
  }
}

export interface RoomSessionCallbacks {
  // The hello is on its way; a RoomState follows (or a refusal close).
  onConnected: () => void;
  // A full snapshot — replace, never merge.
  onState: (state: RoomState) => void;
  // One delta, already sequence-checked.
  onEvent: (ev: RoomEvent) => void;
  onReconnecting: (info: ReconnectInfo) => void;
  // 4007: the room ended. `reason` is the ROOM_END_REASON_* the preceding
  // RoomEnding event carried, or null when the close arrived without one.
  onEnded: (reason: number | null) => void;
  // Terminal failure (never after onEnded).
  onError: (err: RoomConnectError) => void;
}

// Post-upgrade close codes the relay uses for join failures (RM2 contract):
// the HTTP status it would have answered had the failure been pre-upgrade.
const CLOSE_NOT_FOUND = 404;
const CLOSE_FORBIDDEN = 403;
const CLOSE_FULL = 429;

// The read loop and wt.closed settle in unspecified order; only wt.closed
// carries the close code (CODE-REVIEW "one event, one authoritative signal").
// The loop's end waits this long for the code before acting without one.
const CLOSE_INFO_GRACE_MS = 250;

export const REFUSED_MESSAGE = 'Room not found or refused';

function failureForClose(code: number | null): RoomConnectError {
  switch (code) {
    case CLOSE_NOT_FOUND:
      return new RoomConnectError('notFound', 'No room with this code', code);
    case CLOSE_FULL:
      return new RoomConnectError('full', 'The room is full', code);
    case CLOSE_FORBIDDEN:
      return new RoomConnectError('forbidden', 'The room refused this key', code);
    default:
      return new RoomConnectError('refused', REFUSED_MESSAGE, code);
  }
}

interface Attempt {
  wt: WebTransport;
  writer: WritableStreamDefaultWriter<Uint8Array>;
  gotState: boolean;
  settled: boolean;
}

export class RoomSession {
  private readonly opts: RoomSessionOptions;
  private readonly cb: RoomSessionCallbacks;
  private target: RoomTarget;
  private grant: RoomSessionGrant | null;
  private nickname: string;
  private attempt: Attempt | null = null;
  private stopped = false;
  private everJoined = false;
  private attemptNo = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private lastSeq = 0;
  private resyncing = false;
  private endingReason: number | null = null;
  private lastReason = '';
  private writeChain: Promise<void> = Promise.resolve();
  // Re-sent on every (re)connect after the hello: the relay only lets the
  // session that attached (in THIS connection) detach again.
  private autoAttach: { broadcastId: string; resumeTokenHex: string; label: string } | null = null;

  constructor(opts: RoomSessionOptions, cb: RoomSessionCallbacks) {
    this.opts = opts;
    this.cb = cb;
    this.target = opts.target;
    this.grant = opts.grant ?? null;
    this.nickname = opts.nickname;
  }

  // The code this session is joined to — known from the start for a join,
  // from the first RoomState for a mint.
  get code(): string | null {
    return this.target.kind === 'join' ? this.target.code : null;
  }

  // Rejects with a RoomConnectError when the first dial fails; later failures
  // arrive through the callbacks.
  async start(): Promise<void> {
    if (this.stopped) throw new Error('room session already stopped');
    try {
      await this.dial();
    } catch (e) {
      const err = e instanceof RoomConnectError ? e : new RoomConnectError('refused', REFUSED_MESSAGE);
      throw err;
    }
  }

  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    this.closeAttempt();
  }

  // ---- Commands ------------------------------------------------------------

  attach(broadcastId: string, resumeTokenHex: string, label: string): void {
    this.autoAttach = { broadcastId, resumeTokenHex, label };
    this.send({
      kind: ROOM_COMMAND_ATTACH,
      broadcastId,
      resumeToken: hexToBytes(resumeTokenHex),
      label,
    });
  }

  detach(broadcastId: string): void {
    if (this.autoAttach?.broadcastId === broadcastId) this.autoAttach = null;
    this.send({ kind: ROOM_COMMAND_DETACH, broadcastId });
  }

  setNickname(nickname: string): void {
    // Remembered so a reconnect's hello carries it (the relay assigns ids per
    // connection; the nickname is the one thing that makes it the same person).
    this.nickname = nickname;
    this.send({ kind: ROOM_COMMAND_SET_NICKNAME, nickname });
  }

  endRoom(): void {
    this.send({ kind: ROOM_COMMAND_END_ROOM });
  }

  resync(): void {
    this.resyncing = true;
    this.send({ kind: ROOM_COMMAND_RESYNC });
  }

  // ---- Dialing -------------------------------------------------------------

  private url(): string {
    const base = this.opts.serverUrl.replace(/\/+$/, '');
    const params = new URLSearchParams();
    if (this.nickname !== '') params.set('name', this.nickname);
    if (this.target.kind === 'mint') {
      params.set('broadcast', this.target.broadcastId);
      params.set('resume', this.target.resumeTokenHex);
      params.set('label', this.target.label);
      if (this.target.createSecret) params.set('create', this.target.createSecret);
      return `${base}/room/new?${params.toString()}`;
    }
    if (this.grant?.kind === 'creator') params.set('creator', this.grant.tokenHex);
    if (this.grant?.kind === 'attach') params.set('attach', this.grant.secret);
    const q = params.toString();
    return `${base}/room/${encodeURIComponent(this.target.code)}${q === '' ? '' : `?${q}`}`;
  }

  private async dial(): Promise<void> {
    const wt = new WebTransport(this.url(), webTransportInit({ certHashHex: this.opts.certHashHex }));
    // Capture the close info from the one signal that carries it, whichever
    // order it settles in relative to the read loop.
    const closeInfo: Promise<{ code: number | null; reason: string }> = wt.closed.then(
      (info) => ({
        code: typeof info?.closeCode === 'number' ? info.closeCode : null,
        reason: info?.reason ?? 'session closed',
      }),
      (err: unknown) => {
        const e = err as { closeCode?: number; reason?: string; message?: string } | null;
        return {
          code: typeof e?.closeCode === 'number' ? e.closeCode : null,
          reason: e?.reason || e?.message || 'session lost',
        };
      },
    );
    try {
      await wt.ready;
    } catch (e) {
      // Pre-upgrade refusal or an unreachable relay — the status is hidden.
      const msg = e instanceof Error ? e.message : String(e);
      log.info(`room dial failed: ${msg}`);
      throw new RoomConnectError('refused', REFUSED_MESSAGE);
    }
    if (this.stopped) {
      try {
        wt.close();
      } catch {
        // already gone
      }
      return;
    }
    let stream: WebTransportBidirectionalStream;
    try {
      stream = await wt.createBidirectionalStream();
    } catch (e) {
      try {
        wt.close();
      } catch {
        // already gone
      }
      throw new RoomConnectError('refused', e instanceof Error ? e.message : String(e));
    }
    const attempt: Attempt = {
      wt,
      writer: stream.writable.getWriter() as WritableStreamDefaultWriter<Uint8Array>,
      gotState: false,
      settled: false,
    };
    this.attempt = attempt;
    this.writeChain = Promise.resolve();
    this.resyncing = false;
    this.write(
      encodeRoomHello({
        protocol: ROOM_PROTOCOL_VERSION,
        clientKind: this.opts.clientKind,
        wantCaps: 0,
        nickname: this.nickname,
      }),
    );
    if (this.autoAttach) {
      const a = this.autoAttach;
      this.send({
        kind: ROOM_COMMAND_ATTACH,
        broadcastId: a.broadcastId,
        resumeToken: hexToBytes(a.resumeTokenHex),
        label: a.label,
      });
    }
    this.cb.onConnected();
    void this.readLoop(attempt, stream.readable, closeInfo);
  }

  private async readLoop(
    attempt: Attempt,
    readable: ReadableStream<Uint8Array>,
    closeInfo: Promise<{ code: number | null; reason: string }>,
  ): Promise<void> {
    const reader = readable.getReader();
    const records = new RoomRecordReader();
    let loopError: string | null = null;
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        if (!value || value.length === 0) continue;
        for (const rec of records.push(value)) this.handleRecord(attempt, rec);
      }
    } catch (e) {
      loopError = e instanceof Error ? e.message : String(e);
    } finally {
      reader.releaseLock();
    }
    if (attempt !== this.attempt || attempt.settled) return;
    // The loop ended: give wt.closed a moment to hand over the close code.
    const info = await Promise.race([
      closeInfo,
      new Promise<null>((resolve) => setTimeout(() => resolve(null), CLOSE_INFO_GRACE_MS)),
    ]);
    this.settle(attempt, info?.code ?? null, info?.reason ?? loopError ?? 'control stream ended');
  }

  private handleRecord(attempt: Attempt, rec: Uint8Array): void {
    if (rec.length < 2) return;
    const type = rec[1];
    try {
      if (type === TYPE_ROOM_STATE) {
        const state = parseRoomState(rec);
        attempt.gotState = true;
        this.everJoined = true;
        this.attemptNo = 0;
        this.lastSeq = state.seq;
        this.resyncing = false;
        if (this.target.kind === 'mint') {
          // The mint answered: from here on this is a join, with the creator
          // token as the grant (the token arrives ONLY in this snapshot).
          this.target = { kind: 'join', code: state.code };
        }
        if (state.creatorToken.length > 0) {
          this.grant = { kind: 'creator', tokenHex: bytesToHexLower(state.creatorToken) };
        }
        this.cb.onState(state);
        return;
      }
      if (type !== TYPE_ROOM_EVENT) return; // unknown types are ignored, not fatal
      let ev: RoomEvent;
      try {
        ev = parseRoomEvent(rec);
      } catch (e) {
        if (e instanceof RoomUnknownKindError) {
          // A reserved (chat/voice) kind from a newer relay: it still
          // occupies a sequence number.
          if (e.seq !== undefined && e.seq > this.lastSeq) this.lastSeq = e.seq;
          return;
        }
        throw e;
      }
      if (ev.kind === ROOM_EVENT_COMMAND_REJECTED) {
        // Carries the CURRENT seq without advancing it.
        if (ev.seq > this.lastSeq + 1) this.noteGap(ev.seq);
        if (!this.resyncing) this.cb.onEvent(ev);
        return;
      }
      if (ev.seq <= this.lastSeq) return; // a duplicate/stale delta
      if (ev.seq > this.lastSeq + 1) {
        this.noteGap(ev.seq);
        return;
      }
      this.lastSeq = ev.seq;
      if (ev.kind === ROOM_EVENT_ROOM_ENDING) this.endingReason = ev.reason;
      if (!this.resyncing) this.cb.onEvent(ev);
    } catch (e) {
      // A malformed record from the relay: the stream cannot be resynchronized
      // (length-prefixed), so drop the connection and let reconnect rebuild
      // from a fresh RoomState.
      const msg = e instanceof WireError ? e.message : String(e);
      log.warn('room control record unreadable; reconnecting:', msg);
      this.lastReason = `malformed control record: ${msg}`;
      this.closeAttempt();
      this.settle(attempt, null, this.lastReason);
    }
  }

  private noteGap(seq: number): void {
    if (this.resyncing) return;
    log.info(`room event gap: expected ${this.lastSeq + 1}, got ${seq}; requesting resync`);
    this.resync();
  }

  // ---- Endings -------------------------------------------------------------

  private settle(attempt: Attempt, code: number | null, reason: string): void {
    if (attempt.settled) return;
    attempt.settled = true;
    if (this.attempt === attempt) this.attempt = null;
    try {
      attempt.writer.releaseLock();
    } catch {
      // the stream is gone
    }
    if (this.stopped) return;
    this.lastReason = reason;

    if (isTerminalRoomClose(code)) {
      log.info(`room ended by relay (code ${CLOSE_CODE_ROOM_ENDED}).`);
      this.stopped = true;
      this.cb.onEnded(this.endingReason);
      return;
    }
    if (!attempt.gotState) {
      if (!this.everJoined) {
        // Never got in: a post-upgrade refusal with the status as the code.
        this.stopped = true;
        this.cb.onError(failureForClose(code));
        return;
      }
      if (code === CLOSE_NOT_FOUND) {
        // Reconnected into a room that ended meanwhile.
        this.stopped = true;
        this.cb.onEnded(this.endingReason);
        return;
      }
    }
    this.scheduleReconnect(code);
  }

  private scheduleReconnect(closeCode: number | null): void {
    this.attemptNo += 1;
    if (this.attemptNo > RECONNECT_MAX_ATTEMPTS) {
      this.stopped = true;
      this.cb.onError(
        new RoomConnectError(
          'lost',
          `reconnect failed after ${RECONNECT_MAX_ATTEMPTS} attempts: ${this.lastReason}`,
          closeCode,
        ),
      );
      return;
    }
    const delayMs = reconnectDelayMs(this.attemptNo, closeCode);
    log.info(`room reconnect attempt ${this.attemptNo}/${RECONNECT_MAX_ATTEMPTS} in ${delayMs}ms (${this.lastReason})`);
    this.cb.onReconnecting({ attempt: this.attemptNo, delayMs, reason: this.lastReason, closeCode });
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.tryReconnect();
    }, delayMs);
  }

  private async tryReconnect(): Promise<void> {
    if (this.stopped) return;
    try {
      await this.dial();
    } catch (e) {
      // A failed reconnect dial consumes budget so a dead relay burns out.
      this.lastReason = e instanceof Error ? e.message : String(e);
      if (!this.stopped) this.scheduleReconnect(null);
    }
  }

  private closeAttempt(): void {
    const a = this.attempt;
    if (!a) return;
    this.attempt = null;
    a.settled = true;
    try {
      a.writer.releaseLock();
    } catch {
      // the stream is gone
    }
    try {
      a.wt.close();
    } catch {
      // already closed
    }
  }

  // ---- Writing -------------------------------------------------------------

  private send(cmd: RoomCommand): void {
    let msg: Uint8Array;
    try {
      msg = encodeRoomCommand(cmd);
    } catch (e) {
      log.warn('room command not sent:', e);
      return;
    }
    this.write(msg);
  }

  private write(msg: Uint8Array): void {
    const a = this.attempt;
    if (!a || a.settled) return; // not connected: the next connect re-sends what matters
    const record = encodeRoomRecord(msg);
    this.writeChain = this.writeChain
      .then(() => a.writer.write(record))
      .catch((e) => {
        // The read loop reports the death; a failed write is only noise here.
        if (!a.settled) log.info('room control write failed:', e);
      });
  }
}

function bytesToHexLower(bytes: Uint8Array): string {
  let out = '';
  for (const b of bytes) out += b.toString(16).padStart(2, '0');
  return out;
}
