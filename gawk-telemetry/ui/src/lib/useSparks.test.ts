// @vitest-environment jsdom
import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useLiveStore } from '../state/liveStore.ts';
import { useSparks } from './useSparks.ts';

// The sparkline poll (R36, docs/86 TU7) keeps the last good tails, like the
// live feed keeps its last good snapshot: freezing the page, or one failed
// poll, must not blank the column (round-2 review).

const tails = { atMs: 1, stepMs: 4000, windowMs: 120000, sessions: { s1: { field: 'decoderFps', values: [60, 59] } } };

let respond: (url: string, init?: RequestInit) => Promise<Response>;

beforeEach(() => {
  useLiveStore.setState({ paused: false });
  respond = async () => new Response(JSON.stringify(tails), { status: 200 });
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => respond(url, init)),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('useSparks', () => {
  it('keeps the tails when the page is paused mid-poll', async () => {
    const { result } = renderHook(() => useSparks('aaaaaaaaaaaa', true));
    await waitFor(() => expect(result.current).not.toBeNull());

    // The next poll hangs; pausing aborts it.
    respond = (_url, init) =>
      new Promise((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
      });
    await act(async () => {
      await new Promise((r) => setTimeout(r, 2100));
    });
    act(() => useLiveStore.setState({ paused: true }));
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(result.current).not.toBeNull();
  });

  it('keeps the last good tails through a failed poll, and clears them on a miss', async () => {
    const { result } = renderHook(() => useSparks('aaaaaaaaaaaa', true));
    await waitFor(() => expect(result.current).not.toBeNull());

    respond = async () => new Response('busy', { status: 503 });
    await act(async () => {
      await new Promise((r) => setTimeout(r, 2100));
    });
    expect(result.current).not.toBeNull();

    // A 404 means the projection no longer holds this broadcast.
    respond = async () => new Response('no', { status: 404 });
    await act(async () => {
      await new Promise((r) => setTimeout(r, 2100));
    });
    expect(result.current).toBeNull();
  }, 10_000);
});
