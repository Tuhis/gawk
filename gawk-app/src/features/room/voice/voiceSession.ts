// One browser voice session with gawk-mumble-bridge (R79, docs/80 D5, D20):
// `CONNECT /voice` on the URL a VoiceTicket named, one bidirectional stream
// carrying room-record-framed VoiceHello (out, first) / VoiceState (in) /
// VoiceControl (out), and VoiceFrame datagrams both ways. No reconnect logic
// lives here — a session is one ticket's worth; voiceController.ts decides
// whether to ask for another.
//
// How it ends is reported exactly once (onClosed), from ONE authoritative
// signal: wt.closed, the only one that carries the close code. The record read
// loop ending defers to it for CLOSE_INFO_GRACE_MS, and a code the bridge
// stated in-band first (SessionClosing, docs/59 — Chrome never reads a
// webtransport-go close code) stands in when wt.closed settles without one.
// A local close() reports nothing: the caller already knows.

import { log } from '../../../lib/logger';
import {
  DatagramSender,
  dialWebTransport,
  newCarrierCounters,
  readDatagrams,
  readServerStreams,
} from '../../../transport/connection';
import {
  RoomRecordReader,
  TYPE_VOICE_FRAME,
  TYPE_VOICE_STATE,
  VOICE_PROTOCOL_VERSION,
  WireError,
  encodeRoomRecord,
  encodeVoiceControl,
  encodeVoiceFrame,
  encodeVoiceHello,
  parseVoiceFrame,
  parseVoiceState,
  type VoiceFrameHeader,
  type VoiceStateMessage,
} from '../../../transport/wire';

// Where a voice join failed. Callers behave differently by phase
// (CODE-REVIEW "type your failure phases"): a ticket refusal says why the
// relay said no; a connect failure may be retried; a capture failure is the
// microphone's (permission, no device, no encoder) and never retried.
export type VoiceFailurePhase = 'ticket' | 'connect' | 'capture';

export class VoiceError extends Error {
  readonly phase: VoiceFailurePhase;
  constructor(phase: VoiceFailurePhase, message: string) {
    super(message);
    this.name = 'VoiceError';
    this.phase = phase;
  }
}

export interface VoiceCloseInfo {
  // The close code, or null for a connection lost without one.
  code: number | null;
  reason: string;
}

export interface VoiceSessionOptions {
  url: string;
  ticket: Uint8Array;
  // VOICE_HELLO_FLAG_* bitmap.
  helloFlags: number;
  init: WebTransportOptions;
  signal?: AbortSignal;
}

export interface VoiceSessionCallbacks {
  onState: (state: VoiceStateMessage) => void;
  onFrame: (header: VoiceFrameHeader, payload: Uint8Array) => void;
  onClosed: (info: VoiceCloseInfo) => void;
}

// The narrow surface the controller drives (a fake in its tests).
export interface VoiceSessionLike {
  start(): Promise<void>;
  sendFrame(header: VoiceFrameHeader, payload: Uint8Array): void;
  sendControl(flags: number): void;
  close(): void;
}

// The read loop and wt.closed settle in unspecified order; only wt.closed
// carries the code. The loop's end waits this long for it.
export const CLOSE_INFO_GRACE_MS = 250;
// A voice frame older than this when its turn comes is dropped: late voice is
// worse than lost voice (Mumble conceals a lost 10 ms frame).
const VOICE_SEND_MAX_AGE_MS = 100;

export class VoiceSession implements VoiceSessionLike {
  private readonly opts: VoiceSessionOptions;
  private readonly cb: VoiceSessionCallbacks;
  private wt: WebTransport | null = null;
  private writer: WritableStreamDefaultWriter<Uint8Array> | null = null;
  private sender: DatagramSender | null = null;
  private writeChain: Promise<void> = Promise.resolve();
  private readonly abort = new AbortController();
  private noticedCode: number | null = null;
  private settled = false;
  private closed = false;

  constructor(opts: VoiceSessionOptions, cb: VoiceSessionCallbacks) {
    this.opts = opts;
    this.cb = cb;
  }

