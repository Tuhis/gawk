// The one relay-URL validation and normalization rule. Stored credentials
// attach by normalized-origin equality, which is only safe if everything that
// stores or compares a relay URL normalizes it the same way.
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
  // A smuggled credential is rejected outright rather than stripped.
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

// The host[:port] a relay URL names, for display. An unparseable value is
// shown as-is rather than hidden.
export function relayHost(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}
