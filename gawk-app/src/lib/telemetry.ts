// R28 TM2 (docs/33): the client telemetry collector.
//
// This module adds NO measurement. `ViewerStats` and `BroadcastStats` already
// exist and are already assembled on the main thread before they reach the
// overlay; the collector subscribes to exactly those objects and ships them.
// If it ever finds itself computing a metric, it has drifted (docs/33 §1.2).
//
// Four properties are load-bearing and every one of them is a test:
//
//  1. **Zero PII (D8).** The envelope carries the OBFUSCATED broadcast key the
//     relay handed us — never the raw, joinable ID a viewer obviously knows —
//     and a coarse browser/OS class reduced here on the device, never the
//     userAgent string. Nothing else identifying is collected; adding anything
//     has to be argued for.
//  2. **Never on the media hot path (D9).** Sampling is a decimated copy of an
//     already-computed object; sending is fire-and-forget. A failed POST is
//     retried a few times and then dropped, silently, forever. No user-visible
//     error, no retry storm, no unbounded buffer. If telemetry can degrade a
//     stream, the item has failed on its own terms.
//  3. **Off means off.** No hello, or a hello with enabled=false, means zero
//     network requests — not a queue that never drains. A relay predating R28
//     and a fleet with telemetry disabled are the same thing here.
//  4. **A truncated session never looks like a complete one.** The per-session
//     byte budget degrades to events-only and says so on the wire, because a
//     silently-clipped session would read as a healthy short one.
//
// The token is a bearer credential. It lives here, goes to the ingest endpoint,
// and is deliberately never placed on ViewerStats — which is what Copy
// diagnostics serializes for pasting into a chat.

import { log } from './logger';
import { telemetrySessionId, type TelemetryHelloMessage } from '../transport/wire';

// The release that produced these samples. D15: this IS the schema version.
declare const __GAWK_APP_VERSION__: string;
export const APP_VERSION: string =
  typeof __GAWK_APP_VERSION__ === 'string' ? __GAWK_APP_VERSION__ : '0.0.0-dev';

export type TelemetryRole = 'viewer' | 'broadcaster';

// How often a batch is flushed. ~10 s of ~2 s samples is ~7.5 KB uncompressed —
// comfortably under the 64 KB keepalive/sendBeacon body cap (docs/33 §4.3).
export const FLUSH_INTERVAL_MS = 10_000;

// The nested object carrying the worst reading seen between two emitted
// samples (docs/33 D16), and the rates it covers.
//
// Deliberately tiny. Its only consumer is the dip detector, which judges one
// primary rate per role; the rest are here because they cost a few bytes each
// and answer "did the whole funnel dip, or just one stage of it?" without a
// second round trip. Everything else keeps its decimated last-tick reading.
export const INTERVAL_MIN_FIELD = 'intervalMin';

export const INTERVAL_MIN_FIELDS = [
  // Viewer.
  'receivedFps',
  'decoderFps',
  'renderedFps',
  // Broadcaster.
  'captureFps',
  'encoderFps',
  'sentFps',
] as const;

// Hard per-session byte budget, counted on the uncompressed JSON we produce.
// 4 MB is about a five-hour session at the default cadence. On exhaustion the
// collector drops to events-only rather than stopping: the events are what
// carry the story (reconnects, close codes, mode changes), and a session that
// went quiet at hour five is still worth knowing about.
export const SESSION_BYTE_BUDGET = 4 << 20;

// A batch that fails this many times is dropped and never mentioned again.
// Small on purpose — the point is to ride out a blip, not to guarantee
// delivery. Telemetry has no delivery guarantee and must not act like it does.
export const MAX_SEND_ATTEMPTS = 3;
export const RETRY_BASE_MS = 2000;

// Bounds on what a session may hold in memory before a flush. These exist so a
// wedged network cannot turn the collector into the memory leak it is supposed
// to help diagnose.
export const MAX_PENDING_SAMPLES = 64;
export const MAX_PENDING_EVENTS = 256;

export interface TelemetrySample<T> {
  tMs: number;
  stats: T;
}

