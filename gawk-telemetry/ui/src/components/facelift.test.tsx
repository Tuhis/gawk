// @vitest-environment jsdom
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import type { Coverage, SessionView } from '../api/types.ts';
import { CoverageNote, DataSince } from './Chrome.tsx';
import { SessionTable } from './SessionTable.tsx';
import { Sparkline } from './Sparkline.tsx';
import { tokenize } from '../lib/sqlTokens.ts';

// R36 (docs/86): the behaviours the facelift added that a restyle could
// quietly lose.

afterEach(cleanup);

const cov = (over: Partial<Coverage>): Coverage => ({
  rawFromMs: Date.UTC(2026, 9, 6),
  rollupsFromMs: Date.UTC(2026, 9, 31),
  retentionDays: 30,
  note: 'some note',
  ...over,
});

describe('CoverageNote (TU4)', () => {
  it('stays silent when the range only reaches back before the service existed', () => {
    const { container } = render(<CoverageNote coverage={cov({ reason: 'before-data' })} />);
    expect(container.textContent).toBe('');
  });

  it('says the retention line when raw samples inside the range were pruned', () => {
    render(<CoverageNote coverage={cov({ reason: 'raw-pruned' })} />);
    expect(screen.getByRole('note').textContent).toMatch(/older than 30 days are pruned/);
  });

  it('still says it when the range ALSO predates the service', () => {
    // The server reports before-data first; retention has pruned inside the
    // range all the same once raw retention starts after the first rollup.
    render(<CoverageNote coverage={cov({ reason: 'before-data', rawFromMs: Date.UTC(2026, 10, 2) })} />);
    expect(screen.getByRole('note').textContent).toMatch(/pruned/);
  });

  it('says it plainly when nothing is stored at all', () => {
    render(<CoverageNote coverage={cov({ reason: 'none', note: 'this service has no stored rollups at all' })} />);
    expect(screen.getByRole('note').textContent).toMatch(/no stored rollups/);
  });

  it('draws the service age beside the range, not as a warning', () => {
    render(<DataSince coverage={cov({})} fromMs={Date.UTC(2026, 9, 29)} />);
    expect(screen.getByText(/data since/)).toBeTruthy();
    cleanup();
    const { container } = render(<DataSince coverage={cov({})} fromMs={Date.UTC(2026, 10, 1)} />);
    expect(container.textContent).toBe('');
  });
});

describe('Sparkline (TU7)', () => {
  it('breaks the line at a gap instead of joining across it', () => {
    const { container } = render(<Sparkline values={[60, 59, null, 30, 31]} />);
    const d = container.querySelector('path')?.getAttribute('d') ?? '';
    expect(d.match(/M/g)?.length).toBe(2);
  });

  it('shows a dash, never a flat line, when there is no tail', () => {
    render(<Sparkline values={[null, null]} />);
    expect(screen.getByText('—')).toBeTruthy();
  });
});

describe('SessionTable (TU5, TU6)', () => {
  const base: SessionView = {
    sessionId: 'aaaaaaaaaaaaaaaaaaaaaaaa',
    broadcastKey: 'bbbbbbbbbbbb',
    role: 'viewer',
    severity: 'warn',
    clientAgeMs: 2000,
    relayAgeMs: 3000,
    clientState: 'reporting',
    relayState: 'observed',
  };

  it('renders a striped viewer as one row with its legs beneath', () => {
    render(
      <SessionTable
        sparks={null}
        sessions={[
          {
            ...base,
            legs: [
              { member: 0, n: 3, pod: 'gawk-server-a', role: 'origin', queueDepth: 0, relayAgeMs: 1000, datagramsDropped: 0 },
              { member: 1, n: 3, pod: 'gawk-server-a', role: 'origin', queueDepth: 2, relayAgeMs: 1000, datagramsDropped: 12 },
              { member: 2, n: 3, pod: 'gawk-server-b', role: 'edge', queueDepth: 0, relayAgeMs: 1000 },
            ],
          },
        ]}
      />,
    );
    expect(screen.getByText(/striped ×3 · 2 pods/)).toBeTruthy();
    expect(screen.getByText('leg 1 / 3')).toBeTruthy();
    expect(screen.getByText('leg 3 / 3')).toBeTruthy();
    expect(screen.getByText('12 dropped')).toBeTruthy();
    // A leg scraped once has no window to report yet — said, not zeroed.
    expect(screen.getByText(/not yet scraped twice/)).toBeTruthy();
    // ONE session row, not four: the legs are not participants.
    expect(screen.getAllByRole('link', { name: /detail/ })).toHaveLength(1);
  });

  it('says when a viewer was served by a PoP', () => {
    render(<SessionTable sparks={null} sessions={[{ ...base, pop: 'hel1' }]} />);
    expect(screen.getByText('PoP hel1')).toBeTruthy();
  });
});

describe('SQL highlighting (TU10)', () => {
  it('tells keywords, functions, strings and numbers apart', () => {
    const toks = tokenize("SELECT count(*) FROM rollups WHERE role = 'viewer' LIMIT 20 -- top");
    const kind = (s: string) => toks.find((t) => t.s === s)?.t;
    expect(kind('SELECT')).toBe('kw');
    expect(kind('count')).toBe('fn');
    expect(kind("'viewer'")).toBe('st');
    expect(kind('20')).toBe('nu');
    expect(kind('-- top')).toBe('cm');
    expect(kind('rollups')).toBe('tx');
    // Nothing is lost: the tokens rebuild the text exactly.
    expect(toks.map((t) => t.s).join('')).toBe("SELECT count(*) FROM rollups WHERE role = 'viewer' LIMIT 20 -- top");
  });
});
