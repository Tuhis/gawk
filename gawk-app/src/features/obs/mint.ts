// R33 (docs/83 D3): minting an OBS stream key. A mint-only publish,
// `CONNECT /publish?mint=1`, passes every gate a broadcast's mint does (the
// publish secret, the per-IP rate limit, IP bans, draining). The relay
// announces a fresh broadcast ID and its resume token on server-opened uni
// streams, as it does for a broadcast, then sends SessionClosing(4017) and
// closes with 4017 without creating a broadcast. The ID and the token are
// the result; 4017 is the expected end, never an error.
//
// The dial reuses the broadcaster's URL builder and WebTransport options, so
// the secret, certificate pins and client identity travel exactly as they do
// for "Start a stream". The session is torn down on every path: a leaked one
// would hold a QUIC connection to the relay until the tab closes.
import { buildPublishUrl } from '../../transport/broadcaster';
import { bytesToHex, connectWebTransport, type ConnectOptions } from '../../transport/connection';
import { readStreamBytes } from '../../lib/bytes';
import {
  CLOSE_CODE_PUBLISH_MINTED,
  parseBroadcastAnnounce,
  parseResumeToken,
  parseSessionClosing,
  peekType,
  TYPE_BROADCAST_ANNOUNCE,
  TYPE_RESUME_TOKEN,
  TYPE_SESSION_CLOSING,
} from '../../transport/wire';
import { buildObsKey, type ObsKey } from './key';

// Where the mint failed, so the page can tell "the relay refused the dial"
// (which may mean it wants the publish secret) from "the session opened but
// never produced a key" (CODE-REVIEW: type your failure phases).
export type MintPhase = 'connect' | 'session';

export class MintError extends Error {
  readonly phase: MintPhase;
  constructor(phase: MintPhase, cause: unknown) {
    const message = cause instanceof Error ? cause.message : String(cause);
    super(message);
    this.name = 'MintError';
    this.phase = phase;
    this.cause = cause;
  }
}

// How long an opened session may take to deliver the ID and the token. The
// relay sends both right after the CONNECT is accepted.
export const MINT_MESSAGE_TIMEOUT_MS = 10_000;

export interface MintOptions {
  publishSecret?: string;
  // The resolved relay's pinned hashes, comma-separated (selectCertHashList).
  certHashHex?: string;
}

export interface MintDeps {
  connect?: (url: string, opts: ConnectOptions) => Promise<WebTransport>;
  timeoutMs?: number;
}

function closeQuietly(wt: WebTransport): void {
  void (wt.closed as Promise<unknown> | undefined)?.catch(() => {});
  try {
    wt.close();
  } catch {
    // Already closed (the relay's 4017 got there first): nothing to free.
  }
}

export async function mintObsKey(serverUrl: string, opts: MintOptions = {}, deps: MintDeps = {}): Promise<ObsKey> {
  const connect = deps.connect ?? connectWebTransport;
  const timeoutMs = deps.timeoutMs ?? MINT_MESSAGE_TIMEOUT_MS;
  const url = buildPublishUrl(serverUrl, { publishSecret: opts.publishSecret, mint: true });
  const connectOpts: ConnectOptions = {};
  if (opts.certHashHex) connectOpts.certHashHex = opts.certHashHex;
  if (opts.publishSecret) connectOpts.publishSecret = opts.publishSecret;

  let wt: WebTransport;
  try {
    wt = await connect(url, connectOpts);
  } catch (e) {
    throw new MintError('connect', e);
  }

  try {
    return await readMintResult(wt, timeoutMs);
  } finally {
    // Success and failure share one teardown (CODE-REVIEW: error paths
    // release what they acquired).
    closeQuietly(wt);
  }
}

// Resolves once both the announce and the token have arrived, whichever
// order they come in. The session's end decides only when it comes first:
// then every stream already being read is finished before the verdict, so a
// close that overtakes the reads of the last messages still succeeds.
function readMintResult(wt: WebTransport, timeoutMs: number): Promise<ObsKey> {
  return new Promise<ObsKey>((resolve, reject) => {
    let broadcastId: string | null = null;
    let tokenHex: string | null = null;
    let closingCode: number | null = null;
    let settled = false;
    const pending = new Set<Promise<void>>();

    const timer = setTimeout(() => fail(new Error('the server sent no stream key')), timeoutMs);

    function settle(): void {
      settled = true;
      clearTimeout(timer);
    }
    function fail(e: Error): void {
      if (settled) return;
      settle();
      reject(new MintError('session', e));
    }
    function check(): void {
      if (settled || broadcastId === null || tokenHex === null) return;
      const key = buildObsKey(broadcastId, tokenHex);
      if (key === null) {
        fail(new Error('the server sent a stream key gawk can’t use'));
        return;
      }
      settle();
      resolve(key);
    }

    async function handle(stream: ReadableStream<Uint8Array>): Promise<void> {
      const data = await readStreamBytes(stream);
      const { msgType } = peekType(data);
      switch (msgType) {
        case TYPE_BROADCAST_ANNOUNCE:
          broadcastId = parseBroadcastAnnounce(data);
          break;
        case TYPE_RESUME_TOKEN:
          tokenHex = bytesToHex(parseResumeToken(data));
          break;
        case TYPE_SESSION_CLOSING:
          closingCode = parseSessionClosing(data);
          break;
        default:
          // Telemetry hello, capabilities, identity: not the mint's business.
          break;
      }
      check();
    }

    void (async () => {
      try {
        const reader = wt.incomingUnidirectionalStreams.getReader();
        try {
          for (;;) {
            const { value, done } = await reader.read();
            if (done || settled) return;
            if (!value) continue;
            const p = handle(value).catch(() => {
              // A malformed or cut-off message: the verdict waits for the
              // others, the session's end or the timeout.
            });
            pending.add(p);
            void p.finally(() => pending.delete(p));
          }
        } finally {
          reader.releaseLock();
        }
      } catch {
        // The stream source errors when the session ends; `closed` reports it.
      }
    })();

    const onClosed = async (code: number | null) => {
      await Promise.allSettled([...pending]);
      if (settled) return;
      check();
      if (settled) return;
      // Chrome often reads no close code from the relay; the in-band
      // SessionClosing carries it then.
      const effective = code ?? closingCode;
      fail(
        new Error(
          effective === CLOSE_CODE_PUBLISH_MINTED
            ? 'the server closed the session without a stream key'
            : `the server closed the session${effective !== null ? ` (code ${effective})` : ''}`,
        ),
      );
    };
    wt.closed.then(
      (info) => void onClosed((info as { closeCode?: number } | undefined)?.closeCode ?? null),
      (e: unknown) => void onClosed((e as { closeCode?: number } | undefined)?.closeCode ?? null),
    );
  });
}
