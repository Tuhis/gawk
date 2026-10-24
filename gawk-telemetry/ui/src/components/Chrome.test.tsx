// @vitest-environment jsdom
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import type { Meta } from '../api/types.ts';
import { useMetaStore } from '../state/metaStore.ts';
import { Nav } from './Chrome.tsx';

// The SQL tab is hidden only where the console was never going to work. On
// 2026-10-22 the engine failed to open in production, meta said `sql: false`,
// and the tab vanished: a failure dressed as a feature nobody enabled.

afterEach(() => {
  cleanup();
  useMetaStore.setState({ meta: null });
});

function meta(over: Partial<Meta>): Meta {
  return {
    retentionDays: 30,
    rawFromMs: 0,
    scrapeIntervalMs: 5000,
    annotations: true,
    sql: true,
    resolve: false,
    streamIntervalMs: 1000,
    serverNowMs: 0,
    ...over,
  };
}

describe('Nav', () => {
  it('shows the SQL tab when the engine is up', () => {
    useMetaStore.setState({ meta: meta({ sql: true }) });
    render(<Nav />);
    expect(screen.queryByRole('link', { name: 'SQL' })).not.toBeNull();
  });

  it('hides the SQL tab in a deployment without an engine', () => {
    useMetaStore.setState({ meta: meta({ sql: false, sqlReason: 'no engine compiled in' }) });
    render(<Nav />);
    expect(screen.queryByRole('link', { name: 'SQL' })).toBeNull();
  });

  it('keeps the SQL tab when a configured engine failed to open', () => {
    useMetaStore.setState({
      meta: meta({ sql: false, sqlFailed: true, sqlReason: 'the query engine failed to open: timeout' }),
    });
    render(<Nav />);
    expect(screen.queryByRole('link', { name: 'SQL' })).not.toBeNull();
  });
});