export interface TelemetryEvent {
  tMs: number;
  kind: string;
  detail?: string;
}

export interface TelemetryAppInfo {
  version: string;
  surface: TelemetryRole;
  browser: string;
  os: string;
}

export interface TelemetryBatch<T> {
  v: 1;
  token: string;
  role: TelemetryRole;
  broadcastKey: string;
  seq: number;
  final: boolean;
  app: TelemetryAppInfo;
  startedAtMs: number;
  samples: TelemetrySample<T>[];
  events: TelemetryEvent[];
  // Present and true only once the session hit SESSION_BYTE_BUDGET. A reader
  // must be able to tell a truncated session from a complete one; the rollup
  // records it (docs/33 §4.5 "Data quality").
  truncated?: true;
}

// What one delivery attempt concluded. `retryable` separates "later" from
// "never": a 429 is the service shedding load and the batch is fine, while a
// 4xx rejection will fail identically forever, so retrying it is pure noise at
// exactly the wrong moment. A bare boolean stays accepted and means
// "retryable if it failed" — the behaviour before this split.
export interface TelemetrySendOutcome {
  ok: boolean;
  retryable?: boolean;
}

// The one network call, injectable so tests never touch fetch/sendBeacon.
// `beacon` asks for the unload-safe path; the transport may ignore it.
export type TelemetryTransport = (
  url: string,
  body: string,
  beacon: boolean,
) => Promise<boolean | TelemetrySendOutcome>;

export interface TelemetryCollectorOptions<T> {
  url: string;
  role: TelemetryRole;
  // R37 (docs/40 §4.10 D15): reports true when this session's relay is NOT
  // the deployment default. Batches then go nowhere until the relay
  // advertises its ingest URL (wire 0x12) — against a foreign fleet, the
  // configured `url` could only die at the home deployment's token check,
  // and traffic that can only be rejected is not sent. A getter (not a
  // boolean) because the resolved server can change while the collector
  // lives.
  requireAdvertisedUrl?: () => boolean;
  transport?: TelemetryTransport;
  now?: () => number;
  // Injectable so tests drive time without wall-clock waits. Defaults to the
  // globals; the returned handle is opaque.
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (handle: unknown) => void;
  app?: Partial<TelemetryAppInfo>;
  // Serialized-batch hook, for the privacy assertions.
  onBatch?: (body: string) => void;
  redact?: (stats: T) => T;
}

export class TelemetryCollector<T> {
  private opts: Required<
    Pick<TelemetryCollectorOptions<T>, 'url' | 'role' | 'transport' | 'now' | 'setTimer' | 'clearTimer'>
  > &
    TelemetryCollectorOptions<T>;

  // Session identity, from the hello. Null = collecting nothing, which is the
  // default state and the correct behaviour against a relay that never sends
  // one.
  private token: string | null = null;
  // hex(nonce) from the token — see the `sessionId` getter.
  private session: string | null = null;
  private broadcastKey = '';
  private reportIntervalMs = 2000;
  private startedAtMs = 0;
  private startedAtPerf = 0;

  private samples: TelemetrySample<T>[] = [];
  private events: TelemetryEvent[] = [];
  private seq = 0;
  private bytesUsed = 0;
  private truncated = false;
  private lastSampleAt = -Infinity;
  // The running minimum of INTERVAL_MIN_FIELDS since the last emitted sample
  // (D16). Null between emissions with nothing yet seen.
  private intervalMin: Record<string, number> | null = null;
  private timer: unknown = null;
  private stopped = false;
  // R37 (docs/40 D15): the relay-advertised ingest URL (wire 0x12). Wins
  // over the configured one whenever present.
  private advertisedUrl: string | null = null;
  // Set once a batch has been abandoned: further failures are not worth
  // logging, and the session's coverage is already imperfect.
  private givenUp = false;

  constructor(options: TelemetryCollectorOptions<T>) {
    this.opts = {
      transport: defaultTransport,
      now: () => Date.now(),
      setTimer: (fn, ms) => setTimeout(fn, ms),
      clearTimer: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
      ...options,
    };
  }

