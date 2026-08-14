import { describe, expect, it } from 'vitest';

import { normalizeRelayOrigin, sameRelayOrigin } from './relayUrl';

// R37 (docs/40 §4.2): the value matrix for the shared relay-URL rule. The
// same function backs every store write path and the `?relay=` grammar, so
// this table is the SP1/SP2 "valid/invalid value matrix" criterion.
describe('normalizeRelayOrigin', () => {
  it.each([
    ['https://relay.example.com:4433', 'https://relay.example.com:4433'],
    ['https://relay.example.com', 'https://relay.example.com'],
    // Lowercasing, default-port elision, trailing slash — all origin rules.
    ['https://Relay.Example.Com:4433/', 'https://relay.example.com:4433'],
    ['https://relay.example.com:443', 'https://relay.example.com'],
    ['  https://relay.example.com:4433  ', 'https://relay.example.com:4433'],
    ['https://localhost:4433', 'https://localhost:4433'],
    ['https://[::1]:4433', 'https://[::1]:4433'],
  ])('normalizes %s to %s', (input, expected) => {
    expect(normalizeRelayOrigin(input)).toBe(expected);
  });

  it.each([
    [''],
    ['   '],
    ['not a url'],
    ['relay.example.com:4433'], // scheme required
    ['http://relay.example.com:4433'], // https only
    ['ws://relay.example.com:4433'],
    ['https://user:pw@relay.example.com:4433'], // smuggled credential (D3)
    ['https://user@relay.example.com:4433'],
    ['https://relay.example.com:4433/subscribe'], // no path
    ['https://relay.example.com:4433/?x=1'], // no query
    ['https://relay.example.com:4433/#frag'], // no fragment
  ])('rejects %s', (input) => {
    expect(normalizeRelayOrigin(input)).toBe(null);
  });
});

describe('sameRelayOrigin', () => {
  it('matches across case and trailing slash (F8)', () => {
    expect(sameRelayOrigin('https://Relay.Example.com:4433/', 'https://relay.example.com:4433')).toBe(true);
  });

  it('never matches an unparseable value, even against itself', () => {
    expect(sameRelayOrigin('nonsense', 'nonsense')).toBe(false);
  });
});
