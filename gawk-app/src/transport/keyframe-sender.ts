// The browser broadcaster's keyframe streams (docs/75 RU9): the engine's F-12
// rule (gawk-broadcast-desktop/crates/engine/src/sender.rs, docs/38 F-12)
// with the browser's two clocks. A keyframe stream is
//
//   opening     createUnidirectionalStream() not yet resolved — not in
//               flight: it makes nothing wait and counts toward no limit;
//   written     open, its write() not yet resolved;
//   draining    handed over (write() resolved), not yet acknowledged
//               (close() resolves on acknowledgement);
//
// and at most two are in Chrome at once — one being written, one draining. A
// new keyframe starts if nothing is being written and fewer than two are in
// Chrome; otherwise it waits in a single pending slot, replacing any older
// waiter, and starts as soon as that holds again — on a handover, an
// acknowledgement or a failure. Everything is driven by those events and by
// keyframes being produced, never by a timer.
//
// A keyframe is never dropped for age: a waiter gives way only to a newer
// keyframe, which loses nothing. A stream is abandoned only when a newer
// keyframe arrives while it is KEYFRAME_ABORT_AGE_MS past its open and still
// unacknowledged — the engine first cancelled any write that outlived its
// GOP and livelocked in the field, every write dying at ~90 % while joining
// viewers saw black (F-12).
//
// The next keyframe starts on the previous one's handover, not its
// acknowledgement, so a long path's round trip does not hold it back. Which
// GOPs' deltas must give way (deltasBlocked) follows the same two clocks.

import { log } from '../lib/logger';

// How long after it opened an unacknowledged keyframe stream is aborted, if a
// newer keyframe arrives (the engine's KEYFRAME_WRITE_CANCEL_AGE_MS).
export const KEYFRAME_ABORT_AGE_MS = 2000;

// How a keyframe left: acknowledged, failed on its own (opening, writing or
// closing rejected), aborted past the age above (also a failure: it did not
// finish), replaced in the slot by a newer waiter, or discarded from the slot
// when an abort made room for the newest one.
export type KeyframeOutcome = 'sent' | 'failed' | 'aborted' | 'replaced' | 'discarded';

// The slice of WritableStreamDefaultWriter a keyframe stream needs.
export interface KeyframeStreamWriter {
  write(chunk: Uint8Array<ArrayBuffer>): Promise<void>;
  close(): Promise<void>;
  abort(reason?: unknown): Promise<void>;
}

// Opens one outgoing unidirectional stream on the current session
// (`() => wt.createUnidirectionalStream()`).
export type OpenKeyframeStream = () => Promise<{ getWriter(): KeyframeStreamWriter }>;

export interface KeyframeSenderEvents {
  // The keyframe had to wait in the slot.
  waiting?(gop: number): void;
  // Its write() resolved.
  handedOver?(gop: number): void;
  // It left for good. `bytes` is its size, for the sent accounting.
  ended?(gop: number, outcome: KeyframeOutcome, bytes: number): void;
}

export interface KeyframeSenderOptions {
  now: () => number;
  // One GOP (keyframeIntervalMs): a keyframe not handed over within this of
  // its production is late.
  gopMs: number;
  abortAgeMs?: number;
  events?: KeyframeSenderEvents;
}

type Phase = 'waiting' | 'opening' | 'writing' | 'draining' | KeyframeOutcome;

interface Keyframe {
  gop: number;
  msg: Uint8Array<ArrayBuffer>;
  producedAt: number;
  phase: Phase;
  openedAt: number;
  writer: KeyframeStreamWriter | null;
}

// How many recent GOPs deltasBlocked() can answer for. A delta is never more
// than a GOP or two behind the newest keyframe.
const GOPS_REMEMBERED = 16;

export class KeyframeSender {
  readonly counters = { sent: 0, failed: 0, superseded: 0, bytesSent: 0 };

  private readonly now: () => number;
  private readonly gopMs: number;
  private readonly abortAgeMs: number;
  private readonly events: KeyframeSenderEvents;

  private open: OpenKeyframeStream | null = null;
  // Bumped on detach: settles from a previous session's streams are ignored.
  private generation = 0;
  private writing: Keyframe | null = null;
  private draining: Keyframe[] = [];
  private waiter: Keyframe | null = null;
  private byGop = new Map<number, Keyframe>();