  // True once a hello has enabled collection. Everything else no-ops until
  // then, so a disabled fleet produces a collector that is inert rather than
  // one that buffers into a void.
  get active(): boolean {
    return this.token !== null && !this.stopped;
  }

  // The sessionId this collector is reporting under, or null when it is not
  // reporting at all — which is the point of routing a display through here
  // rather than deriving it from the hello: an id shown to a user must be one
  // the dashboard will actually have a row for. A disabled fleet's hello still
  // carries a well-formed token, and naming a session that was never collected
  // would send an operator hunting for a row that cannot exist.
  //
  // Unlike the token, this is not a credential (docs/33 §4.2): it names a
  // session, it does not authorize writing to one. That is what makes it safe
  // to put on the stats overlay and into a Copy-diagnostics blob.
  get sessionId(): string | null {
    return this.session;
  }

  // True once the session hit its byte budget and dropped to events-only.
  // Exposed so a caller (and a test) can tell a degraded session from a short
  // one without parsing a batch.
  get truncatedSession(): boolean {
    return this.truncated;
  }

  // Adopt a session identity from wire 0x0D. A reconnect is a NEW relay
  // session with a NEW token, so this both closes the previous session (final
  // flush) and starts a fresh one — sample histories from two transport
  // sessions must never merge into one row.
  begin(hello: TelemetryHelloMessage): void {
    if (this.stopped) return;
    if (!hello.enabled) {
      // An explicitly-disabled fleet. Drop whatever a previous session had and
      // go inert; do not send it anywhere.
      this.discard();
      return;
    }
    if (this.token !== null) this.flush(true);

    this.token = hello.token;
    // Derived once, defensively: the token has already been strict-parsed off
    // the wire, so a throw here would only ever be a programming error — and
    // D9 says telemetry must never be the thing that breaks a stream, least of
    // all over a display detail. A session with an unnameable id still
    // collects; it just cannot be pointed at.
    try {
      this.session = telemetrySessionId(hello.token);
    } catch {
      this.session = null;
    }
    this.broadcastKey = hello.broadcastKey;
    this.reportIntervalMs = Math.max(hello.reportIntervalMs, 250);
    this.startedAtMs = this.opts.now();
    this.startedAtPerf = perfNow();
    this.samples = [];
    this.events = [];
    this.seq = 0;
    this.bytesUsed = 0;
    this.truncated = false;
    this.givenUp = false;
    this.lastSampleAt = -Infinity;
    this.arm();
  }

  // Record one stats object. Decimated to the relay-requested cadence: the
  // overlay keeps its 500 ms tick, and telemetry does not need every one of
  // them. Cheap enough to call from the stats handler.
  //
  // Decimation alone was lossy in a way that mattered (docs/33 D16): the three
  // discarded ticks between two emitted samples could contain a complete
  // collapse, and nothing downstream could ever know. So the minimum of a few
  // experiential rates is carried across the gap and attached to the emitted
  // sample as `intervalMin`.
  //
  // Deliberately NOT done by emitting the worst tick as the sample: that would
  // bias every median downward and break the funnel ratios, which are the one
  // thing D16 must leave untouched.
  sample(stats: T): void {
    if (!this.active || this.truncated) return;
    const t = perfNow();
    this.trackMinima(stats);
    if (t - this.lastSampleAt < this.reportIntervalMs) return;
    this.lastSampleAt = t;
    const value = this.opts.redact ? this.opts.redact(stats) : stats;
    const withMin = this.attachMinima(value);
    this.intervalMin = null;
    this.samples.push({ tMs: Math.round(t - this.startedAtPerf), stats: withMin });
    // Bound the in-memory batch. Shedding the OLDEST keeps the window near
    // live, which matters because a wedged sender is exactly when the recent
    // samples are the interesting ones.
    if (this.samples.length > MAX_PENDING_SAMPLES) {
      this.samples.splice(0, this.samples.length - MAX_PENDING_SAMPLES);
    }
  }

