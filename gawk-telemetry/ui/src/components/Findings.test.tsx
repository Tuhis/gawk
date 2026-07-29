// @vitest-environment jsdom
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { ReportPanel } from './Findings.tsx';
import type { Report } from '../api/types.ts';

// TH6's criteria, as tests.
//
// The defect being fixed: the dashboard rendered `finding.verdict` as a bare
// sentence and dropped `Evidence`, `Confidence`, `Action`, `Passed` and
// `Unavailable` — D6/D7's whole provenance apparatus. The API argued and the
// page asserted. So these tests are mostly "is it on screen at all", which is
// the right shape for the thing that went wrong.

afterEach(cleanup);

const relayAnchored: Report = {
  subject: 'aaaa',
  scope: 'session',
  healthy: false,
  findings: [
    {
      id: 'leg-b-single-viewer',
      verdict: 'Leg B for this viewer — their downlink or machine',
      severity: 'warn',
      confidence: 0.85,
      action: 'Check that viewer’s network or device.',
      evidence: [
        { signal: 'subscriber.dropped', value: 412, from: 'relay', comparison: 'vs peer median' },
        { signal: 'peer.median.dropped', value: 3, from: 'relay' },
      ],
    },
  ],
  passed: ['decoder-choking', 'stall-attribution'],
  unavailable: [{ id: 'config-or-limits', missingSignals: ['relay.framesRelayedPerSec'] }],
  caveats: ['no relay-side observation of this session'],
};

const clientOnly: Report = {
  subject: 'bbbb',
  scope: 'session',
  healthy: false,
  findings: [
    {
      id: 'decoder-choking',
      verdict: 'Decoder choking — likely a software-decode fallback',
      severity: 'warn',
      confidence: 0.6,
      evidence: [{ signal: 'receivedFps', value: 60, unit: 'fps', from: 'client' }],
    },
  ],
};

describe('a finding renders as an argument, not an assertion', () => {
  it('shows every evidence row with its value and provenance', () => {
    render(<ReportPanel report={relayAnchored} />);
    expect(screen.getByText('subscriber.dropped')).toBeTruthy();
    expect(screen.getByText('412')).toBeTruthy();
    expect(screen.getByText('vs peer median')).toBeTruthy();
    // The provenance chip: a relay-counted number and a client's account of
    // itself are different kinds of claim (D7).
    expect(screen.getAllByText('relay').length).toBeGreaterThan(0);
  });

  it('shows the playbook action', () => {
    render(<ReportPanel report={relayAnchored} />);
    expect(screen.getByText(/Check that viewer/)).toBeTruthy();
  });

  it('makes a client-only verdict READ as weaker than a relay-anchored one', () => {
    const { unmount } = render(<ReportPanel report={relayAnchored} />);
    expect(screen.getByText(/relay-anchored/)).toBeTruthy();
    unmount();
    render(<ReportPanel report={clientOnly} />);
    // Not merely a smaller number: the label says which kind of claim it is,
    // and the title explains the cap rather than leaving 60 % unexplained.
    expect(screen.getByText(/client only/)).toBeTruthy();
  });

  it('surfaces the report’s caveats', () => {
    render(<ReportPanel report={relayAnchored} />);
    expect(screen.getByText(/no relay-side observation/)).toBeTruthy();
  });

  it('counts what passed and what could not be evaluated', () => {
    render(<ReportPanel report={relayAnchored} />);
    // Both, in the same control: a healthy session and one nothing analysed
    // must be distinguishable at a glance.
    expect(screen.getByText(/2 passed · 1 unavailable/)).toBeTruthy();
  });
});

describe('a healthy report reports health POSITIVELY', () => {
  it('names the checks that support it', () => {
    render(
      <ReportPanel
        report={{ subject: 'c', scope: 'session', healthy: true, passed: ['a', 'b', 'c'] }}
      />,
    );
    expect(screen.getByText(/3 checks passed/)).toBeTruthy();
  });

  it('says so when nothing could be evaluated at all', () => {
    // The one thing an ops view must never do: paint an absence of analysis as
    // green.
    render(<ReportPanel report={{ subject: 'd', scope: 'session', healthy: true }} />);
    expect(screen.getByText(/absence of analysis, not health/)).toBeTruthy();
  });
});
