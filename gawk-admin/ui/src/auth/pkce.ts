// PKCE + nonce primitives for the portal's OIDC public-client flow (docs/42
// §4.8).
//
// Everything here is WebCrypto: `crypto.getRandomValues` for the unguessable
// values and `crypto.subtle.digest` for the S256 challenge. No dependency, and
// nothing that reaches another origin — the two properties the portal's CSP
// and the embedded-assets rule demand of the whole bundle.

/** base64url, unpadded — RFC 7636 §4.2's encoding for `code_challenge`. */
export function base64url(bytes: ArrayBuffer | Uint8Array): string {
  const view = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  let s = '';
  for (const b of view) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/**
 * A fresh unguessable string, used for the `code_verifier`, `state` and
 * `nonce`.
 *
 * 32 bytes -> 43 base64url characters, which is exactly RFC 7636's minimum
 * verifier length and comfortably above the 128-bit entropy the spec asks of
 * `state`. One generator for all three so none of them can accidentally be the
 * weak one.
 */
export function randomToken(bytes = 32): string {
  const buf = new Uint8Array(bytes);
  crypto.getRandomValues(buf);
  return base64url(buf);
}

/** The S256 `code_challenge` for a verifier. Plain `S256`, never `plain`. */
export async function codeChallengeS256(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier));
  return base64url(digest);
}
