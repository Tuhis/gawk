import { create } from 'zustand';

import { fetchFields, fetchMeta } from '../api/client.ts';
import type { FieldDoc, Meta } from '../api/types.ts';

// What this deployment can do, asked ONCE at boot.
//
// Two things live here and neither belongs anywhere else:
//
//   * **Capabilities.** Annotations and the SQL console are optional, and an
//     ops page that renders an affordance for something nobody enabled is a
//     page that teaches its operator not to trust it. Asked once, not
//     discovered by requesting a thing and reading the failure.
//   * **The field catalogue.** UD8's whole point: the UI never carries a second
//     copy of `schema.ViewerFields`. A field added to the Go tables appears in
//     the picker with no change here, which is only true if the picker's source
//     is this fetch.
//
// The clock check is small and worth it. Every historical timestamp on the page
// is absolute (UD5), and a browser whose clock disagrees with the service's
// would shift all of them silently. Stating the skew is cheaper than debugging
// the confusion it causes.

/** Above this, the browser's clock is worth mentioning. */
export const CLOCK_SKEW_WARN_MS = 60_000;

interface MetaState {
  meta: Meta | null;
  fields: FieldDoc[];
  /** browserNow − serverNow at load. Positive: this browser runs ahead. */
  clockSkewMs: number;
  error: string | null;
  load: () => Promise<void>;
}

export const useMetaStore = create<MetaState>((set) => ({
  meta: null,
  fields: [],
  clockSkewMs: 0,
  error: null,

  load: async () => {
    try {
      const before = Date.now();
      const [meta, fields] = await Promise.all([fetchMeta(), fetchFields()]);
      // The round trip is split in half and removed, so a slow port-forward is
      // not reported as a clock problem.
      const rtt = (Date.now() - before) / 2;
      set({
        meta,
        fields,
        clockSkewMs: Math.round(Date.now() - rtt - meta.serverNowMs),
        error: null,
      });
    } catch (e) {
      // A missing /v1/meta means an older backend, not a broken page. Every
      // optional surface then stays hidden, which is the correct degradation.
      set({ error: e instanceof Error ? e.message : String(e) });
    }
  },
}));

/** The catalogue entry for a field, or undefined if this build has no type. */
export function fieldDoc(fields: FieldDoc[], name: string): FieldDoc | undefined {
  return fields.find((f) => f.name === name);
}

/**
 * Fields this role reports, current spellings first.
 *
 * Legacy capitalized spellings are kept — sessions on disk use them for the
 * whole raw window — but grouped after the current ones so a picker does not
 * offer `SentFps` beside `sentFps` as though they were different measurements.
 */
export function fieldsForRole(fields: FieldDoc[], role: string): FieldDoc[] {
  return fields
    .filter((f) => f.roles.includes(role))
    .sort((a, b) => Number(a.legacy ?? false) - Number(b.legacy ?? false) || a.name.localeCompare(b.name));
}
