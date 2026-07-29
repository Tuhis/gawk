import { describe, expect, it } from 'vitest';

import { findingToMarkdown, reportToMarkdown, seriesToCsv, timelineToCsv } from './export.ts';
import type { Report, Timeline } from '../api/types.ts';

// TH11's export criteria. The shape they have to fit is this project's own
// habit: field findings end up in `docs/*.md`, so a finding must paste in with
// its evidence intact rather than as a sentence somebody then has to justify.

describe('markdown export', () => {
  it('carries every evidence row with value, unit and provenance', () => {
    const md = findingToMarkdown({
      id: 'keyframe-gap-churn',
      verdict: 'Delta loss on leg B is eating GOPs',
      severity: 'bad',
      confidence: 0.8,
      action: 'Lower the rung.',
      evidence: [
        { signal: 'reorderGapResyncs', value: 41, from: 'client', comparison: 'per keyframe' },
        { signal: 'subscriber.keyframesDropped', value: 7, unit: 'frames', from: 'relay' },
      ],
    });
    expect(md).toContain('**BAD**');
    expect(md).toContain('`keyframe-gap-churn`');
    expect(md).toContain('confidence: 80 %');
    expect(md).toContain('`reorderGapResyncs` = 41 _(client)_ — per keyframe');
    expect(md).toContain('`subscriber.keyframesDropped` = 7 frames _(relay)_');
    expect(md).toContain('action: Lower the rung.');
  });

  it('exports an absent value as an em dash, never as zero', () => {
    // The absent-vs-zero distinction is what this whole surface exists to
    // preserve. An export that flattened it would launder "not measured" into
    // "measured zero" the moment it left the page.
    const md = findingToMarkdown({
      id: 'x',
      verdict: 'v',
      severity: 'warn',
      evidence: [{ signal: 'capToRenderMs', from: 'client' }],
    });
    expect(md).toContain('`capToRenderMs` = —');
    expect(md).not.toContain('= 0');
  });

  it('carries passed and unavailable, not only the findings', () => {
    const rep: Report = {
      subject: 'abc',
      scope: 'session',
      healthy: false,
      findings: [{ id: 'f', verdict: 'v', severity: 'warn' }],
      passed: ['p1', 'p2'],
      unavailable: [{ id: 'u1', missingSignals: ['relay.x'] }],
    };
    const md = reportToMarkdown(rep);
    expect(md).toContain('**Passed** (2)');
    expect(md).toContain('`u1` — missing `relay.x`');
  });

  it('includes annotations beside the findings', () => {
    const md = reportToMarkdown(
      { subject: 'abc', scope: 'session', healthy: true, passed: ['p'] },
      {
        notes: [
          { id: '1', createdAtMs: 1, atMs: Date.UTC(2026, 6, 29, 21, 4), text: 'switched to WiFi' },
        ],
      },
    );
    expect(md).toContain('switched to WiFi');
  });
});

describe('csv export', () => {
  it('quotes cells that would otherwise break the format', () => {
    const csv = seriesToCsv(['a', 'b'], [['x,y', 'he said "hi"']]);
    expect(csv).toContain('"x,y"');
    expect(csv).toContain('"he said ""hi"""');
  });

  it('leads with an absolute timestamp and leaves absent cells empty', () => {
    const tl: Timeline = {
      sessionId: 's',
      role: 'viewer',
      fields: ['tMs', 'receivedFps', 'decoderFps'],
      points: [
        { tMs: 0, receivedFps: 60, decoderFps: 60 },
        // decoderFps absent on the second sample: it must stay empty, because
        // an empty cell and a 0 are different claims in a spreadsheet too.
        { tMs: 2000, receivedFps: 58 },
      ],
      downsampled: false,
      totalSamples: 2,
      startedAtMs: Date.UTC(2026, 6, 29, 21, 4, 0),
    };
    const rows = timelineToCsv(tl).split('\n');
    expect(rows[0]).toBe('atIso,tMs,receivedFps,decoderFps');
    expect(rows[1]).toBe('2026-07-29T21:04:00.000Z,0,60,60');
    expect(rows[2]).toBe('2026-07-29T21:04:02.000Z,2000,58,');
  });
});
