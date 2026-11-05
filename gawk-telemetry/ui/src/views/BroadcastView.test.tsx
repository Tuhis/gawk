// @vitest-environment jsdom
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { BroadcastDetail } from '../api/types.ts';

// ECharts needs a canvas, which jsdom does not have; the charts are not what
// this test is about.
vi.mock('../charts/useChart.ts', () => ({ useChart: () => ({ current: null }) }));

const pending: Record<string, (d: BroadcastDetail) => void> = {};
vi.mock('../api/client.ts', () => ({
  fetchBroadcast: (key: string) => new Promise<BroadcastDetail>((resolve) => (pending[key] = resolve)),
  fetchBroadcastDiagnose: () => new Promise(() => {}),
  fetchAnnotations: () => Promise.resolve([]),
}));

// jsdom has no matchMedia; the page asks it whether it is on a phone.
window.matchMedia ??= ((q: string) =>
  ({ matches: false, media: q, addEventListener: () => {}, removeEventListener: () => {} }) as unknown as MediaQueryList);

const { BroadcastView } = await import('./BroadcastView.tsx');

afterEach(cleanup);

const detail = (key: string): BroadcastDetail => ({
  broadcastKey: key,
  fromMs: 1_000_000,
  toMs: 2_000_000,
  live: false,
  lanes: [{ kind: 'relay', severity: 'ok', startedAtMs: 1_000_000, cadenceMs: 5000, points: [{ atMs: 1_500_000, value: 30 }] }],
  coverage: { rawFromMs: 0 },
});

// Moving from one broadcast to another reused the page with the OLD
// broadcast's lanes under the new heading until the new fetch landed, and
// its charts at the old zoom (round-3 review). The page now shows that it
// is loading the new one, and the lanes it then draws are a fresh stack.
describe('BroadcastView', () => {
  it('does not show the last broadcast’s lanes while the next one loads', async () => {
    const { rerender } = render(<BroadcastView broadcastKey="aaaaaaaaaaaa" />);
    pending.aaaaaaaaaaaa(detail('aaaaaaaaaaaa'));
    expect(await screen.findByText('aaaaaaaaaaaa')).toBeTruthy();

    rerender(<BroadcastView broadcastKey="bbbbbbbbbbbb" />);
    expect(await screen.findByText(/Assembling the lanes/)).toBeTruthy();
    expect(screen.queryByText('aaaaaaaaaaaa')).toBeNull();
  });
});
