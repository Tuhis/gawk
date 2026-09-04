// R28 TM2 (docs/33): the collector's four load-bearing properties — zero PII,
// never on the hot path, off means off, and a truncated session that says so.

import { beforeEach, describe, expect, it, vi } from 'vitest';

import {
  describeClient,
  MAX_PENDING_EVENTS,
  MAX_PENDING_SAMPLES,
  MAX_SEND_ATTEMPTS,
  SESSION_BYTE_BUDGET,
  TelemetryCollector,
  type TelemetryBatch,
} from './telemetry';
import type { TelemetryHelloMessage } from '../transport/wire';

const HELLO: TelemetryHelloMessage = {
  enabled: true,
  reportIntervalMs: 2000,
  token: '00012345000102030405060708090a0ba1a2a3a4a5a6a7a8',
  broadcastKey: '1a2b3c4d5e6f',
};

interface Sent {
  url: string;
  body: string;
  beacon: boolean;
}

// A collector wired to fake time and a recording transport. Timers are driven
// explicitly so nothing here waits on a wall clock.
function harness(opts: { fail?: boolean; reportIntervalMs?: number; retryable?: boolean } = {}) {
  const sent: Sent[] = [];
  const timers: { fn: () => void; ms: number }[] = [];
  let clock = 0;
  const nowSpy = vi.spyOn(performance, 'now').mockImplementation(() => clock);

  const collector = new TelemetryCollector<Record<string, unknown>>({
    url: '/api/telemetry/v1/ingest',
    role: 'viewer',
    now: () => 1_700_000_000_000,
    transport: async (url, body, beacon) => {
      sent.push({ url, body, beacon });
      if (!opts.fail) return true;
      return opts.retryable === undefined ? false : { ok: false, retryable: opts.retryable };
    },
    setTimer: (fn, ms) => {
      timers.push({ fn, ms });
      return timers.length - 1;
    },
    clearTimer: () => {},
    app: { browser: 'Chrome 152', os: 'Windows' },
  });

  return {
    collector,
    sent,
    timers,
    nowSpy,
    advance: (ms: number) => {
      clock += ms;
    },
    // Run every timer scheduled so far, once.
    runTimers: () => {
      const pending = timers.splice(0, timers.length);
      for (const t of pending) t.fn();
    },
    batches: () => sent.map((s) => JSON.parse(s.body) as TelemetryBatch<Record<string, unknown>>),
    begin: (hello: TelemetryHelloMessage = HELLO) =>
      collector.begin({ ...hello, reportIntervalMs: opts.reportIntervalMs ?? hello.reportIntervalMs }),
  };
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe('TelemetryCollector — off means off', () => {
  it('makes zero network requests without a hello', () => {
    const h = harness();
    h.collector.sample({ fps: 30 });
    h.collector.event('reconnect');
    h.collector.flush(true);
    h.collector.finish();
    expect(h.sent).toEqual([]);
    expect(h.collector.active).toBe(false);
  });

  it('makes zero network requests when the fleet reports enabled: false', () => {
    const h = harness();
    h.collector.begin({ ...HELLO, enabled: false });
    h.collector.sample({ fps: 30 });
    h.collector.flush(true);
    expect(h.sent).toEqual([]);
    expect(h.collector.active).toBe(false);
    // And no timer was armed — a disabled collector is inert, not a queue
    // waiting to drain into a void.
    expect(h.timers).toHaveLength(0);
  });

  it('goes inert and sends nothing when a later hello disables collection', () => {
    const h = harness();
    h.begin();
    h.advance(3000);
    h.collector.sample({ fps: 30 });
    h.collector.begin({ ...HELLO, enabled: false });
    h.collector.flush(true);
    expect(h.sent).toEqual([]);
  });
});

// docs/33 §4.13: the id a viewer reads off its own stats overlay. The property
// that matters is not the derivation (wire.test.ts pins that) but the gating —
// the collector names a session only while it is actually reporting one, so an
// operator handed an id can always expect a dashboard row for it.
describe('TelemetryCollector — sessionId (§4.13)', () => {
  it('names the session it is reporting under, and nothing before or after', () => {
    const h = harness();
    expect(h.collector.sessionId).toBeNull();

    h.begin();
    // hex(nonce): the middle 12 bytes of HELLO's token, and the same value the
    // relay recorded for this session.
    expect(h.collector.sessionId).toBe('000102030405060708090a0b');

    // A reconnect is a new relay session with a new token — and a new row.
    h.collector.begin({ ...HELLO, token: '0009abcd' + 'aabbccddeeff001122334455' + '9d4e7750cdf69a2b' });
    expect(h.collector.sessionId).toBe('aabbccddeeff001122334455');

    h.collector.finish();
    expect(h.collector.sessionId).toBeNull();
  });

  it('names no session on a fleet that collects nothing', () => {
    const h = harness();
    // The hello is well-formed and its token parses — but nothing is being
    // collected, so there is no row to point an operator at.
    h.collector.begin({ ...HELLO, enabled: false });
    expect(h.collector.sessionId).toBeNull();
    expect(h.collector.active).toBe(false);
  });

  it('collects anyway when a token cannot be named (D9)', () => {
    const h = harness();
    // Unreachable from the wire (parseTelemetryHello enforces 24 bytes), so
    // this pins the choice rather than a real case: telemetry must never break
    // a stream, least of all over a display detail.
    h.collector.begin({ ...HELLO, token: 'abcd' });
    expect(h.collector.sessionId).toBeNull();
    expect(h.collector.active).toBe(true);
    h.advance(3000);
    h.collector.sample({ fps: 30 });
    h.collector.flush(false);
    expect(h.sent).toHaveLength(1);
  });
});

describe('TelemetryCollector — zero PII (D8)', () => {
  it('carries the obfuscated key and coarse client class, never a raw UA', () => {
    const h = harness();
    h.begin();
    h.advance(3000);
    h.collector.sample({ fps: 30 });
    h.collector.flush(false);

    expect(h.sent).toHaveLength(1);
    const body = h.sent[0].body;
    const batch = h.batches()[0];
    expect(batch.broadcastKey).toBe('1a2b3c4d5e6f');
    expect(batch.app.browser).toBe('Chrome 152');
    expect(batch.app.os).toBe('Windows');
    expect(batch.role).toBe('viewer');
    // Asserted by grepping the SERIALIZED body, per TM2's criteria — a field
    // added later that smuggles the UA in would fail here even if the typed
    // envelope still looks clean.
    expect(body).not.toContain('Mozilla/');
    expect(body).not.toContain('AppleWebKit');
    expect(body.toLowerCase()).not.toContain('useragent');
  });

  // The raw broadcast ID is something a viewer obviously knows (it is in the
  // URL). What stops it being reported is that the collector only ever sends
  // the obfuscated key the relay handed it — there is no code path that reads
  // the ID at all.
  it('never sends a raw broadcast ID even when one is in scope', () => {
    const h = harness();
    h.begin();
    h.advance(3000);
    h.collector.sample({ fps: 30, note: 'nothing identifying here' });
    h.collector.flush(false);
    expect(h.sent[0].body).not.toContain('K7XQ2M');
    expect(h.batches()[0].broadcastKey).toHaveLength(12);
  });

  it('reduces user agents to a browser family and OS, and never guesses', () => {
    expect(
      describeClient(
        'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36',
      ),
    ).toEqual({ browser: 'Chrome 152', os: 'Windows' });
    // Every Chromium browser also says "Chrome"; Edge/Opera must win.
    expect(
      describeClient(
        'Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0',
      ).browser,
    ).toBe('Edge 152');
    expect(
      describeClient('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/26.4 Safari/605.1.15'),
    ).toEqual({ browser: 'Safari 26', os: 'macOS' });
    expect(
      describeClient('Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 Version/26.4 Mobile/15E148 Safari/604.1'),
    ).toEqual({ browser: 'Safari 26', os: 'iOS' });
    expect(describeClient('Mozilla/5.0 (X11; Linux x86_64; rv:145.0) Gecko/20100101 Firefox/145.0')).toEqual({
      browser: 'Firefox 145',
      os: 'Linux',
    });
    // Headless Chrome says "HeadlessChrome/", never "Chrome/" — a bare Chrome
    // rule reports a real, shipping browser as "unknown", which is exactly how
    // the e2e harness's own browser looked until it was measured. Headlessness
    // is not a browser family.
    expect(
      describeClient(
        'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/141.0.0.0 Safari/537.36',
      ),
    ).toEqual({ browser: 'Chrome 141', os: 'macOS' });
    expect(describeClient('something nobody has ever shipped')).toEqual({
      browser: 'unknown',
      os: 'unknown',
    });
  });
});

describe('TelemetryCollector — sampling and batching', () => {
  it('decimates to the relay-requested cadence rather than the stats tick', () => {
    const h = harness({ reportIntervalMs: 2000 });
    h.begin();
    // Twenty 500 ms stats ticks over 10 s ⇒ ~5 samples at a 2 s cadence.
    for (let i = 0; i < 20; i++) {
      h.collector.sample({ fps: 30, i });
      h.advance(500);
    }
    h.collector.flush(false);
    const batch = h.batches()[0];
    expect(batch.samples.length).toBeGreaterThanOrEqual(4);
    expect(batch.samples.length).toBeLessThanOrEqual(6);
    // Timestamps are rebased to the session start, monotonic.
    expect(batch.samples[0].tMs).toBe(0);
    for (let i = 1; i < batch.samples.length; i++) {
      expect(batch.samples[i].tMs).toBeGreaterThan(batch.samples[i - 1].tMs);
    }
  });

  it('numbers batches so a dropped one is a visible gap, not a silent hole', () => {
    const h = harness();
    h.begin();
    for (const i of [0, 1, 2]) {
      h.advance(3000);
      h.collector.sample({ fps: 30, i });
      h.collector.flush(false);
    }
    expect(h.batches().map((b) => b.seq)).toEqual([0, 1, 2]);
  });

  it('carries events even when no sample is due', () => {
    const h = harness();
    h.begin();
    h.collector.event('reconnect', 'close 4002');
    h.collector.flush(false);
    const batch = h.batches()[0];
    expect(batch.samples).toEqual([]);
    expect(batch.events).toEqual([{ tMs: 0, kind: 'reconnect', detail: 'close 4002' }]);
  });

  // A plain `.slice(0, n)` truncates at a UTF-16 CODE UNIT boundary, which
  // can land inside a surrogate pair (an emoji or other astral character) and
  // leave a lone high surrogate. That is not valid UTF-16 text — sendBeacon
  // and fetch both encode the request body to UTF-8, which silently rewrites
  // an unpaired surrogate to U+FFFD, so the stored value would be corrupted
  // rather than merely shortened (the same class of bug as the ingest
  // service's byte-boundary clip()). Built so the emoji's leading surrogate
  // lands exactly on the cut.
  it('truncates event fields on a UTF-16 boundary instead of splitting a surrogate pair', () => {
    const h = harness();
    h.begin();
    const kind = 'a'.repeat(63) + '😀' + 'zzzzzzzzzz';
    const detail = 'x'.repeat(255) + '😀' + 'zzzzzzzzzz';
    h.collector.event(kind, detail);
    h.collector.flush(false);
    const event = h.batches()[0].events[0];
    expect(event.kind).toBe('a'.repeat(63));
    expect(event.detail).toBe('x'.repeat(255));
  });

  it('skips an empty non-final flush entirely', () => {
    const h = harness();
    h.begin();
    h.collector.flush(false);
    expect(h.sent).toEqual([]);
  });

  it('sends a final batch on finish so the service need not wait for an idle timeout', () => {
    const h = harness();
    h.begin();
    h.collector.finish();
    expect(h.sent).toHaveLength(1);
    expect(h.batches()[0].final).toBe(true);
  });

  // hidden fires on a tab switch, not only on close. Marking that batch final
  // would finalize a session that is still streaming.
  it('flushes on unload via the beacon path WITHOUT claiming the session ended', () => {
    const h = harness();
    h.begin();
    h.advance(3000);
    h.collector.sample({ fps: 30 });
    h.collector.flushForUnload();
    expect(h.sent).toHaveLength(1);
    expect(h.sent[0].beacon).toBe(true);
    expect(h.batches()[0].final).toBe(false);
  });

  it('starts a fresh session on a reconnect hello and closes the previous one', () => {
    const h = harness();
    h.begin();
    h.advance(3000);
    h.collector.sample({ fps: 30 });
    const second: TelemetryHelloMessage = { ...HELLO, token: 'ff'.repeat(24) };
    h.collector.begin(second);

    expect(h.sent).toHaveLength(1);
    expect(h.batches()[0].final).toBe(true);
    expect(h.batches()[0].token).toBe(HELLO.token);

    h.advance(3000);
    h.collector.sample({ fps: 30 });
    h.collector.flush(false);
    const next = h.batches()[1];
    expect(next.token).toBe(second.token);
    // A new relay session is a new telemetry session: numbering restarts, so
    // two transport sessions never merge into one row.
    expect(next.seq).toBe(0);
  });

  it('bounds what it holds in memory, shedding the oldest to stay near live', () => {
    // The cadence has a 250 ms floor (a relay cannot ask for 0), so samples
    // are spaced past it rather than fighting the decimator.
    const h = harness({ reportIntervalMs: 250 });
    h.begin();
    for (let i = 0; i < MAX_PENDING_SAMPLES + 50; i++) {
      h.advance(300);
      h.collector.sample({ i });
    }
    for (let i = 0; i < MAX_PENDING_EVENTS + 50; i++) h.collector.event('tick', String(i));
    h.collector.flush(false);
    const batch = h.batches()[0];
    expect(batch.samples).toHaveLength(MAX_PENDING_SAMPLES);
    expect(batch.events).toHaveLength(MAX_PENDING_EVENTS);
    // Oldest shed: the newest sample survived.
    expect(batch.samples.at(-1)?.stats.i).toBe(MAX_PENDING_SAMPLES + 49);
  });
});

describe('TelemetryCollector — never degrades the stream (D9)', () => {
  it('retries a failed batch a few times, then drops it silently and keeps collecting', async () => {
    const h = harness({ fail: true });
    h.begin();
    h.advance(3000);
    h.collector.sample({ fps: 30 });
    h.collector.flush(false);

    // Drain the retry chain. Each failure schedules the next attempt from an
    // async continuation, so the microtask queue has to settle between ticks.
    for (let i = 0; i < 10; i++) {
      await Promise.resolve();
      await Promise.resolve();
      h.runTimers();
    }
    await Promise.resolve();

    expect(h.sent.length).toBe(MAX_SEND_ATTEMPTS);
    // Still collecting — a dead ingest must not stop the session's telemetry
    // from resuming when the endpoint comes back.
    expect(h.collector.active).toBe(true);
    h.advance(3000);
    h.collector.sample({ fps: 30 });
    h.collector.flush(false);
    expect(h.sent.length).toBeGreaterThan(MAX_SEND_ATTEMPTS);
  });

  it('never throws out of sample/event/flush when the transport explodes', () => {
    const collector = new TelemetryCollector<Record<string, unknown>>({
      url: '/x',
      role: 'viewer',
      transport: () => {
        throw new Error('network is on fire');
      },
      setTimer: () => 0,
      clearTimer: () => {},
    });
    collector.begin(HELLO);
    expect(() => {
      collector.sample({ fps: 30 });
      collector.event('boom');
      collector.flush(true);
      collector.finish();
    }).not.toThrow();
  });

  it('drops a batch that will not serialize instead of retrying forever', () => {
    const h = harness();
    h.begin();
    const cyclic: Record<string, unknown> = {};
    cyclic.self = cyclic;
    h.advance(3000);
    h.collector.sample(cyclic);
    h.collector.flush(false);
    expect(h.sent).toEqual([]);
  });
});

describe('TelemetryCollector — byte budget (§4.3)', () => {
  it('degrades to events-only on exhaustion and marks the session truncated', () => {
    const h = harness({ reportIntervalMs: 250 });
    h.begin();
    // One fat sample per flush until the budget is gone.
    const fat = { blob: 'x'.repeat(64 * 1024) };
    let guard = 0;
    while (!h.collector.truncatedSession && guard++ < 200) {
      h.advance(300);
      h.collector.sample(fat);
      h.collector.flush(false);
    }
    expect(h.collector.truncatedSession).toBe(true);
    expect(h.sent.length).toBeGreaterThan(SESSION_BYTE_BUDGET / (65 * 1024) - 2);

    // Crossing the budget must reach the WIRE, not just a private flag:
    // samples stop, so without an explicit signal every later flush would be
    // empty and the truncation would never be sent at all — leaving a clipped
    // session indistinguishable from one that simply ended.
    h.advance(5000);
    h.collector.sample({ fps: 30 });
    h.collector.flush(false);
    const after = h.batches().at(-1)!;
    expect(after.truncated).toBe(true);
    expect(after.samples).toEqual([]);
    expect(after.events.map((e) => e.kind)).toContain('telemetry-budget-exhausted');

    // Events keep flowing afterwards; they are what carries the story.
    h.advance(5000);
    h.collector.event('reconnect', 'close 4002');
    h.collector.flush(false);
    const later = h.batches().at(-1)!;
    expect(later.truncated).toBe(true);
    expect(later.samples).toEqual([]);
    expect(later.events.map((e) => e.kind)).toEqual(['reconnect']);
  });
});

describe('TelemetryCollector — retry semantics', () => {
  // 429 means "later"; every other rejection means "never" (review finding 2).
  // Retrying a batch the service will reject identically forever is pure noise
  // at exactly the moment the fleet is busiest.
  it('does not retry a rejection the service will repeat', async () => {
    const h = harness({ fail: true, retryable: false });
    h.begin();
    h.advance(3000);
    h.collector.sample({ fps: 30 });
    h.collector.flush(false);

    for (let i = 0; i < 10; i++) {
      await Promise.resolve();
      await Promise.resolve();
      h.runTimers();
    }
    await Promise.resolve();

    expect(h.sent.length).toBe(1);
    // And the session keeps collecting: one bad batch is not a dead session.
    expect(h.collector.active).toBe(true);
  });

  it('retries a rejection the service says is transient', async () => {
    const h = harness({ fail: true, retryable: true });
    h.begin();
    h.advance(3000);
    h.collector.sample({ fps: 30 });
    h.collector.flush(false);

    for (let i = 0; i < 10; i++) {
      await Promise.resolve();
      await Promise.resolve();
      h.runTimers();
    }
    await Promise.resolve();

    expect(h.sent.length).toBe(MAX_SEND_ATTEMPTS);
  });
});

describe('TelemetryCollector — interval minima (docs/33 D16)', () => {
  it('carries the worst tick across a decimation gap', () => {
    const h = harness({ reportIntervalMs: 2000 });
    h.begin();
    // Four 500 ms ticks per emitted sample. The collapse happens on a tick
    // that decimation discards — which is exactly how an intermittent stutter
    // used to become invisible before it ever left the browser.
    h.collector.sample({ receivedFps: 30 });
    h.advance(500);
    h.collector.sample({ receivedFps: 2 });
    h.advance(500);
    h.collector.sample({ receivedFps: 30 });
    h.advance(500);
    h.collector.sample({ receivedFps: 30 });
    h.advance(500);
    h.collector.sample({ receivedFps: 30 });
    h.collector.flush(false);

    const samples = h.batches()[0].samples;
    // The second emitted sample covers the interval containing the collapse.
    const withMin = samples.find(
      (s) => (s.stats as Record<string, unknown>).intervalMin !== undefined,
    );
    expect(withMin).toBeDefined();
    expect((withMin!.stats as Record<string, Record<string, number>>).intervalMin.receivedFps).toBe(
      2,
    );
    // And the sample's own reading is untouched: minima inform the detector,
    // they never replace the value a median is computed from.
    expect((withMin!.stats as Record<string, number>).receivedFps).toBe(30);
  });

  it('omits intervalMin when nothing between samples was worse', () => {
    const h = harness({ reportIntervalMs: 2000 });
    h.begin();
    for (let i = 0; i < 8; i++) {
      h.collector.sample({ receivedFps: 30 });
      h.advance(500);
    }
    h.collector.flush(false);
    for (const s of h.batches()[0].samples) {
      expect((s.stats as Record<string, unknown>).intervalMin).toBeUndefined();
    }
  });

  it('resets the running minimum after each emitted sample', () => {
    const h = harness({ reportIntervalMs: 2000 });
    h.begin();
    h.collector.sample({ receivedFps: 30 });
    h.advance(500);
    h.collector.sample({ receivedFps: 2 }); // discarded tick, dip recorded
    h.advance(1500);
    h.collector.sample({ receivedFps: 30 }); // emitted, carries the dip
    h.advance(2000);
    h.collector.sample({ receivedFps: 30 }); // emitted, must be clean again
    h.collector.flush(false);

    const samples = h.batches()[0].samples;
    const last = samples[samples.length - 1].stats as Record<string, unknown>;
    expect(last.intervalMin).toBeUndefined();
  });

  it('ignores non-finite and non-numeric readings', () => {
    const h = harness({ reportIntervalMs: 2000 });
    h.begin();
    h.collector.sample({ receivedFps: 30 });
    h.advance(500);
    h.collector.sample({ receivedFps: Number.NaN });
    h.advance(500);
    h.collector.sample({ receivedFps: '2' as unknown as number });
    h.advance(1000);
    h.collector.sample({ receivedFps: 30 });
    h.collector.flush(false);

    for (const s of h.batches()[0].samples) {
      const min = (s.stats as Record<string, Record<string, number> | undefined>).intervalMin;
      if (min !== undefined) {
        expect(Number.isFinite(min.receivedFps)).toBe(true);
      }
    }
  });
});

// --- R37 (docs/40 §4.10 SP13): the relay-advertised destination ------------

describe('advertised telemetry endpoint (R37, revised per review R3-A/R3-C)', () => {
  it('an advertised URL wins over the configured one (D15)', () => {
    const h = harness();
    h.begin();
    h.collector.setAdvertisedUrl('https://foreign.example/api/telemetry/v1/ingest');
    h.collector.sample({ fps: 30 });
    h.collector.flush();
    expect(h.sent.at(-1)!.url).toBe('https://foreign.example/api/telemetry/v1/ingest');
  });

  // The revised D15 fallback: with no 0x12, batches go to the CONFIGURED
  // URL on any relay — the pre-R37 behavior. The same fleet is legitimately
  // reached through non-default URLs (direct IP, alternate DNS, a migrated
  // legacy setting) where the configured collector shares the key; the
  // suppression variant silently stopped those sessions' telemetry (the
  // e2e failure that proved it).
  it('falls back to the configured URL when no 0x12 arrives', () => {
    const h = harness();
    h.begin();
    h.collector.sample({ fps: 30 });
    h.collector.flush();
    expect(h.sent).toHaveLength(1);
    expect(h.sent[0].url).toBe('/api/telemetry/v1/ingest');
  });

  // G2: precedence re-resolves at every send — a late 0x12 redirects the
  // rest of the session's batches (already-sent ones went to the fallback).
  it('a late 0x12 redirects subsequent batches', () => {
    const h = harness();
    h.begin();
    h.collector.sample({ fps: 30 });
    h.collector.flush();
    expect(h.sent[0].url).toBe('/api/telemetry/v1/ingest');
    h.collector.setAdvertisedUrl('https://foreign.example/ingest');
    h.advance(3000); // past the sampling floor so the next sample lands
    h.collector.sample({ fps: 31 });
    h.collector.flush();
    expect(h.sent[1].url).toBe('https://foreign.example/ingest');
  });

  it('reportingToAdvertised backs the indicator disclosure (D16)', () => {
    const h = harness();
    h.begin();
    expect(h.collector.reportingToAdvertised).toBe(false);
    h.collector.setAdvertisedUrl('https://foreign.example/ingest');
    expect(h.collector.reportingToAdvertised).toBe(true);
  });

  // G3: the 250 ms sampling floor is security-load-bearing since R37 — the
  // hello's interval is relay-chosen and the destination can be a hostile
  // relay's choice, so the clamp is what bounds the reflector rate. A relay
  // asking for 0 ms must not sample faster than the floor.
  it('clamps a below-floor hello interval to 250 ms', () => {
    const h = harness({ reportIntervalMs: 0 });
    h.begin();
    h.collector.sample({ fps: 1 });
    // A second sample immediately after must be coalesced by the floor.
    h.collector.sample({ fps: 2 });
    h.collector.flush();
    expect(h.sent).toHaveLength(1);
    const batch = JSON.parse(h.sent[0].body) as { samples: unknown[] };
    expect(batch.samples).toHaveLength(1);
  });
});

// D17: the unload beacon rides a CORS-safelisted content type so sendBeacon
// needs no preflight it cannot perform during unload — cross-origin included.
describe('beacon content type (R37 D17)', () => {
  it('sendBeacon is handed a text/plain blob', async () => {
    const calls: Array<{ url: string; type: string }> = [];
    const nav = navigator as unknown as Record<string, unknown>;
    const original = nav.sendBeacon;
    nav.sendBeacon = (url: string, blob: Blob) => {
      calls.push({ url, type: blob.type });
      return true;
    };
    try {
      const collector = new TelemetryCollector<Record<string, unknown>>({
        url: '/api/telemetry/v1/ingest',
        role: 'viewer',
      });
      collector.begin(HELLO);
      collector.sample({ fps: 1 });
      collector.flushForUnload();
      await Promise.resolve();
      expect(calls).toHaveLength(1);
      expect(calls[0].type.toLowerCase()).toBe('text/plain;charset=utf-8');
    } finally {
      if (original === undefined) delete nav.sendBeacon;
      else nav.sendBeacon = original;
    }
  });
});

// R42 (docs/44 §4.10, RM8): the HMAC'd room key rides every batch while set,
// and only the key — the code never enters the collector at all.
describe('TelemetryCollector — room key (R42)', () => {
  it('stamps roomKey on every batch once set, in either order relative to begin()', () => {
    const h = harness();
    h.collector.setRoomKey('1a2b3c4d5e6f');
    h.begin();
    h.collector.event('x');
    h.collector.flush();
    expect(h.batches()[0].roomKey).toBe('1a2b3c4d5e6f');
    h.collector.event('y');
    h.collector.flush();
    expect(h.batches()[1].roomKey).toBe('1a2b3c4d5e6f');
    expect(h.sent.every((s) => !s.body.includes('AB2CD3'))).toBe(true);
  });

  it('is absent outside a room and after clearing', () => {
    const h = harness();
    h.begin();
    h.collector.event('x');
    h.collector.flush();
    expect('roomKey' in h.batches()[0]).toBe(false);
    h.collector.setRoomKey('1a2b3c4d5e6f');
    h.collector.setRoomKey(null);
    h.collector.event('y');
    h.collector.flush();
    expect('roomKey' in h.batches()[1]).toBe(false);
  });
});
