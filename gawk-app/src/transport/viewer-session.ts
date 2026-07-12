// Reconnecting wrapper around ViewerPipeline. The pipeline is deliberately
// single-shot (its stopping latch and decoder chain aren't restartable), so
// each attempt builds a fresh one — decoder/reassembler/WebTransport
// teardown-recreate comes for free.
//
// Policy: a WebTransportError exposes no HTTP status, so 429-full, a bad
// cert hash and a wrong URL are indistinguishable in JS. The rule that
// sidesteps this: a session that never connected fails fatally (no retry);
// a session that drops after connecting reconnects with backoff.

import { log } from '../lib/logger';
import type { ConnectOptions } from './connection';
import { ViewerPipeline, type ViewerCallbacks, type ViewerStats } from './viewer';
import type { DecoderConfigMessage } from './wire';
import type { DecodedFrame } from '../media/decoder';

export const RECONNECT_MAX_ATTEMPTS = 10;

// 1s, 2s, 4s, 8s, then capped at 15s — ~100s total before giving up,
// comfortably covering a relay restart.
export function reconnectDelayMs(attempt: number): number {
  return Math.min(1000 * 2 ** (attempt - 1), 15_000);
}

export interface ReconnectInfo {
  attempt: number;
  delayMs: number;
  reason: string;
}

export interface ViewerSessionCallbacks {
  onDecodedFrame: (decoded: DecodedFrame) => void;
  onConfig: (config: DecoderConfigMessage) => void;
  onStats: (stats: ViewerStats) => void;
  // Fired after every successful connect, initial or re-.
  onConnected: () => void;
  // Fired when a reconnect attempt has been scheduled.
  onReconnecting: (info: ReconnectInfo) => void;
  // Fatal: the initial connect failed (rethrown from start() too) or the
  // reconnect budget is exhausted.
  onError: (err: Error) => void;
  // The session is over for good: user stop, or after a fatal error.
  onEnded: () => void;
}

// The subset of ViewerPipeline the session drives; injectable for tests.
export interface PipelineHandle {
  start(): Promise<void>;
  stop(): Promise<void>;
}

export type PipelineFactory = (
  serverUrl: string,
  connectOpts: ConnectOptions,
  callbacks: ViewerCallbacks,
) => PipelineHandle;

const defaultFactory: PipelineFactory = (url, opts, cbs) => new ViewerPipeline(url, opts, cbs);

export class ViewerSession {
  private serverUrl: string;
  private connectOpts: ConnectOptions;
  private cb: ViewerSessionCallbacks;
  private createPipeline: PipelineFactory;

  private pipeline: PipelineHandle | null = null;
  private starting: Promise<void> | null = null;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private attempt = 0;
  private stopped = false;
  private lastReason = 'session closed';

  constructor(
    serverUrl: string,
    connectOpts: ConnectOptions,
    callbacks: ViewerSessionCallbacks,
    createPipeline: PipelineFactory = defaultFactory,
  ) {
    this.serverUrl = serverUrl;
    this.connectOpts = connectOpts;
    this.cb = callbacks;
    this.createPipeline = createPipeline;
  }

  // Rejects on first-connect failure (fatal by policy); nothing is retried
  // and no callbacks fire — the caller owns the error surface for this case.
  async start(): Promise<void> {
    const p = this.buildPipeline();
    this.pipeline = p;
    this.starting = p.start();
    try {
      await this.starting;
    } catch (e) {
      this.pipeline = null;
      throw e;
    } finally {
      this.starting = null;
    }
    if (!this.stopped) this.cb.onConnected();
  }

  // Idempotent; cancels a pending backoff timer or tears down the live
  // pipeline. Always ends with exactly one onEnded.
  async stop(): Promise<void> {
    if (this.stopped) return;
    this.stopped = true;

    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
      this.cb.onEnded();
      return;
    }
    if (this.starting) {
      // A connect is in flight; settle it before tearing down.
      await this.starting.catch(() => {});
    }
    if (this.pipeline) {
      // Fires the inner onEnded, which we forward.
      await this.pipeline.stop();
    } else {
      this.cb.onEnded();
    }
  }

  private buildPipeline(): PipelineHandle {
    const inner: ViewerCallbacks = {
      onDecodedFrame: (d) => this.cb.onDecodedFrame(d),
      onConfig: (c) => this.cb.onConfig(c),
      onStats: (s) => this.cb.onStats(s),
      // Reason capture only: the pipeline's fail() follows up with onEnded
      // via its own stop(), and that is where we act. Acting here too would
      // surface a fatal error before every reconnect.
      onError: (e) => {
        this.lastReason = e.message;
      },
      onEnded: () => this.handlePipelineEnded(),
    };
    return this.createPipeline(this.serverUrl, this.connectOpts, inner);
  }

  private handlePipelineEnded(): void {
    this.pipeline = null;
    if (this.stopped) {
      this.cb.onEnded();
      return;
    }
    this.scheduleReconnect();
  }

  private scheduleReconnect(): void {
    this.attempt += 1;
    if (this.attempt > RECONNECT_MAX_ATTEMPTS) {
      this.cb.onError(
        new Error(`reconnect failed after ${RECONNECT_MAX_ATTEMPTS} attempts: ${this.lastReason}`),
      );
      return;
    }
    const delayMs = reconnectDelayMs(this.attempt);
    log.info(`Viewer reconnect attempt ${this.attempt}/${RECONNECT_MAX_ATTEMPTS} in ${delayMs}ms (${this.lastReason})`);
    this.cb.onReconnecting({ attempt: this.attempt, delayMs, reason: this.lastReason });
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.tryReconnect();
    }, delayMs);
  }

  private async tryReconnect(): Promise<void> {
    if (this.stopped) return;
    const p = this.buildPipeline();
    this.pipeline = p;
    this.starting = p.start();
    try {
      await this.starting;
    } catch (e) {
      // Failed reconnects consume the budget so an unreachable/full relay
      // burns out (~100s) instead of looping forever.
      this.pipeline = null;
      this.lastReason = e instanceof Error ? e.message : String(e);
      if (!this.stopped) this.scheduleReconnect();
      return;
    } finally {
      this.starting = null;
    }
    if (this.stopped) return; // stop() raced the connect and owns teardown
    this.attempt = 0;
    this.cb.onConnected();
  }
}
