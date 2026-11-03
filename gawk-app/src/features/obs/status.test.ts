// R33 (docs/83 U4, §5.6): every D9 state, warning and lastError reason maps
// to a sentence; an unknown reason gets the generic line; no raw status code
// or reason string reaches the screen.
import { describe, expect, it, vi } from 'vitest';
import {
  BACKOFF_REASONS,
  clearWhipBackoff,
  describeObsStatus,
  fmtCountdown,
  GENERIC_CAUSE,
  LAST_ERROR_REASONS,
  parseWhipStatus,
  readWhipStatus,
  WARNING_REASONS,
  type ObsView,
  type WhipStatus,
} from './status';

const CODE = 'K7Q2XM';

const st = (s: Partial<WhipStatus>): WhipStatus => ({ state: 'idle', warnings: [], ...s });

// Every string the view would put on screen.
function shown(v: ObsView): string[] {
  switch (v.kind) {
    case 'waiting':
    case 'ended':
      return [v.title, v.body];
    case 'live':
      return [v.title, ...(v.warning ? [v.warning.cause, v.warning.fix ?? ''] : [])];
    case 'stopped':
      return [v.title, v.body, v.reason.cause, v.reason.fix ?? ''];
    case 'refused':
      return [v.reason.cause, v.reason.fix ?? ''];
  }
}

// §5.6, verbatim. {code} and {g} filled with K7Q2XM / the GOP.
const COPY: Record<string, { kind: ObsView['kind']; lines: string[] }> = {
  lost: {
    kind: 'ended',
    lines: [
      'OBS disconnected',
      'If OBS is still open, it reconnects on its own. If not, press Start Streaming in OBS again; your code stays K7Q2XM.',
    ],
  },
  gop_too_long: {
    kind: 'stopped',
    lines: [
      'OBS sent a keyframe only every 4.0 s. Viewers who joined, or hit a glitch, saw a frozen picture for seconds at a time.',
      'In OBS: Settings → Output → Keyframe Interval → 1 s. If it says 0 (auto), that is the cause.',
    ],
  },
  bframes: {
    kind: 'stopped',
    lines: [
      'Your encoder sends B-frames, which gawk can’t play live.',
      'Set B-frames to 0 in the encoder’s settings (Settings → Output), then retry.',
    ],
  },
  invalid_h264: {
    kind: 'stopped',
    lines: ['gawk couldn’t read the video OBS sent.', 'Choose another H.264 encoder in Settings → Output, then retry.'],
  },
  codec: {
    kind: 'refused',
    lines: ['OBS is sending HEVC. gawk takes H.264.', 'In OBS: Settings → Output → Video Encoder → an H.264 one.'],
  },
  simulcast: {
    kind: 'refused',
    lines: ['OBS is sending several layers (simulcast).', 'Turn Simulcast off in Settings → Stream.'],
  },
  gateway_full: {
    kind: 'refused',
    lines: [
      'This server is streaming as many OBS broadcasts as it can.',
      'Wait a minute, then press Start Streaming in OBS again.',
    ],
  },
  relay_busy: {
    kind: 'refused',
    lines: ['This server has no room for another stream right now.', 'Try again in a few minutes.'],
  },
  rejected: {
    kind: 'refused',
    lines: [
      'The server didn’t accept this key. It may have been reset by whoever runs it.',
      'Reset the key here, then paste the new one into OBS.',
    ],
  },
  banned: { kind: 'refused', lines: ['The server’s operator has blocked this stream.'] },
  reserved: {
    kind: 'refused',
    lines: ['This code is in use on another gawk server.', 'Reset the key to get a new code.'],
  },
  client_ip_unknown: {
    kind: 'refused',
    lines: ['This server is set up wrong: it can’t see where your stream comes from.', 'Tell whoever runs it.'],
  },
  killed: { kind: 'refused', lines: ['The server’s operator ended this stream.'] },
};

