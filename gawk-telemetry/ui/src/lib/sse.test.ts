import { describe, expect, it } from 'vitest';

import { readSse, SseParser, type SseEvent } from './sse.ts';

// The parser the live feed rides on since it stopped being an EventSource
// (docs/55 D4). The cases are the ones `stream.go` actually emits plus the
// framing a proxy is free to impose on them: arbitrary chunk boundaries.

function all(parser: SseParser, ...chunks: string[]): SseEvent[] {
  return chunks.flatMap((c) => parser.push(c));
}

describe('SseParser', () => {
  it('parses exactly what the server writes', () => {
    const p = new SseParser();
    const evs = all(
      p,
      'retry: 2000\n\n',
      'event: snapshot\ndata: {"atMs":1}\n\n',
      ': keepalive\n\n',
      'event: expired\ndata: {}\n\n',
    );
    expect(p.retryMs).toBe(2000);
    expect(evs).toEqual([
      { event: 'snapshot', data: '{"atMs":1}' },
      { event: 'expired', data: '{}' },
    ]);
  });

  it('survives any chunk boundary, including inside a CRLF', () => {
    const text = 'event: snapshot\r\ndata: a\r\ndata: b\r\n\r\nevent: snapshot\rdata: c\r\r';
    const whole = all(new SseParser(), text);
    expect(whole).toEqual([
      { event: 'snapshot', data: 'a\nb' },
      { event: 'snapshot', data: 'c' },
    ]);
    for (let cut = 1; cut < text.length; cut++) {
      const split = all(new SseParser(), text.slice(0, cut), text.slice(cut));
      expect(split, `cut at ${cut}`).toEqual(whole);
    }
    // One character at a time is every boundary at once.
    expect(all(new SseParser(), ...text.split(''))).toEqual(whole);
  });

  it('defaults the type to message, and drops events that carry no data', () => {
    const p = new SseParser();
    expect(all(p, 'data: x\n\n', 'event: lonely\n\n', 'data\n\n')).toEqual([
      { event: 'message', data: 'x' },
      // `data` with no colon is a data line with an empty value.
      { event: 'message', data: '' },
    ]);
  });

  it('strips one leading space, not two, and ignores unknown fields', () => {
    const p = new SseParser();
    expect(all(p, 'data:  two\nid: 7\nbogus: x\n\n')).toEqual([{ event: 'message', data: ' two' }]);
  });

  it('accepts retry only as digits', () => {
    const p = new SseParser();
    all(p, 'retry: 1500\n\n', 'retry: soon\n\n', 'retry: -1\n\n');
    expect(p.retryMs).toBe(1500);
  });

  it('holds back an event the stream has not finished', () => {
    const p = new SseParser();
    expect(all(p, 'event: snapshot\ndata: {"a"')).toEqual([]);
    expect(all(p, ':1}\n\n')).toEqual([{ event: 'snapshot', data: '{"a":1}' }]);
  });
});

function bodyOf(chunks: string[], error?: Error): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(enc.encode(c));
      if (error) controller.error(error);
      else controller.close();
    },
  });
}

describe('readSse', () => {
  it('delivers events and retry hints, then resolves at the end of the stream', async () => {
    const events: SseEvent[] = [];
    const retries: number[] = [];
    await readSse(
      bodyOf(['retry: 2000\n\nevent: snap', 'shot\ndata: 1\n\n', 'event: snapshot\ndata: 2']),
      (e) => events.push(e),
      (ms) => retries.push(ms),
    );
    expect(retries).toEqual([2000]);
    // The unterminated third frame is discarded, as EventSource would.
    expect(events).toEqual([{ event: 'snapshot', data: '1' }]);
  });

  it('decodes a multi-byte character split across chunks', async () => {
    const bytes = new TextEncoder().encode('data: ä\n\n');
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        // 'ä' is two bytes in UTF-8; cut between them.
        c.enqueue(bytes.slice(0, 7));
        c.enqueue(bytes.slice(7));
        c.close();
      },
    });
    const events: SseEvent[] = [];
    await readSse(body, (e) => events.push(e));
    expect(events).toEqual([{ event: 'message', data: 'ä' }]);
  });

  it('rejects when the connection fails', async () => {
    const events: SseEvent[] = [];
    await expect(
      readSse(bodyOf(['data: 1\n\n'], new Error('reset')), (e) => events.push(e)),
    ).rejects.toThrow('reset');
  });
});
