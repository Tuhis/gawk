// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { ContentFlagStrip, FlaggedPinSlot } from './FlaggedPin.tsx';
import type { Broadcast } from '../api/types.ts';
import { contentFlag } from '../testing/flags.ts';

afterEach(cleanup);

const broadcast: Broadcast = {
  id: 'ABC123',
  key: '3f9a1c2b4d5e',
  publisherActive: true,
  publisherRemoteIp: '203.0.113.7',
  startedAt: new Date().toISOString(),
  viewersGlobal: 3,
  pods: [],
  banState: { banned: false, ban: null },
};

describe('the pin (the R39 seam, given a source in R40)', () => {
  // R39's assertion, kept for every row that is not flagged: an unflagged row
  // carries no stray badge, dot or gap.
  it('renders nothing at all for a broadcast with no flag', () => {
    const { container } = render(<FlaggedPinSlot broadcast={broadcast} />);
    expect(container.innerHTML).toBe('');
  });

  it('renders nothing for a resolved flag or an observed one', () => {
    for (const flag of [
      contentFlag({ state: 'resolved', resolution: 'cleared' }),
      contentFlag({ mode: 'observe' }),
    ]) {
      const { container } = render(<FlaggedPinSlot broadcast={{ ...broadcast, contentFlag: flag }} />);
      expect(container.innerHTML).toBe('');
      cleanup();
    }
  });

  it('pins an open flag, and says when it has been acknowledged', () => {
    render(<FlaggedPinSlot broadcast={{ ...broadcast, contentFlag: contentFlag() }} />);
    expect(screen.getByText('flagged')).toBeTruthy();
    cleanup();
    render(
      <FlaggedPinSlot broadcast={{ ...broadcast, contentFlag: contentFlag({ state: 'acknowledged' }) }} />,
    );
    expect(screen.getByText('flagged · acknowledged')).toBeTruthy();
  });
});

describe('the flag strip', () => {
  function mount(over: Parameters<typeof contentFlag>[0] = {}, extra: { watch?: string; kill?: boolean } = {}) {
    const actions = { ack: vi.fn(), dismiss: vi.fn(), kill: vi.fn() };
    const flag = contentFlag(over);
    render(
      <ContentFlagStrip
        flag={flag}
        now={Date.parse(flag.raisedAt) + 190_000}
        busy={false}
        watch={extra.watch}
        onAcknowledge={actions.ack}
        onDismiss={actions.dismiss}
        onKill={extra.kill === false ? undefined : actions.kill}
      />,
    );
    return actions;
  }

  // Score, window, age and state: what an operator judges by, since there is
  // no frame to look at (D8).
  it('shows the score, the window, the labels, the age and the state', () => {
    mount({ reminders: 2 });
    expect(screen.getByText('Content flag #12')).toBeTruthy();
    expect(screen.getByText('open')).toBeTruthy();
    expect(screen.getByText('signal live')).toBeTruthy();
    expect(screen.getByText('0.97')).toBeTruthy();
    expect(screen.getByText(/mean 0\.81, peak 0\.99/)).toBeTruthy();
    expect(screen.getByText('window 4 of 6')).toBeTruthy();
    expect(screen.getByText('FEMALE_GENITALIA_EXPOSED ×4')).toBeTruthy();
    expect(screen.getByText('raised 3m 10s ago')).toBeTruthy();
    expect(screen.getByText('re-paged 2×')).toBeTruthy();
  });

  // A silent sampler is not evidence of a clean broadcast (D9).
  it('marks an unknown signal as a caution, not as cleared', () => {
    mount({ signal: 'unknown' });
    expect(screen.getByText('signal unknown')).toBeTruthy();
    expect(screen.queryByText('signal cleared')).toBeNull();
  });

  it('offers Acknowledge, Dismiss, Watch and Kill on an open flag', () => {
    const actions = mount({}, { watch: 'https://gawk.example/#/view/ABC123' });
    fireEvent.click(screen.getByRole('button', { name: 'Acknowledge' }));
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss…' }));
    fireEvent.click(screen.getByRole('button', { name: 'Kill…' }));
    expect(actions.ack).toHaveBeenCalledOnce();
    expect(actions.dismiss).toHaveBeenCalledOnce();
    expect(actions.kill).toHaveBeenCalledOnce();
    expect(screen.getByRole('link', { name: 'Watch' }).getAttribute('href')).toBe(
      'https://gawk.example/#/view/ABC123',
    );
  });

  it('drops Acknowledge once acknowledged, and names who did it', () => {
    mount({ state: 'acknowledged', acknowledgedBy: 'juho@example.com', acknowledgedAt: new Date().toISOString() });
    expect(screen.queryByRole('button', { name: 'Acknowledge' })).toBeNull();
    expect(screen.getByText('acknowledged by juho@example.com')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Dismiss…' })).toBeTruthy();
  });

  // No dead links, no dead buttons: Watch needs -app-base-url, Kill needs the
  // broadcast in the fleet scan.
  it('omits Watch without a link and Kill without a target', () => {
    mount({}, { kill: false });
    expect(screen.queryByRole('link', { name: 'Watch' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Kill…' })).toBeNull();
  });
});