  // Fold this tick's rates into the running interval minimum. Runs on EVERY
  // tick, including the ones decimation is about to discard — that is the
  // whole point.
  private trackMinima(stats: T): void {
    const src = stats as unknown as Record<string, unknown>;
    for (const field of INTERVAL_MIN_FIELDS) {
      const v = src[field];
      if (typeof v !== 'number' || !Number.isFinite(v)) continue;
      if (this.intervalMin === null) this.intervalMin = {};
      const seen = this.intervalMin[field];
      if (seen === undefined || v < seen) this.intervalMin[field] = v;
    }
  }

  // Attach the interval minimum, but only where it is actually lower than the
  // emitted sample's own reading. An `intervalMin` that merely repeats the
  // sample is bytes on the wire saying nothing.
  private attachMinima(value: T): T {
    if (this.intervalMin === null) return value;
    const src = value as unknown as Record<string, unknown>;
    const lower: Record<string, number> = {};
    let any = false;
    for (const [field, low] of Object.entries(this.intervalMin)) {
      const cur = src[field];
      if (typeof cur === 'number' && Number.isFinite(cur) && low >= cur) continue;
      lower[field] = low;
      any = true;
    }
    if (!any) return value;
    return { ...value, [INTERVAL_MIN_FIELD]: lower };
  }

  // Record something a sample grid cannot represent: a reconnect, a close
  // code, a delivery-mode switch, a ladder step, a decoder error. These are
  // what a human narrates when describing what went wrong, and they survive
  // the byte budget when samples no longer do.
  event(kind: string, detail?: string): void {
    if (!this.active) return;
    this.events.push({
      tMs: Math.round(perfNow() - this.startedAtPerf),
      kind: truncateUtf16Safe(kind, 64),
      ...(detail === undefined ? {} : { detail: truncateUtf16Safe(String(detail), 256) }),
    });
    if (this.events.length > MAX_PENDING_EVENTS) {
      this.events.splice(0, this.events.length - MAX_PENDING_EVENTS);
    }
  }

  // Send whatever is pending.
  //
  // `final` and `beacon` are deliberately independent. `final` is a claim
  // about the SESSION ("this is its last batch", so the service can finalize
  // without waiting for an idle timeout); `beacon` is a claim about the
  // DOCUMENT ("send this in a way that survives the page going away"). The
  // visibilitychange→hidden flush needs the second and must not make the
  // first: `hidden` fires when the user merely switches tabs, and a session
  // marked final there would be finalized while it is still streaming.
  flush(final = false, beacon = false): void {
    if (this.token === null) return;
    // R37 (docs/40 §4.10): a foreign relay that advertised no destination
    // gets ZERO requests — the samples stay buffered (bounded by the byte
    // budget) in case a late 0x12 names one.
    if (this.effectiveUrl() === null) return;
    if (this.samples.length === 0 && this.events.length === 0 && !final) return;

    const batch: TelemetryBatch<T> = {
      v: 1,
      token: this.token,
      role: this.opts.role,
      broadcastKey: this.broadcastKey,
      seq: this.seq++,
      final,
      app: this.appInfo(),
      startedAtMs: this.startedAtMs,
      samples: this.samples,
      events: this.events,
      ...(this.truncated ? { truncated: true as const } : {}),
    };
    this.samples = [];
    this.events = [];

    let body: string;
    try {
      body = JSON.stringify(batch);
    } catch {
      // A stats object that will not serialize is a client bug, not a reason
      // to keep retrying: drop the batch and carry on.
      return;
    }
    this.bytesUsed += body.length;
    if (!this.truncated && this.bytesUsed >= SESSION_BYTE_BUDGET) {
      // Degrade rather than stop, and say so — out loud. Setting the flag
      // alone is not enough: once samples stop, every later flush is empty and
      // returns early, so the marker would never reach the wire at all and a
      // truncated session would look exactly like one that ended here. The
      // event is what guarantees at least one more batch carrying it.
      this.truncated = true;
      this.event('telemetry-budget-exhausted', `${this.bytesUsed} bytes; events only from here`);
    }
    this.opts.onBatch?.(body);
    void this.send(body, beacon, 1);
  }