  constructor(opts: KeyframeSenderOptions) {
    this.now = opts.now;
    this.gopMs = opts.gopMs;
    this.abortAgeMs = opts.abortAgeMs ?? KEYFRAME_ABORT_AGE_MS;
    this.events = opts.events ?? {};
  }

  // Points the sender at a (new) session.
  attach(open: OpenKeyframeStream): void {
    this.open = open;
    this.pump();
  }

  // Leaves the session. On a resume (`countUnfinished`) every keyframe not
  // yet acknowledged dies with the old session and counts as failed, and the
  // waiter is discarded and counted as superseded: the resume forces a fresh
  // keyframe, which starts at once on the new session — the engine's own
  // resume. On a stop nothing is counted.
  detach(countUnfinished: boolean): void {
    this.generation++;
    this.open = null;
    const unfinished = [...this.draining, ...(this.writing ? [this.writing] : [])];
    for (const kf of this.byGop.values()) if (kf.phase === 'opening') unfinished.push(kf);
    for (const kf of unfinished) {
      void kf.writer?.abort().catch(() => {});
      this.end(kf, 'failed', countUnfinished);
    }
    this.writing = null;
    this.draining = [];
    if (this.waiter) this.end(this.waiter, 'discarded', countUnfinished);
    this.waiter = null;
  }

  // A keyframe produced while there was no session to send it on: its GOP's
  // deltas cannot be decoded either. Nothing is counted — no stream existed.
  missed(gop: number): void {
    this.remember({ gop, msg: new Uint8Array(0), producedAt: this.now(), phase: 'discarded', openedAt: 0, writer: null });
  }

  // A keyframe was produced: the abort check first, then start or wait.
  produce(gop: number, msg: Uint8Array<ArrayBuffer>): void {
    const now = this.now();
    let aborted = false;
    for (const kf of [...this.draining, ...(this.writing ? [this.writing] : [])]) {
      if (now - kf.openedAt >= this.abortAgeMs) {
        this.abort(kf);
        aborted = true;
      }
    }
    if (aborted && this.waiter) {
      this.end(this.waiter, 'discarded', true);
      this.waiter = null;
    }

    const kf: Keyframe = { gop, msg, producedAt: now, phase: 'waiting', openedAt: 0, writer: null };
    this.remember(kf);
    if (this.canStart()) {
      this.start(kf);
      return;
    }
    if (this.waiter) this.end(this.waiter, 'replaced', true);
    this.waiter = kf;
    this.events.waiting?.(gop);
  }

  // Whether a GOP's deltas must give way to its keyframe, judged when a
  // delta's turn comes: the keyframe did not go out (no viewer can decode
  // the GOP, and its deltas would take the link the next keyframe needs), or
  // it waits behind a late keyframe — one not handed over within one GOP of
  // its own production — or behind the two-stream limit. A brief wait behind
  // an on-time keyframe (one forced by a quality change, say) costs nothing.
  deltasBlocked(gop: number): boolean {
    const kf = this.byGop.get(gop);
    if (!kf) return false;
    switch (kf.phase) {
      case 'failed':
      case 'aborted':
      case 'replaced':
      case 'discarded':
        return true;
      case 'waiting':
        return (
          this.inChrome() >= 2 ||
          (this.writing !== null && this.now() - this.writing.producedAt > this.gopMs)
        );
      default:
        return false;
    }
  }

  private inChrome(): number {
    return this.draining.length + (this.writing ? 1 : 0);
  }

  private canStart(): boolean {
    return this.open !== null && this.writing === null && this.inChrome() < 2;
  }

  private remember(kf: Keyframe): void {
    this.byGop.set(kf.gop, kf);
    for (const g of this.byGop.keys()) {
      if (this.byGop.size <= GOPS_REMEMBERED) break;
      this.byGop.delete(g);
    }
  }

