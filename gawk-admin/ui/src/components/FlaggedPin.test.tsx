// @vitest-environment jsdom
import { cleanup, render } from '@testing-library/react';
import { afterEach, expect, it } from 'vitest';

import { FlaggedPinSlot } from './FlaggedPin.tsx';
import type { Broadcast } from '../api/types.ts';

afterEach(cleanup);

const flagged: Broadcast = {
  id: 'ABC123',
  key: '3f9a1c2b4d5e',
  publisherActive: true,
  publisherRemoteIp: '203.0.113.7',
  startedAt: new Date().toISOString(),
  viewersGlobal: 3,
  pods: [],
  banState: { banned: false, ban: null },
};

// AP6's last criterion: the slot exists, and renders NOTHING.
//
// The assertion is worth its line because the failure it guards is silent. R40
// adds content flags and this becomes a red pin; if R39 shipped it rendering an
// empty span, a stray dot or a stray gap, nobody would notice until the table
// was full of them.
it('renders nothing at all in R39 — it is R40’s hook point', () => {
  const { container } = render(<FlaggedPinSlot broadcast={flagged} />);
  expect(container.innerHTML).toBe('');
  expect(container.childNodes.length).toBe(0);
});
