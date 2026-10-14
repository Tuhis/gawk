import { useCallback, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import type { ContentFlagPage, ReplayRequest, ReplayResponse } from '../api/types.ts';
import { AuthRedirect } from '@gawk/oidc-session';
import { ScreeningBanners } from '../components/ScreeningBanners.tsx';
import { describeLabels, describeResolution, formatScore } from '../lib/flags.ts';
import { formatInstant } from '../lib/format.ts';
import { useLoader } from '../lib/useLoader.ts';
import ui from '../styles/ui.module.css';

const REFRESH_MS = 15_000;

/** docs/71 D7's placeholder defaults, until CF9's calibration replaces them. */
const DEFAULT_THRESHOLD = '0.8';
const DEFAULT_WINDOW = '6';
const DEFAULT_HITS = '4';
/** The replay range the form opens on: the last week, CF9's minimum run. */
const DEFAULT_RANGE_DAYS = 7;
/** docs/71 D21's window cap, mirrored so the form refuses before a round trip. */
const MAX_WINDOW = 64;
/** CF9's acceptance bar: fewer than one would-trip per 100 broadcast-hours. */
const TARGET_PER_100H = 1;

/**
 * Calibration (R40, docs/71 D19–D21, CF9).
 *
 * The sampler ships in `observe` mode: it scores every broadcast, stores the
 * interesting samples in gawk-admin and raises OBSERVED flags that page no
 * one. This view answers the calibration question — "what would this setting
 * have done?" — by replaying any candidate window rule over the stored
 * samples, and lists the observed flags beside it with a Watch link, so an
 * operator notified of one (`content_flag.observed`, opt-in) can join live
 * while the trigger is still on screen (OD14). Recall cannot be measured
 * here — on live traffic there should be nothing to find — which is what
 * `gawk-sampler score` is for.
 */
export function CalibrationView({ refreshMs = REFRESH_MS }: { refreshMs?: number }) {
  const api = useApi();
  const load = useCallback(() => api.contentFlags('all', 'observe', 50), [api]);
  const { data, loadedAt, error, loading } = useLoader<ContentFlagPage>(load, refreshMs);
  const disabled = error !== null && /HTTP 404|no such endpoint/i.test(error);

  return (
    <section>
      <div className={ui.head}>
        <h1>Calibration</h1>
        <span className={ui.sub}>observe-mode replay and observed flags</span>
      </div>

      {disabled ? (
        <p className={ui.warning} role="status">
          Content screening is not enabled on this deployment: the chart’s{' '}
          <code>screening.enabled</code> is off, so there is nothing to calibrate (docs/71).
        </p>
      ) : (
        <>
          {error ? <p className={ui.error}>{error}</p> : null}
          <ScreeningBanners status={data?.screening ?? null} />
          {data && data.screening.expectedShards === 0 && !data.screening.lastPollAt ? (
            <p className={ui.dim}>No sampler has polled this deployment yet.</p>
          ) : null}
          <ReplayPanel />
          <ObservedFlags page={data} loading={loading} now={loadedAt} />
        </>
      )}
    </section>
  );
}

/** `datetime-local`'s value for an instant: local wall time, minute precision. */
function toLocalInput(ms: number): string {
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function defaultRange(): { from: string; to: string } {
  const to = Date.now();
  return { from: toLocalInput(to - DEFAULT_RANGE_DAYS * 86_400_000), to: toLocalInput(to) };
}

/**
 * The form's draft as a request, or the sentence that says why it is not one.
 * Mirrors the server's own refusals (D21) so the operator learns them before a
 * round trip — the server is still the authority, and the one check only it
 * can make, a threshold under the stored floor, comes back as its 400.
 */
function toRequest(draft: {
  threshold: string;
  window: string;
  hits: string;
  labels: string;
  from: string;
  to: string;
}): ReplayRequest | string {
  const threshold = Number(draft.threshold);
  const window = Number(draft.window);
  const windowHits = Number(draft.hits);
  if (!(threshold > 0 && threshold <= 1)) return 'The threshold must be above 0 and at most 1.';
  if (!Number.isInteger(window) || window < 1 || window > MAX_WINDOW) {
    return `The window must be a whole number of samples from 1 to ${MAX_WINDOW}.`;
  }
  if (!Number.isInteger(windowHits) || windowHits < 1 || windowHits > window) {
    return 'The hits must be a whole number from 1 to the window size.';
  }
  const from = Date.parse(draft.from);
  const to = Date.parse(draft.to);
  if (!Number.isFinite(from) || !Number.isFinite(to) || from >= to) {
    return 'The range must start before it ends.';
  }
  const labels = draft.labels
    .split(/[\s,]+/)
    .map((l) => l.trim().toUpperCase())
    .filter(Boolean);
  return {
    threshold,
    window,
    windowHits,
    ...(labels.length ? { labels } : {}),
    from: new Date(from).toISOString(),
    to: new Date(to).toISOString(),
  };
}

function ReplayPanel() {
  const api = useApi();
  const [threshold, setThreshold] = useState(DEFAULT_THRESHOLD);
  const [size, setSize] = useState(DEFAULT_WINDOW);
  const [hits, setHits] = useState(DEFAULT_HITS);
  const [labels, setLabels] = useState('');
  const [range, setRange] = useState(defaultRange);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<ReplayResponse | null>(null);

  async function run() {
    const req = toRequest({ threshold, window: size, hits, labels, from: range.from, to: range.to });
    if (typeof req === 'string') {
      setError(req);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      setResult(await api.replay(req));
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className={ui.panel}>
      <h2>Replay a setting</h2>
      <p className={ui.dim}>
        Runs the sampler’s window rule over the samples stored in the range: a broadcast trips
        when at least <em>hits</em> of its last <em>window</em> samples score at or above the
        threshold. Samples below the sampler’s floor are not stored, so a threshold under it is
        refused.
      </p>
      <div className={ui.grid2}>
        <div className={ui.field}>
          <label htmlFor="cal-threshold">Threshold (0–1)</label>
          <input
            id="cal-threshold"
            type="number"
            step="0.01"
            min={0}
            max={1}
            value={threshold}
            onChange={(e) => setThreshold(e.target.value)}
          />
        </div>
        <div className={ui.field}>
          <label htmlFor="cal-labels">Labels (blank = every flagged label)</label>
          <input
            id="cal-labels"
            className={ui.mono}
            placeholder="FEMALE_GENITALIA_EXPOSED, ANUS_EXPOSED"
            value={labels}
            onChange={(e) => setLabels(e.target.value)}
          />
        </div>
        <div className={ui.field}>
          <label htmlFor="cal-window">Window (samples)</label>
          <input
            id="cal-window"
            type="number"
            min={1}
            max={MAX_WINDOW}
            value={size}
            onChange={(e) => setSize(e.target.value)}
          />
        </div>
        <div className={ui.field}>
          <label htmlFor="cal-hits">Hits to trip</label>
          <input
            id="cal-hits"
            type="number"
            min={1}
            max={MAX_WINDOW}
            value={hits}
            onChange={(e) => setHits(e.target.value)}
          />
        </div>
        <div className={ui.field}>
          <label htmlFor="cal-from">From</label>
          <input
            id="cal-from"
            type="datetime-local"
            value={range.from}
            onChange={(e) => setRange((r) => ({ ...r, from: e.target.value }))}
          />
        </div>
        <div className={ui.field}>
          <label htmlFor="cal-to">To</label>
          <input
            id="cal-to"
            type="datetime-local"
            value={range.to}
            onChange={(e) => setRange((r) => ({ ...r, to: e.target.value }))}
          />
        </div>
      </div>
      {error ? (
        <p className={ui.error} role="alert">
          {error}
        </p>
      ) : null}
      <div className={ui.actions}>
        <button type="button" className={ui.primary} disabled={busy} onClick={() => void run()}>
          {busy ? 'Replaying…' : 'Replay'}
        </button>
      </div>
      {result ? <ReplayResult result={result} /> : null}
    </div>
  );
}

function ReplayResult({ result }: { result: ReplayResponse }) {
  const hours = result.broadcastHours;
  const rate = result.wouldTripsPer100h;
  return (
    <div aria-label="Replay result">
      <h3>Result</h3>
      <table className={`${ui.table} ${ui.kv}`}>
        <tbody>
          <tr>
            <th>Would-trips</th>
            <td>
              <strong>{result.wouldTrips}</strong>
            </td>
          </tr>
          <tr>
            <th>Per 100 broadcast-hours</th>
            <td>
              {hours > 0 ? (
                <>
                  <strong>{rate.toFixed(2)}</strong>{' '}
                  {rate < TARGET_PER_100H ? (
                    <span className={ui.badgeOk}>under the CF9 bar of {TARGET_PER_100H}</span>
                  ) : (
                    <span className={ui.badgeWarn}>at or over the CF9 bar of {TARGET_PER_100H}</span>
                  )}
                </>
              ) : (
                <span className={ui.dim}>no broadcast-hours in the range</span>
              )}
            </td>
          </tr>
          <tr>
            <th>Broadcast-hours</th>
            <td>{hours.toFixed(1)}</td>
          </tr>
          <tr>
            <th>Sessions</th>
            <td>{result.sessions}</td>
          </tr>
          <tr>
            <th>Samples taken / stored</th>
            <td>
              {result.samplesTaken} / {result.samplesStored}
            </td>
          </tr>
          <tr>
            <th>Sample floor</th>
            <td>{formatScore(result.floor)}</td>
          </tr>
        </tbody>
      </table>
      {result.trips.length > 0 ? (
        <div className={ui.scroll}>
          <table className={ui.table}>
            <thead>
              <tr>
                <th>Would have tripped</th>
                <th>Broadcast</th>
                <th>Key</th>
                <th>Peak</th>
              </tr>
            </thead>
            <tbody>
              {result.trips.map((t) => (
                <tr key={`${t.publisherEpoch}-${t.at}`}>
                  <td className={ui.nowrap}>{formatInstant(t.at)}</td>
                  <td className={ui.mono}>{t.broadcastId}</td>
                  <td className={ui.mono}>{t.broadcastKey}</td>
                  <td>{formatScore(t.peakScore)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className={ui.dim}>No broadcast would have tripped.</p>
      )}
      {result.truncated ? (
        <p className={ui.caveat}>
          Only the newest {result.trips.length} trips are listed; the counts above are exact.
        </p>
      ) : null}
    </div>
  );
}

/**
 * Observed flags, newest first. They are calibration data — never pinned,
 * never reminded, never acted on — but each still names a broadcast that may
 * be live, and Watch is how the operator judges one while it is on screen.
 */
function ObservedFlags({
  page,
  loading,
  now,
}: {
  page: ContentFlagPage | null;
  loading: boolean;
  now: number;
}) {
  const flags = page?.contentFlags ?? [];
  return (
    <div className={ui.panel}>
      <h2>Observed flags</h2>
      {flags.length === 0 ? (
        <p className={ui.dim}>
          {loading && !page ? 'Loading…' : 'No observed flags: nothing has tripped the window yet.'}
        </p>
      ) : (
        <div className={ui.scroll}>
          <table className={ui.table}>
            <thead>
              <tr>
                <th>Raised</th>
                <th>Broadcast</th>
                <th>State</th>
                <th>Peak</th>
                <th>Window</th>
                <th>Labels</th>
                <th>Links</th>
              </tr>
            </thead>
            <tbody>
              {flags.map((f) => (
                <tr key={f.id} className={f.state === 'resolved' ? ui.recessed : undefined}>
                  <td className={ui.nowrap}>{formatInstant(f.raisedAt)}</td>
                  <td className={ui.mono}>{f.broadcastId}</td>
                  <td>
                    {f.state === 'resolved' ? (
                      <span className={ui.badge}>{describeResolution(f)}</span>
                    ) : (
                      <span
                        className={ui.badgeWarn}
                        title={`last sampler report ${formatInstant(f.lastPostAt)}`}
                      >
                        tripped{f.signal === 'unknown' ? ' · signal unknown' : ''}
                      </span>
                    )}
                  </td>
                  <td>{formatScore(f.peakScore)}</td>
                  <td>
                    {f.window.flaggedSamples} of {f.window.samples}
                  </td>
                  <td className={ui.mono}>{describeLabels(f)}</td>
                  <td>
                    {f.links?.watch ? (
                      <a href={f.links.watch} target="_blank" rel="noreferrer">
                        Watch
                      </a>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {page && now ? (
        <p className={ui.caveat}>Updated {formatInstant(new Date(now).toISOString())}.</p>
      ) : null}
    </div>
  );
}
