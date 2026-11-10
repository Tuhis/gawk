// The browser broadcaster's carrier lane (docs/88 BR2, R84): in Reliable
// mode a GOP's deltas ride one uni stream behind their keyframe instead of
// datagrams, so a burst or an outage on the broadcaster's uplink is
// retransmitted by QUIC instead of lost for every viewer.
//
// The GOP stream itself — its prologue and keyframe, the two-in-Chrome limit,
// the 2 s abort, the FIN at rotation — is KeyframeSender's (keyframe-sender.ts,
// docs/88 D2, D3): one F-12 state machine for keyframes in both modes. This
// lane is what rides behind the keyframe: each delta frame's records (byte for
// byte the datagrams datagram mode would send, parity included, docs/75 D1) as
// ONE write, a queue drained one write at a time so a retired GOP's records can
// still be shed, and the counters. The encoder never waits on it.
//
// What a GOP's deltas get (D3):
//   - its stream is opening or its keyframe is being written: they queue;
//   - its stream has taken the keyframe: they are written, in order;
//   - it has no stream (its keyframe waits in the slot, was replaced, failed or
//     was aborted) or it was retired (a newer keyframe exists): they are shed,
//     and once one delta of a GOP is shed the rest of that GOP follows it — a
//     later delta could not be decoded anyway.
// Nothing waits in gawk for longer than one GOP: a retired GOP's queue is shed.

import type { CarrierLane, KeyframeOutcome, KeyframeSender } from './keyframe-sender';
import { CAP_KEYFRAME_CARRIERS, CAP_UPLINK_CARRIERS } from './parity';
import { CARRIER_RECORD_HEADER_SIZE, encodeCarrierRecord } from './wire';

// The broadcaster's Delivery setting (docs/88 §7, D6), the native app's words
// (gawk-broadcast-desktop/crates/engine/src/carrier.rs `Delivery`). Stored and
// reported (`uplinkModeRequested`) as the native app's values.
export type Delivery = 'reliable' | 'lowest-latency';
export const DELIVERIES: readonly Delivery[] = ['reliable', 'lowest-latency'];

// The browser's default until BR0 and BR5 hold (docs/88 OD2, owner
// 2026-11-10). The one place it lives: a blank stored value follows it.
export const DEFAULT_DELIVERY: Delivery = 'lowest-latency';

export const DELIVERY_LABEL = 'Delivery';
export const DELIVERY_LABELS: Record<Delivery, string> = {
  reliable: 'Reliable',
  'lowest-latency': 'Lowest latency',
};
export const DELIVERY_HINTS: Record<Delivery, string> = {
  reliable: 'If your connection drops some video, viewers wait a moment for it instead of losing the picture.',
  'lowest-latency': 'Viewers never wait for dropped video. If your connection drops some, they see brief freezes.',
};

// How the current GOP's deltas leave (`uplinkMode`, docs/75 D8's words).
export type UplinkMode = 'datagram' | 'carrier';

// D1/D6's engage rule, the whole of it, decided per GOP when its keyframe is
// produced: the relay advertised both carrier bits on this session (null:
// its capabilities have not arrived, so the GOP goes as datagrams, as
// natively), the broadcaster chose Reliable, and the browser is one OD4
// allows. No automatic switching.
export function engageCarrier(relayFlags: number | null, delivery: Delivery, chromium: boolean): UplinkMode {
  const both = CAP_UPLINK_CARRIERS | CAP_KEYFRAME_CARRIERS;
  if (relayFlags === null || (relayFlags & both) !== both) return 'datagram';
  return delivery === 'reliable' && chromium ? 'carrier' : 'datagram';
}

// OD4: Chromium only (Chrome, Edge and the other Chromium browsers) until
// Firefox and Safari each pass BR0-B. Chrome on iOS is WebKit and says CriOS.
export function isChromium(ua: string): boolean {
  return /\b(?:Headless)?Chrome\/\d+/.test(ua) && !/\bFirefox\//.test(ua);
}

