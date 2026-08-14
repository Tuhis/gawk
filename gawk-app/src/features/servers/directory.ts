// R37 (docs/40 §4.5): the server directory — an operator-configured JSON
// document the picker offers. Fetched when the picker opens, never at boot
// (D9, the termsUrl rule); errors and timeouts degrade to "directory
// unavailable" without blocking the panel's own content. Entries are offers,
// not saves: adding one is the same explicit act as manual entry, and no
// credential fields exist in the schema.

import { normalizeRelayOrigin } from '../../lib/relayUrl';

export const DIRECTORY_FETCH_TIMEOUT_MS = 3000;
export const DIRECTORY_MAX_LABEL_LEN = 64;

export interface DirectoryOffer {
  label: string;
  // Normalized https origin — the same rule as ?relay= values.
  url: string;
  // Display groundwork for §4.9; no behaviour in R37.
  managed: boolean;
}

// null = directory unavailable (fetch failed, timed out, wrong version,
// or unparseable). Individually invalid entries are dropped, not fatal.
export async function fetchServerDirectory(
  directoryUrl: string,
  fetchFn: typeof fetch = fetch,
): Promise<DirectoryOffer[] | null> {
  const abort = new AbortController();
  const timer = setTimeout(() => abort.abort(), DIRECTORY_FETCH_TIMEOUT_MS);
  let parsed: unknown;
  try {
    const res = await fetchFn(directoryUrl, { cache: 'no-store', signal: abort.signal });
    if (!res.ok) return null;
    parsed = await res.json();
  } catch {
    return null;
  } finally {
    clearTimeout(timer);
  }
  if (typeof parsed !== 'object' || parsed === null) return null;
  const doc = parsed as { version?: unknown; servers?: unknown };
  if (doc.version !== 1 || !Array.isArray(doc.servers)) return null;

  const offers: DirectoryOffer[] = [];
  for (const item of doc.servers) {
    if (typeof item !== 'object' || item === null) continue;
    const entry = item as { label?: unknown; url?: unknown; managed?: unknown };
    const url = typeof entry.url === 'string' ? normalizeRelayOrigin(entry.url) : null;
    if (url === null) continue; // same validation rule as ?relay=; drop individually
    const label =
      typeof entry.label === 'string' && entry.label.trim() !== ''
        ? entry.label.trim().slice(0, DIRECTORY_MAX_LABEL_LEN)
        : new URL(url).host;
    // Credential-bearing or unknown extra fields are ignored by construction:
    // only these three are ever read.
    offers.push({ label, url, managed: entry.managed === true });
  }
  return offers;
}
