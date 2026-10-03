// A Server-Sent Events parser for a `fetch` response body (docs/55 D4).
//
// The live feed used to be an `EventSource`, which cannot send a header — and
// in OIDC mode every read carries `Authorization: Bearer`. So the stream is
// read with `fetch` and parsed here. The alternatives were a token in the URL
// (it lands in Ingress logs and browser history) and a cookie for one
// endpoint (the CSRF class R39 deleted); D4 rejects both.
//
// This is the WHATWG "interpret an event stream" algorithm, restricted to what
// a parser has to do — the reconnection policy that `EventSource` bundles with
// it lives in `state/liveStore.ts`, where it can be seen and tested:
//
//   * Lines end in CRLF, LF or CR, and a CRLF may be split across two chunks.
//   * A blank line dispatches the event being built; a line starting with `:`
//     is a comment (the server's `: keepalive` heartbeat).
//   * `field: value` — one leading space after the colon is dropped. Unknown
//     fields are ignored.
//   * `data` lines accumulate, joined with `\n`. An event with no data is not
//     dispatched, exactly as `EventSource` would not dispatch it.
//   * `retry` takes ASCII digits only; anything else is ignored.
//   * An event left unterminated when the stream ends is discarded: a frame
//     cut off mid-way is not a frame.

export interface SseEvent {
  /** `message` when the stream named none, as `EventSource` would. */
  event: string;
  data: string;
}

export class SseParser {
  private buf = '';
  /** The previous chunk ended in CR, so a leading LF here is its other half. */
  private pendingCR = false;
  private data: string[] = [];
  private event = '';
  /** The last valid `retry:` value seen, in milliseconds. */
  retryMs: number | null = null;

  /** Feed decoded text; returns every event the text completed. */
  push(text: string): SseEvent[] {
    if (this.pendingCR && text.startsWith('\n')) text = text.slice(1);
    this.pendingCR = false;
    this.buf += text;

    const out: SseEvent[] = [];
    let start = 0;
    for (let i = 0; i < this.buf.length; i++) {
      const c = this.buf[i];
      if (c !== '\n' && c !== '\r') continue;
      const line = this.buf.slice(start, i);
      if (c === '\r') {
        if (i + 1 < this.buf.length) {
          if (this.buf[i + 1] === '\n') i++;
        } else {
          // The LF that would make this a CRLF may be the first byte of the
          // next chunk; remember to swallow it there.
          this.pendingCR = true;
        }
      }
      start = i + 1;
      const ev = this.line(line);
      if (ev) out.push(ev);
    }
    this.buf = this.buf.slice(start);
    return out;
  }

  private line(line: string): SseEvent | null {
    if (line === '') return this.dispatch();
    if (line.startsWith(':')) return null;
    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    switch (field) {
      case 'data':
        this.data.push(value);
        break;
      case 'event':
        this.event = value;
        break;
      case 'retry':
        if (/^\d+$/.test(value)) this.retryMs = Number(value);
        break;
      default:
        // `id` included: nothing here resumes from a last event ID — the
        // server sends a full snapshot first on every connection.
        break;
    }
    return null;
  }

  private dispatch(): SseEvent | null {
    const data = this.data;
    const event = this.event || 'message';
    this.data = [];
    this.event = '';
    if (data.length === 0) return null;
    return { event, data: data.join('\n') };
  }
}

/**
 * Read an event stream to its end, calling `onEvent` for each event as it
 * completes. Resolves when the server closes the stream; rejects when the
 * connection fails or is aborted. `onRetry` hears each `retry:` hint.
 */
export async function readSse(
  body: ReadableStream<Uint8Array>,
  onEvent: (ev: SseEvent) => void,
  onRetry?: (ms: number) => void,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  const parser = new SseParser();
  let lastRetry: number | null = null;
  const deliver = (events: SseEvent[]) => {
    if (onRetry && parser.retryMs !== null && parser.retryMs !== lastRetry) {
      lastRetry = parser.retryMs;
      onRetry(parser.retryMs);
    }
    for (const ev of events) onEvent(ev);
  };
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      deliver(parser.push(decoder.decode(value, { stream: true })));
    }
    deliver(parser.push(decoder.decode()));
  } finally {
    reader.releaseLock();
  }
}
