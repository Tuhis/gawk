// R37 (docs/40 §4.2): the one relay-URL validation + normalization rule,
// shared by every write path (store save/edit/migration, directory add) and
// by the `?relay=` link grammar — credential attachment is normalized-origin
// equality, and that test is only honest if everything that stores or
// compares a relay URL normalizes it the same way (F8).
//
// A relay value is an https origin and nothing else: no credentials, no
// path, no query, no fragment. `URL.origin` does the heavy lifting
// (lowercases the host, elides the default port, drops any trailing slash).

export function normalizeRelayOrigin(value: string): string | null {
  const trimmed = value.trim();
  if (trimmed === '') return null;
  let url: URL;
  try {
    url = new URL(trimmed);
  } catch {
    return null;
  }
  if (url.protocol !== 'https:') return null;
  // A smuggled credential is rejected outright rather than stripped — a link
  // carrying one is malformed by the grammar, not "close enough" (D3).
  if (url.username !== '' || url.password !== '') return null;
  if (url.pathname !== '/' && url.pathname !== '') return null;
  if (url.search !== '' || url.hash !== '') return null;
  return url.origin;
}

// True when two relay URL strings name the same origin under the shared
// normalization rule. Unparseable values are equal to nothing (not even
// themselves) — they can never attach credentials.
export function sameRelayOrigin(a: string, b: string): boolean {
  const na = normalizeRelayOrigin(a);
  return na !== null && na === normalizeRelayOrigin(b);
}