  private start(kf: Keyframe): void {
    kf.phase = 'opening';
    const gen = this.generation;
    let opening: ReturnType<OpenKeyframeStream>;
    try {
      opening = this.open!();
    } catch (e) {
      opening = Promise.reject(e);
    }
    opening.then(
      (stream) => this.onOpened(kf, stream.getWriter(), gen),
      (e: unknown) => {
        if (gen !== this.generation || kf.phase !== 'opening') return;
        this.fail(kf, e);
      },
    );
  }

  private onOpened(kf: Keyframe, writer: KeyframeStreamWriter, gen: number): void {
    if (gen !== this.generation || kf.phase !== 'opening') {
      // A previous session's stream, or one torn down while opening: it
      // carries nothing yet, so release it quietly.
      void writer.abort().catch(() => {});
      return;
    }
    kf.writer = writer;
    kf.openedAt = this.now();
    // Newest registration wins: a stream that opens becomes the one being
    // written, aborting any other still being written (the engine's
    // supersede-at-register).
    if (this.writing) this.abort(this.writing);
    if (this.draining.length >= 2) {
      // Two already drain (both started while this one was opening): the
      // limit holds, so this keyframe goes back to waiting — its empty
      // stream is released, it loses nothing — unless a newer one already
      // waits.
      void writer.abort().catch(() => {});
      kf.writer = null;
      if (this.waiter) {
        this.end(kf, 'replaced', true);
      } else {
        kf.phase = 'waiting';
        this.waiter = kf;
        this.events.waiting?.(kf.gop);
      }
      return;
    }
    kf.phase = 'writing';
    this.writing = kf;
    writer.write(kf.msg).then(
      () => this.onHandedOver(kf, gen),
      (e: unknown) => this.onStreamError(kf, e, gen),
    );
  }

  private onHandedOver(kf: Keyframe, gen: number): void {
    if (gen !== this.generation || kf.phase !== 'writing') return;
    kf.phase = 'draining';
    this.writing = null;
    this.draining.push(kf);
    this.events.handedOver?.(kf.gop);
    kf.writer!.close().then(
      () => this.onAcknowledged(kf, gen),
      (e: unknown) => this.onStreamError(kf, e, gen),
    );
    this.pump();
  }

  private onAcknowledged(kf: Keyframe, gen: number): void {
    if (gen !== this.generation || kf.phase !== 'draining') return;
    this.leaveChrome(kf);
    this.end(kf, 'sent', true);
    this.pump();
  }

  // A write or close rejecting other than by our own abort (an aborted
  // stream is no longer writing or draining, so its rejections land here
  // and are ignored).
  private onStreamError(kf: Keyframe, e: unknown, gen: number): void {
    if (gen !== this.generation || (kf.phase !== 'writing' && kf.phase !== 'draining')) return;
    this.leaveChrome(kf);
    this.fail(kf, e);
  }

  private fail(kf: Keyframe, e: unknown): void {
    log.warn('Keyframe stream send failed:', e);
    this.end(kf, 'failed', true);
    this.pump();
  }

  // RST_STREAM. Counts as superseded and, being unfinished, as failed — as
  // the engine counts a cancelled write.
  private abort(kf: Keyframe): void {
    this.leaveChrome(kf);
    void kf.writer?.abort().catch(() => {});
    this.end(kf, 'aborted', true);
  }

  private leaveChrome(kf: Keyframe): void {
    if (this.writing === kf) this.writing = null;
    this.draining = this.draining.filter((d) => d !== kf);
  }

  private end(kf: Keyframe, outcome: KeyframeOutcome, count: boolean): void {
    kf.phase = outcome;
    if (count) {
      switch (outcome) {
        case 'sent':
          this.counters.sent++;
          this.counters.bytesSent += kf.msg.length;
          break;
        case 'failed':
          this.counters.failed++;
          break;
        case 'aborted':
          this.counters.superseded++;
          this.counters.failed++;
          break;
        case 'replaced':
        case 'discarded':
          this.counters.superseded++;
          break;
      }
      this.events.ended?.(kf.gop, outcome, kf.msg.length);
    }
  }

  // Starts the waiter if nothing is being written and fewer than two are in
  // Chrome.
  private pump(): void {
    if (!this.waiter || !this.canStart()) return;
    const kf = this.waiter;
    this.waiter = null;
    this.start(kf);
  }
}