  // End the session: last batch out, timers down, identity forgotten. Safe to
  // call more than once.
  finish(): void {
    if (this.token === null) return;
    this.flush(true);
    this.disarm();
    this.token = null;
    this.session = null;
  }

  // Tear the collector down for good (component unmount). Distinct from
  // finish(): stop() also refuses any later begin().
  stop(): void {
    this.finish();
    this.stopped = true;
  }

  // R37 (docs/40 D15): adopt the relay-advertised ingest URL. Arriving on
  // its own uni stream it races the 0x0D hello, so this is legal before OR
  // after begin(); a flush blocked by requireAdvertisedUrl unblocks here.
  setAdvertisedUrl(url: string): void {
    this.advertisedUrl = url;
  }

  // True while batches are actually leaving for a relay-advertised (foreign)
  // destination — the in-session indicator's disclosure reads this.
  get reportingToAdvertised(): boolean {
    return this.active && this.advertisedUrl !== null;
  }

  private effectiveUrl(): string | null {
    if (this.advertisedUrl !== null) return this.advertisedUrl;
    if (this.opts.requireAdvertisedUrl?.()) return null;
    return this.opts.url;
  }

  // The visibilitychange → hidden hook. `pagehide`/`unload` are unreliable on
  // mobile; `hidden` is the one that fires, and it is bfcache-compatible — a
  // page that comes back simply keeps collecting under the same identity.
  flushForUnload(): void {
    if (!this.active) return;
    this.flush(false, true);
  }

  private appInfo(): TelemetryAppInfo {
    return {
      version: APP_VERSION,
      surface: this.opts.role,
      ...describeClient(),
      ...this.opts.app,
    };
  }

  private arm(): void {
    this.disarm();
    this.timer = this.opts.setTimer(() => {
      this.timer = null;
      if (!this.active) return;
      this.flush(false);
      this.arm();
    }, FLUSH_INTERVAL_MS);
  }

  private disarm(): void {
    if (this.timer !== null) {
      this.opts.clearTimer(this.timer);
      this.timer = null;
    }
  }

  private discard(): void {
    this.disarm();
    this.token = null;
    this.session = null;
    this.samples = [];
    this.events = [];
  }

  private async send(body: string, beacon: boolean, attempt: number): Promise<void> {
    const url = this.effectiveUrl();
    if (url === null) return;
    let outcome: boolean | TelemetrySendOutcome = false;
    try {
      outcome = await this.opts.transport(url, body, beacon);
    } catch {
      outcome = false;
    }
    const ok = typeof outcome === 'boolean' ? outcome : outcome.ok;
    const retryable = typeof outcome === 'boolean' ? true : outcome.retryable !== false;
    if (ok || this.stopped) return;
    if (!retryable) {
      // A rejection that will fail the same way every time. Retrying it would
      // only add load; the gap shows up honestly in the `seq` sequence.
      if (!this.givenUp) {
        this.givenUp = true;
        log.info('telemetry batch rejected; collection continues');
      }
      return;
    }
    if (attempt >= MAX_SEND_ATTEMPTS) {
      // Dropped, silently and forever. The missing window shows up honestly
      // as a gap in the batch `seq` sequence (docs/33 §4.5), which is a
      // diagnosable fact rather than a lie about coverage.
      if (!this.givenUp) {
        this.givenUp = true;
        log.info('telemetry batch dropped after retries; collection continues');
      }
      return;
    }
    const delay = RETRY_BASE_MS * attempt;
    this.opts.setTimer(() => void this.send(body, beacon, attempt + 1), delay);
  }
}

