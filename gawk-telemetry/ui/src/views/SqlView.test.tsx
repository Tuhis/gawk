// @vitest-environment jsdom
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { QueryStatus } from '../api/types.ts';

// An engine that failed to open is a failure with a cause, not a build
// property; the console must not tell the operator otherwise.

const status = vi.hoisted(() => ({ current: null as QueryStatus | null }));

vi.mock('../api/client.ts', async (orig) => ({
  ...(await orig<typeof import('../api/client.ts')>()),
  fetchQueryStatus: () => Promise.resolve(status.current),
}));

const { SqlView } = await import('./SqlView.tsx');

afterEach(cleanup);

describe('SqlView without an engine', () => {
  it('says the engine failed to open, and why', async () => {
    status.current = {
      enabled: false,
      failed: true,
      reason: 'the query engine failed to open: query did not finish within 30s',
      rowLimit: 1000,
      timeoutMs: 30000,
    };
    render(<SqlView />);
    expect(await screen.findByText(/failed to open: query did not finish within 30s/)).not.toBeNull();
    expect(screen.queryByText(/compiled into this build/)).toBeNull();
  });

  it('calls a build without one a build property', async () => {
    status.current = { enabled: false, reason: 'no engine compiled in', rowLimit: 0, timeoutMs: 0 };
    render(<SqlView />);
    expect(await screen.findByText(/No query engine is compiled into this build/)).not.toBeNull();
  });
});
