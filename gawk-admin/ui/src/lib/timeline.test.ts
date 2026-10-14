import { describe, expect, it } from 'vitest';

import { contentFlag } from '../testing/flags.ts';
import { timeline } from '../testing/timeline.ts';
import { axisPct, axisTicks, defaultIncident, formatSeconds, incidentEnd, shortLabel, suggestedBan, thresholdAt } from './timeline.ts';

describe('the timeline helpers (R70)', () => {
  it('opens on the newest unresolved flag-mode incident, else the newest', () => {
    const old = contentFlag({ id: 1, raisedAt: '2026-10-15T19:00:00Z' });
    const resolved = contentFlag({ id: 2, state: 'resolved', raisedAt: '2026-10-15T20:00:00Z' });
    const observed = contentFlag({ id: 3, mode: 'observe', raisedAt: '2026-10-15T21:00:00Z' });
    expect(defaultIncident([old, resolved, observed])?.id).toBe(1);
    expect(defaultIncident([resolved, observed])?.id).toBe(3);
    expect(defaultIncident([])).toBeNull();
  });

  it('pre-selects the ban the server suggested, and only that', () => {
    expect(suggestedBan(timeline({ suggestion: { verdict: 'violation', banSeconds: 604_800 } }))).toBe('week');
    expect(suggestedBan(timeline({ suggestion: { verdict: 'violation', banSeconds: 86_400 } }))).toBe('day');
    expect(suggestedBan(timeline({ suggestion: {} }))).toBe('cooldown');
    expect(suggestedBan(timeline({ suggestion: { banSeconds: 123 } }))).toBe('cooldown');
  });

  it('ends an incident at its resolution, a held clear, or the range', () => {
    const to = Date.parse('2026-10-15T22:00:00Z');
    expect(incidentEnd(contentFlag({ resolvedAt: '2026-10-15T20:00:00Z' }), to)).toBe(Date.parse('2026-10-15T20:00:00Z'));
    expect(incidentEnd(contentFlag({ clearedAt: '2026-10-15T21:00:00Z' }), to)).toBe(Date.parse('2026-10-15T21:00:00Z'));
    expect(incidentEnd(contentFlag(), to)).toBe(to);
  });

  it('places and labels the axis', () => {
    const from = Date.parse('2026-10-15T19:00:00Z');
    const to = from + 3_600_000;
    expect(axisPct(from + 1_800_000, from, to)).toBe(50);
    expect(axisPct(to + 1, from, to)).toBe(100);
    const ticks = axisTicks(from, to);
    expect(ticks.length).toBeGreaterThanOrEqual(4);
    expect(ticks.length).toBeLessThanOrEqual(7);
    expect(ticks.every((t) => t.at >= from && t.at <= to)).toBe(true);
  });

  it('reads the threshold of the session a moment falls in', () => {
    const tl = timeline();
    expect(thresholdAt(tl, Date.parse('2026-10-15T19:30:00Z'))).toBe(0.5);
    const unknown = timeline({ levels: { ...tl.levels, threshold: null }, sessions: [] });
    expect(thresholdAt(unknown, Date.parse('2026-10-15T19:30:00Z'))).toBeNull();
  });

  it('words durations and labels', () => {
    expect(formatSeconds(40)).toBe('40 s');
    expect(formatSeconds(2460)).toBe('41 min');
    expect(formatSeconds(7800)).toBe('2 h 10 min');
    expect(formatSeconds(0)).toBe('0 s');
    expect(shortLabel('MALE_GENITALIA_EXPOSED')).toBe('MALE_GENITALIA');
  });
});