describe('describeObsStatus (U4)', () => {
  it('has a row for every lastError reason, and nothing else', () => {
    expect(Object.keys(COPY).sort()).toEqual([...LAST_ERROR_REASONS].sort());
  });

  describe.each(LAST_ERROR_REASONS.map((r) => [r]))('lastError %s', (reason) => {
    const e = { reason, gopMs: 4000, codec: 'HEVC' };

    it('says the §5.6 copy, with no raw reason or status code on screen', () => {
      const v = describeObsStatus(st({ state: 'ended', lastError: e }), CODE);
      expect(v.kind).toBe(COPY[reason].kind);
      const lines = shown(v).filter((l) => l !== '');
      for (const line of COPY[reason].lines) expect(lines).toContain(line);
      for (const line of lines) {
        expect(line).not.toContain(reason.includes('_') ? reason : `"${reason}"`);
        expect(line).not.toMatch(/\b(4\d\d|5\d\d)\b/);
      }
    });

    it('a refused POST before any session (state idle) reads the same', () => {
      const v = describeObsStatus(st({ state: 'idle', lastError: e }), CODE);
      expect(shown(v)).toEqual(expect.arrayContaining(COPY[reason].lines));
    });
  });

  // A 422 during a backoff records nothing new: the status keeps the
  // backoff's own reason, with retryAfterMs (D9).
  it.each(BACKOFF_REASONS.map((r) => [r]))('a 422 reported with backoff reason %s gets that reason’s copy and Retry now', (reason) => {
    const v = describeObsStatus(st({ state: 'ended', lastError: { reason, gopMs: 4000 }, retryAfterMs: 48_000 }), CODE);
    expect(v).toMatchObject({
      kind: 'stopped',
      title: 'gawk ended your stream',
      body: 'Viewers on K7Q2XM are waiting; they pick the stream up again when you’re back.',
      retryAfterMs: 48_000,
    });
    expect(shown(v)).toEqual(expect.arrayContaining(COPY[reason].lines));
  });

  it('S4 without a running backoff has no countdown or Retry now', () => {
    const v = describeObsStatus(st({ state: 'ended', lastError: { reason: 'bframes' } }), CODE);
    expect(v).toMatchObject({ kind: 'stopped', retryAfterMs: null });
  });

  it('refusals without a backoff are the red box alone', () => {
    for (const reason of ['codec', 'gateway_full', 'banned']) {
      expect(describeObsStatus(st({ lastError: { reason } }), CODE).kind).toBe('refused');
    }
  });

  it('an unknown reason gets the generic line', () => {
    const v = describeObsStatus(st({ state: 'ended', lastError: { reason: 'brand_new_reason' } }), CODE);
    expect(v).toEqual({ kind: 'refused', reason: GENERIC_CAUSE });
    expect(GENERIC_CAUSE).toEqual({
      cause: 'OBS couldn’t stream to gawk.',
      fix: 'Check the three steps above, then press Start Streaming again.',
    });
    expect(shown(v).join(' ')).not.toContain('brand_new_reason');
  });

  it('an unknown reason during a backoff still reads as S4, with the generic line', () => {
    const v = describeObsStatus(st({ state: 'ended', lastError: { reason: 'later' }, retryAfterMs: 5000 }), CODE);
    expect(v).toMatchObject({ kind: 'stopped', reason: GENERIC_CAUSE, retryAfterMs: 5000 });
  });

  it('names AV1, and never a raw unknown codec string', () => {
    expect(shown(describeObsStatus(st({ lastError: { reason: 'codec', codec: 'AV1' } }), CODE))[0]).toBe(
      'OBS is sending AV1. gawk takes H.264.',
    );
    expect(shown(describeObsStatus(st({ lastError: { reason: 'codec', codec: '<b>x</b>' } }), CODE))[0]).not.toContain(
      '<b>',
    );
  });

  it('idle with no lastError is S2 step 3', () => {
    expect(describeObsStatus(st({}), CODE)).toEqual({
      kind: 'waiting',
      title: 'Waiting for OBS…',
      body: 'This page updates on its own when OBS connects.',
    });
  });

  it('live is S3', () => {
    expect(describeObsStatus(st({ state: 'live' }), CODE)).toEqual({
      kind: 'live',
      title: 'You’re live from OBS',
      warning: null,
    });
  });

  it('ended without lastError is S5 "Your stream has ended"', () => {
    expect(describeObsStatus(st({ state: 'ended' }), CODE)).toEqual({
      kind: 'ended',
      title: 'Your stream has ended',
      body: 'Press Start Streaming in OBS to go live again on K7Q2XM.',
    });
  });

  it.each(WARNING_REASONS.map((r) => [r]))('warning %s is the amber banner on the keyframes cell', (reason) => {
    const v = describeObsStatus(st({ state: 'live', gopMs: 2000, warnings: [{ reason, gopMs: 2000 }] }), CODE);
    expect(v).toEqual({
      kind: 'live',
      title: 'You’re live from OBS',
      warning: {
        cell: 'keyframes',
        cause:
          'OBS sends a keyframe every 2.0 s. At 1 s, viewers who join or hit a glitch see a moving picture again sooner.',
        fix: 'In OBS: Settings → Output → Keyframe Interval → 1 s. It applies the next time you start streaming.',
      },
    });
  });

  it('ignores an unknown warning rather than show its reason', () => {
    expect(describeObsStatus(st({ state: 'live', warnings: [{ reason: 'mystery' }] }), CODE)).toMatchObject({
      warning: null,
    });
  });
});