function perfNow(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

// Bound a string to n UTF-16 code units without splitting a surrogate pair.
// A plain `.slice(0, n)` cuts at a code-unit boundary, which can land inside
// a pair (an emoji or other astral character) and leave a lone high
// surrogate. That is not valid UTF-16 text: sendBeacon/fetch both encode the
// request body to UTF-8, which silently rewrites an unpaired surrogate to
// U+FFFD — corrupting the stored value rather than merely shortening it (the
// same class of bug as the ingest service's byte-boundary clip(), just one
// encoding down). So drop the whole trailing high surrogate rather than keep
// it half-formed: never longer than n, only shorter, which is fine because n
// is a size cap, not a target length.
function truncateUtf16Safe(s: string, n: number): string {
  if (s.length <= n) return s;
  let end = n;
  const last = s.charCodeAt(end - 1);
  if (last >= 0xd800 && last <= 0xdbff) end -= 1;
  return s.slice(0, end);
}

// The production transport. Same-origin by default (docs/33 D1), which is what
// makes the beacon path work without a preflight it could not perform during
// unload.
const defaultTransport: TelemetryTransport = async (url, body, beacon) => {
  if (beacon && typeof navigator !== 'undefined' && typeof navigator.sendBeacon === 'function') {
    try {
      // A Blob carries the content type; sendBeacon cannot set headers.
      // text/plain is CORS-safelisted (R37, docs/40 D17): no preflight —
      // which sendBeacon cannot perform during unload — even when the
      // destination is a foreign relay's ingest. The envelope bytes are
      // identical; the ingest never dispatched on Content-Type.
      if (navigator.sendBeacon(url, new Blob([body], { type: 'text/plain;charset=UTF-8' }))) return true;
    } catch {
      // Fall through to fetch — a refused beacon is not a reason to lose the
      // batch when the page is merely hidden rather than closing.
    }
  }
  if (typeof fetch !== 'function') return false;
  try {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body,
      // keepalive lets a flush outlive the document, which is the whole point
      // of collecting out-of-band (D1).
      keepalive: body.length < 60_000,
    });
    // 429 and 5xx are worth another attempt; every other rejection is final.
    return { ok: res.ok, retryable: res.status === 429 || res.status >= 500 };
  } catch {
    return false;
  }
};

// Coarse client class, reduced HERE so the raw string never leaves the device
// (D8). Deliberately crude: browser family + major version, OS family. Anything
// finer starts to be a fingerprint, and none of the diagnostics need it.
export function describeClient(ua?: string): { browser: string; os: string } {
  const s = ua ?? (typeof navigator !== 'undefined' ? navigator.userAgent : '');
  return { browser: browserClass(s), os: osClass(s) };
}

function browserClass(ua: string): string {
  // Order matters: every Chromium browser also says "Chrome", and every iOS
  // browser says "Safari" because they are all WebKit.
  const rules: [RegExp, string][] = [
    [/\bEdg\/(\d+)/, 'Edge'],
    [/\bOPR\/(\d+)/, 'Opera'],
    [/\bFirefox\/(\d+)/, 'Firefox'],
    // Headless Chrome says "HeadlessChrome/141", never "Chrome/141", so a
    // bare Chrome rule reports it as "unknown" — which is how the R20 e2e
    // harness's own browser looked until it was measured. Headlessness is not
    // a browser family, so it reports as Chrome; the CI-only distinction is
    // not worth a field, and telling them apart would edge toward a
    // fingerprint.
    [/\bHeadlessChrome\/(\d+)/, 'Chrome'],
    [/\bChrome\/(\d+)/, 'Chrome'],
    [/\bVersion\/(\d+).*\bSafari\//, 'Safari'],
  ];
  for (const [re, name] of rules) {
    const m = re.exec(ua);
    if (m) return `${name} ${m[1]}`;
  }
  return 'unknown';
}

function osClass(ua: string): string {
  if (/\bAndroid\b/.test(ua)) return 'Android';
  // iPadOS reports as Macintosh; the touch-point check is the usual
  // discriminator and stays coarse enough not to fingerprint.
  if (/\b(iPhone|iPad|iPod)\b/.test(ua)) return 'iOS';
  if (/\bWindows\b/.test(ua)) return 'Windows';
  if (/\bCrOS\b/.test(ua)) return 'ChromeOS';
  if (/\bMac OS X\b|\bMacintosh\b/.test(ua)) return 'macOS';
  if (/\bLinux\b/.test(ua)) return 'Linux';
  return 'unknown';
}
