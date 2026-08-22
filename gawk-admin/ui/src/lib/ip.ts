// IP-ban honesty (docs/42 §4.9).
//
// Two small rules that drive real UI, both of them about not letting an
// operator fire a wider weapon than they meant to.

import type { Broadcast } from '../api/types.ts';

export type IpFamily = 'v4' | 'v6';

/**
 * Normalize whatever the relay reported into a bare address plus its family.
 *
 * `publisherRemoteIp` is an address, but addresses arrive from a `RemoteAddr`
 * often enough that a `host:port` or `[v6]:port` is worth surviving: displaying
 * `203.0.113.7:51234` beside a `/32` checkbox would be a lie about what the ban
 * covers. Returns null for anything unrecognizable, and the caller then offers
 * no IP ban at all.
 */
export function parseIp(raw: string | null | undefined): { ip: string; family: IpFamily } | null {
  if (!raw) return null;
  let s = raw.trim();
  if (!s) return null;

  // `[2001:db8::1]:443` -> `2001:db8::1`
  const bracketed = /^\[([^\]]+)\](?::\d+)?$/.exec(s);
  if (bracketed) s = bracketed[1];

  if (s.includes(':')) {
    // A single colon on an otherwise dotted string is v4 with a port.
    const parts = s.split(':');
    if (parts.length === 2 && parts[0].includes('.')) s = parts[0];
  }

  // An IPv4-mapped IPv6 address is an IPv4 host, and banning it as a /64 would
  // sweep in the whole mapped space.
  const mapped = /^::ffff:(\d{1,3}(?:\.\d{1,3}){3})$/i.exec(s);
  if (mapped) s = mapped[1];

  if (/^\d{1,3}(?:\.\d{1,3}){3}$/.test(s)) {
    if (s.split('.').some((o) => Number(o) > 255)) return null;
    return { ip: s, family: 'v4' };
  }
  // Loose on the v6 grammar on purpose: the relay produced it, the server
  // re-normalizes it (moderation.Normalize), and this only has to decide which
  // default prefix to offer.
  if (s.includes(':') && /^[0-9a-f:.]+$/i.test(s)) return { ip: s, family: 'v6' };
  return null;
}

/**
 * The prefix length the dialog pre-selects.
 *
 * v4 `/32` — one address, one publisher. v6 `/64` — privacy-address rotation
 * (RFC 8981) hands a client a new `/128` whenever it feels like it, so a
 * `/128` ban is near-useless while a `/64` is the smallest unit that actually
 * corresponds to a subscriber.
 */
export function defaultPrefixLength(family: IpFamily): number {
  return family === 'v4' ? 32 : 64;
}

/** The prefix lengths the dialog offers, widest last so a slip cannot widen it. */
export function prefixChoices(family: IpFamily): number[] {
  return family === 'v4' ? [32, 24] : [64, 56, 48];
}

export interface SharedIpVerdict {
  /** How many live broadcasts report this exact publisher IP. */
  sharing: number;
  /** How many live broadcasts report any publisher IP at all. */
  total: number;
  /** sharing / total, 0 when nothing is known. */
  ratio: number;
  /** Whether the dialog must show the fleet-wide-outage warning. */
  warn: boolean;
}

/**
 * The shared-publisher-IP heuristic (§4.9).
 *
 * When more than half the live broadcasts report the SAME publisher IP, the
 * load balancer is not preserving client addresses — `externalTrafficPolicy`
 * is not `Local` — and every publisher looks like the node or LB. An IP ban
 * then is not a targeted action, it is a fleet-wide outage switch, and the
 * relay cannot detect the misconfiguration itself. So the UI says it.
 *
 * `sharing >= 2` is part of the test, not a fudge: with a single live
 * broadcast the ratio is trivially 1.0 and carries no evidence either way.
 */
export function sharedIpVerdict(
  broadcasts: readonly Broadcast[],
  rawIp: string | null | undefined,
): SharedIpVerdict {
  const target = parseIp(rawIp)?.ip ?? null;
  let sharing = 0;
  let total = 0;
  for (const b of broadcasts) {
    const parsed = parseIp(b.publisherRemoteIp);
    if (!parsed) continue;
    total++;
    if (target && parsed.ip === target) sharing++;
  }
  const ratio = total === 0 ? 0 : sharing / total;
  return { sharing, total, ratio, warn: sharing >= 2 && ratio > 0.5 };
}