describe('parseWhipStatus', () => {
  it('reads every D9 field', () => {
    expect(
      parseWhipStatus({
        state: 'live',
        codec: 'H.264',
        resolution: { width: 1920, height: 1080 },
        fps: 60,
        gopMs: 1000,
        bitrateKbps: 6000,
        lossPct: 0.1,
        viewers: 3,
        warnings: [{ reason: 'gop_long', gopMs: 2000 }],
        lastError: { reason: 'codec', codec: 'AV1', gopMs: 1 },
        retryAfterMs: 100,
      }),
    ).toEqual({
      state: 'live',
      codec: 'H.264',
      resolution: { width: 1920, height: 1080 },
      fps: 60,
      gopMs: 1000,
      bitrateKbps: 6000,
      lossPct: 0.1,
      viewers: 3,
      warnings: [{ reason: 'gop_long', gopMs: 2000 }],
      lastError: { reason: 'codec', codec: 'AV1', gopMs: 1 },
      retryAfterMs: 100,
    });
  });

  it('drops mistyped fields and refuses a body without a known state', () => {
    expect(parseWhipStatus({ state: 'idle', fps: '60', warnings: 'x', lastError: { reason: 7 } })).toEqual({
      state: 'idle',
      warnings: [],
    });
    expect(parseWhipStatus({ state: 'weird' })).toBeNull();
    expect(parseWhipStatus(null)).toBeNull();
    expect(parseWhipStatus([])).toBeNull();
  });
});

describe('the status endpoint (D9)', () => {
  const KEY = 'gawk1_K7Q2XM_0123456789abcdef0123456789abcdef';

  it('presents the key as a bearer token, never in the URL', async () => {
    const fetchImpl = vi.fn(async () => ({ ok: true, status: 200, json: async () => ({ state: 'idle' }) }) as Response);
    const read = await readWhipStatus('https://whip.example/whip/status', KEY, undefined, fetchImpl);
    expect(read).toEqual({ kind: 'ok', status: { state: 'idle', warnings: [] } });
    const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('https://whip.example/whip/status');
    expect(url).not.toContain(KEY);
    expect((init.headers as Record<string, string>).Authorization).toBe(`Bearer ${KEY}`);
  });

  it('reads 401 as a malformed key and anything else failing as an error', async () => {
    const res = (status: number) => vi.fn(async () => ({ ok: status < 300, status, json: async () => ({}) }) as Response);
    expect(await readWhipStatus('u', KEY, undefined, res(401))).toEqual({ kind: 'malformed-key' });
    expect(await readWhipStatus('u', KEY, undefined, res(503))).toEqual({ kind: 'error' });
    expect(await readWhipStatus('u', KEY, undefined, res(200))).toEqual({ kind: 'error' });
    const throwing = vi.fn(async () => {
      throw new TypeError('Failed to fetch');
    });
    expect(await readWhipStatus('u', KEY, undefined, throwing)).toEqual({ kind: 'error' });
  });

  it('Retry now sends DELETE with the key', async () => {
    const fetchImpl = vi.fn(async () => ({ ok: true, status: 204 }) as Response);
    expect(await clearWhipBackoff('https://whip.example/whip/status', KEY, fetchImpl)).toBe(true);
    const [, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
    expect(init.method).toBe('DELETE');
    expect((init.headers as Record<string, string>).Authorization).toBe(`Bearer ${KEY}`);
  });
});

describe('fmtCountdown', () => {
  it('writes m:ss, rounding up and never negative', () => {
    expect(fmtCountdown(48_000)).toBe('0:48');
    expect(fmtCountdown(47_100)).toBe('0:48');
    expect(fmtCountdown(600_000)).toBe('10:00');
    expect(fmtCountdown(-5)).toBe('0:00');
  });
});