  // Dials, opens the stream and sends the hello. Rejects with a 'connect'
  // VoiceError after releasing everything it opened.
  async start(): Promise<void> {
    let wt: WebTransport;
    try {
      wt = await dialWebTransport(this.opts.url, this.opts.init, this.opts.signal);
    } catch (e) {
      throw new VoiceError('connect', e instanceof Error ? e.message : String(e));
    }
    this.wt = wt;
    if (this.closed) {
      this.release();
      throw new VoiceError('connect', 'voice session closed while connecting');
    }
    let stream: WebTransportBidirectionalStream;
    try {
      stream = await wt.createBidirectionalStream();
      if (this.closed) throw new Error('voice session closed while connecting');
      this.writer = stream.writable.getWriter() as WritableStreamDefaultWriter<Uint8Array>;
      this.writeRecord(
        encodeVoiceHello({ protocol: VOICE_PROTOCOL_VERSION, flags: this.opts.helloFlags, ticket: this.opts.ticket }),
      );
      this.sender = new DatagramSender(wt, { maxAgeMs: VOICE_SEND_MAX_AGE_MS });
    } catch (e) {
      this.release();
      throw new VoiceError('connect', e instanceof Error ? e.message : String(e));
    }

    const closeInfo: Promise<VoiceCloseInfo> = wt.closed.then(
      (info) => ({
        code: typeof info?.closeCode === 'number' ? info.closeCode : null,
        reason: info?.reason ?? '',
      }),
      (err: unknown) => {
        const e = err as { closeCode?: number; reason?: string; message?: string } | null;
        return {
          code: typeof e?.closeCode === 'number' ? e.closeCode : null,
          reason: e?.reason || e?.message || 'voice connection lost',
        };
      },
    );
    void closeInfo.then((info) => this.settle(info));
    void this.readRecords(stream.readable, closeInfo);
    void readDatagrams(wt, (d) => this.handleDatagram(d), this.abort.signal).catch(() => {
      // the record loop and wt.closed report the end
    });
    void readServerStreams(
      wt,
      {
        onKeyframe: () => {},
        onCarrierRecord: () => {},
        onSessionClosing: (code) => {
          this.noticedCode = code;
        },
      },
      newCarrierCounters(),
      this.abort.signal,
    ).catch(() => {
      // as above
    });
  }

  sendFrame(header: VoiceFrameHeader, payload: Uint8Array): void {
    const sender = this.sender;
    if (!sender || this.settled || this.closed) return;
    let dgram: Uint8Array<ArrayBuffer>;
    try {
      dgram = encodeVoiceFrame(header, payload);
    } catch (e) {
      log.warn('voice frame not sent:', e);
      return;
    }
    // A failed send means the session is going; wt.closed reports it.
    sender.send([dgram], { kind: 'audio' }).catch(() => {});
  }

  sendControl(flags: number): void {
    if (this.settled || this.closed) return;
    this.writeRecord(encodeVoiceControl(flags));
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.release();
  }

  private writeRecord(msg: Uint8Array): void {
    const w = this.writer;
    if (!w) return;
    const rec = encodeRoomRecord(msg);
    this.writeChain = this.writeChain
      .then(() => w.write(rec))
      .catch((e) => {
        if (!this.settled && !this.closed) log.info('voice control write failed:', e);
      });
  }

  private async readRecords(readable: ReadableStream<Uint8Array>, closeInfo: Promise<VoiceCloseInfo>): Promise<void> {
    const reader = readable.getReader();
    const records = new RoomRecordReader();
    let loopError: string | null = null;
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        if (!value || value.length === 0) continue;
        for (const rec of records.push(value)) this.handleRecord(rec);
      }
    } catch (e) {
      loopError = e instanceof Error ? e.message : String(e);
    } finally {
      try {
        reader.releaseLock();
      } catch {
        // already released
      }
    }
    if (this.settled || this.closed) return;
    const info = await Promise.race([
      closeInfo,
      new Promise<null>((resolve) => setTimeout(() => resolve(null), CLOSE_INFO_GRACE_MS)),
    ]);
    this.settle(info ?? { code: null, reason: loopError ?? 'voice stream ended' });
  }

  private handleRecord(rec: Uint8Array): void {
    if (this.settled || this.closed || rec.length < 2) return;
    if (rec[1] !== TYPE_VOICE_STATE) return; // unknown records are skipped
    try {
      this.cb.onState(parseVoiceState(rec));
    } catch (e) {
      // A malformed record: the length-prefixed stream cannot be trusted any
      // more. End the session; the controller treats it as a lost connection.
      log.warn('voice state unreadable; closing the voice session:', e instanceof WireError ? e.message : e);
      this.settle({ code: null, reason: 'malformed voice record' });
    }
  }

  private handleDatagram(d: Uint8Array): void {
    if (this.settled || this.closed || d.length < 2 || d[1] !== TYPE_VOICE_FRAME) return;
    let frame: { header: VoiceFrameHeader; payload: Uint8Array };
    try {
      frame = parseVoiceFrame(d);
    } catch {
      return; // one bad datagram costs one frame
    }
    this.cb.onFrame(frame.header, frame.payload);
  }

  private settle(info: VoiceCloseInfo): void {
    if (this.settled || this.closed) return;
    this.settled = true;
    this.release();
    this.cb.onClosed({ code: info.code ?? this.noticedCode, reason: info.reason });
  }

  // Shared teardown for every ending: success, failure mid-start, close().
  private release(): void {
    this.abort.abort();
    try {
      this.sender?.close();
    } catch {
      // the writer is gone
    }
    this.sender = null;
    try {
      this.writer?.releaseLock();
    } catch {
      // a pending write holds it; the session close ends it
    }
    this.writer = null;
    const wt = this.wt;
    this.wt = null;
    if (wt) {
      void (wt.closed as Promise<unknown> | undefined)?.catch(() => {});
      try {
        wt.close();
      } catch {
        // already closed
      }
    }
  }
}