// One frame's records: uint16 length ‖ datagram for each, concatenated, so a
// frame is one write.
export function frameRecords(datagrams: readonly Uint8Array[]): Uint8Array<ArrayBuffer> {
  let n = 0;
  for (const d of datagrams) n += CARRIER_RECORD_HEADER_SIZE + d.length;
  const out = new Uint8Array(n);
  let o = 0;
  for (const d of datagrams) {
    out.set(encodeCarrierRecord(d), o);
    o += CARRIER_RECORD_HEADER_SIZE + d.length;
  }
  return out;
}

export type CarrierSendResult = 'sent' | 'dropped';

interface Frame {
  bytes: Uint8Array<ArrayBuffer>;
  records: number;
  resolve: (r: CarrierSendResult) => void;
  reject: (e: Error) => void;
}

interface Gop {
  queue: Frame[];
  write: ((bytes: Uint8Array<ArrayBuffer>) => Promise<void>) | null;
  busy: boolean;
  done: boolean;
}

export class CarrierSender implements CarrierLane {
  // recordsShed: records (datagrams) dropped unwritten, docs/75 D8's
  // uplinkRecordsShed. recordsSent / framesSent: handed to the browser.
  readonly counters = { recordsShed: 0, recordsSent: 0, framesSent: 0 };

  private readonly keyframes: KeyframeSender;
  private gops = new Map<number, Gop>();
  // The GOP whose deltas are being shed: once one is, the rest follow.
  private sheddingGop: number | null = null;

  constructor(keyframes: KeyframeSender) {
    this.keyframes = keyframes;
    keyframes.setCarrierLane(this);
  }

  // Queues one delta frame's datagrams for its GOP's stream. Resolves 'sent'
  // once its write was handed to the browser, 'dropped' when it was shed;
  // rejects only when the session went away with it still queued.
  send(gop: number, datagrams: readonly Uint8Array[]): Promise<CarrierSendResult> {
    if (gop === this.sheddingGop || this.keyframes.carrierState(gop) === 'gone') {
      this.sheddingGop = gop;
      this.counters.recordsShed += datagrams.length;
      return Promise.resolve('dropped');
    }
    const g = this.gop(gop);
    return new Promise((resolve, reject) => {
      g.queue.push({ bytes: frameRecords(datagrams), records: datagrams.length, resolve, reject });
      this.pump(gop, g);
    });
  }

  opened(gop: number, write: (bytes: Uint8Array<ArrayBuffer>) => Promise<void>): void {
    const g = this.gop(gop);
    g.write = write;
    this.pump(gop, g);
  }

  retired(gop: number): void {
    this.close(gop);
  }

  ended(gop: number, _outcome: KeyframeOutcome): void {
    this.close(gop);
  }

  reset(): void {
    const gone = new Error('session ended');
    for (const g of this.gops.values()) {
      for (const f of g.queue.splice(0)) f.reject(gone);
      g.write = null;
      g.done = true;
    }
    this.gops.clear();
  }

  private gop(gop: number): Gop {
    let g = this.gops.get(gop);
    if (!g) {
      g = { queue: [], write: null, busy: false, done: false };
      this.gops.set(gop, g);
    }
    return g;
  }

  // Nothing more of this GOP is written: what is queued is shed; a write in
  // progress finishes (the browser has the bytes).
  private close(gop: number): void {
    const g = this.gops.get(gop);
    if (!g) return;
    g.done = true;
    g.write = null;
    for (const f of g.queue.splice(0)) {
      this.counters.recordsShed += f.records;
      f.resolve('dropped');
    }
    if (!g.busy) this.gops.delete(gop);
  }

  private pump(gop: number, g: Gop): void {
    if (g.busy || !g.write || g.queue.length === 0) return;
    const f = g.queue.shift()!;
    g.busy = true;
    g.write(f.bytes).then(
      () => {
        this.counters.recordsSent += f.records;
        this.counters.framesSent++;
        f.resolve('sent');
      },
      () => {
        // The stream failed or was aborted under it: KeyframeSender ends it,
        // which sheds the rest.
        this.counters.recordsShed += f.records;
        f.resolve('dropped');
      },
    ).finally(() => {
      g.busy = false;
      if (g.done) {
        if (this.gops.get(gop) === g) this.gops.delete(gop);
        return;
      }
      this.pump(gop, g);
    });
  }
}
