import { describe, expect, it } from 'vitest';

import { defaultPrefixLength, parseIp, prefixChoices, sharedIpVerdict } from './ip.ts';
import type { Broadcast } from '../api/types.ts';

function broadcast(id: string, ip: string | null): Broadcast {
  return {
    id,
    key: id.toLowerCase(),
    publisherActive: true,
    publisherRemoteIp: ip,
    startedAt: new Date().toISOString(),
    viewersGlobal: 0,
    pods: [],
  };
}

describe('parseIp', () => {
  it('accepts a bare address of either family', () => {
    expect(parseIp('203.0.113.7')).toEqual({ ip: '203.0.113.7', family: 'v4' });
    expect(parseIp('2001:db8::1')).toEqual({ ip: '2001:db8::1', family: 'v6' });
  });

  it('strips a port, because a /32 next to "1.2.3.4:51234" would be a lie', () => {
    expect(parseIp('203.0.113.7:51234')?.ip).toBe('203.0.113.7');
    expect(parseIp('[2001:db8::1]:443')).toEqual({ ip: '2001:db8::1', family: 'v6' });
  });

  it('treats an IPv4-mapped address as v4', () => {
    // Otherwise the dialog would offer a /64 over the mapped space.
    expect(parseIp('::ffff:203.0.113.7')).toEqual({ ip: '203.0.113.7', family: 'v4' });
  });

  it('returns null for nothing at all, so no IP ban is offered', () => {
    expect(parseIp(null)).toBeNull();
    expect(parseIp('')).toBeNull();
    expect(parseIp('not-an-address')).toBeNull();
    expect(parseIp('300.1.1.1')).toBeNull();
  });
});

describe('default prefixes (§4.9)', () => {
  it('is /32 for v4 and /64 for v6', () => {
    // /128 is near-useless: privacy-address rotation hands the same machine a
    // new address whenever it likes.
    expect(defaultPrefixLength('v4')).toBe(32);
    expect(defaultPrefixLength('v6')).toBe(64);
  });

  it('offers the narrowest choice first', () => {
    expect(prefixChoices('v4')[0]).toBe(32);
    expect(prefixChoices('v6')[0]).toBe(64);
  });
});

describe('the shared-publisher-IP heuristic (§4.9)', () => {
  it('warns when more than half the live broadcasts share the address', () => {
    const fleet = [
      broadcast('A', '198.51.100.9'),
      broadcast('B', '198.51.100.9'),
      broadcast('C', '203.0.113.7'),
    ];
    const v = sharedIpVerdict(fleet, '198.51.100.9');
    expect(v).toMatchObject({ sharing: 2, total: 3, warn: true });
  });

  it('stays quiet at exactly half — the rule is *more* than half', () => {
    const fleet = [
      broadcast('A', '198.51.100.9'),
      broadcast('B', '198.51.100.9'),
      broadcast('C', '203.0.113.7'),
      broadcast('D', '203.0.113.8'),
    ];
    expect(sharedIpVerdict(fleet, '198.51.100.9').warn).toBe(false);
  });

  it('does not warn on a single live broadcast', () => {
    // The ratio is trivially 1.0 and carries no evidence either way; warning
    // there would train the operator to ignore the warning.
    const fleet = [broadcast('A', '198.51.100.9')];
    expect(sharedIpVerdict(fleet, '198.51.100.9')).toMatchObject({ ratio: 1, warn: false });
  });

  it('ignores broadcasts with no known publisher IP', () => {
    const fleet = [
      broadcast('A', '198.51.100.9'),
      broadcast('B', '198.51.100.9'),
      broadcast('C', null),
      broadcast('D', null),
    ];
    expect(sharedIpVerdict(fleet, '198.51.100.9')).toMatchObject({ total: 2, warn: true });
  });

  it('matches through ports, so one row with a port does not hide the collision', () => {
    const fleet = [
      broadcast('A', '198.51.100.9'),
      broadcast('B', '198.51.100.9:41234'),
      broadcast('C', '203.0.113.7'),
    ];
    expect(sharedIpVerdict(fleet, '198.51.100.9').sharing).toBe(2);
  });
});
