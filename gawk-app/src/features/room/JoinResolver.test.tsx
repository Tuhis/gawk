// @vitest-environment jsdom
//
// R74 (docs/76 item 43.7): the join box's probe CONNECT to home is one of
// "every CONNECT", so it says lr=1 while the tab would follow an offer.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/react';

const { made } = vi.hoisted(() => ({ made: [] as Array<{ followOffers?: boolean }> }));
vi.mock('../../transport/room-session', () => ({
  RoomSession: class {
    constructor(opts: { followOffers?: boolean }) {
      made.push(opts);
    }
    start() {
      return new Promise(() => {});
    }
    stop() {}
  },
}));

import { JoinResolver } from './JoinResolver';
import { usePopStore } from '../pop/popStore';
import { resetSwitchForTest } from '../pop/switch';

beforeEach(() => {
  made.length = 0;
  resetSwitchForTest();
});
afterEach(() => {
  cleanup();
  resetSwitchForTest();
});

describe('JoinResolver', () => {
  it('says lr=1 on its probe while the tab would follow an offer', async () => {
    render(<JoinResolver code="AB2CD3" />);
    await waitFor(() => expect(made).toHaveLength(1));
    expect(made[0].followOffers).toBe(true);
  });

  it('says nothing once the browser has denied local network access', async () => {
    usePopStore.setState({ permission: 'denied' });
    render(<JoinResolver code="AB2CD3" />);
    await waitFor(() => expect(made).toHaveLength(1));
    expect(made[0].followOffers).toBe(false);
  });
});
