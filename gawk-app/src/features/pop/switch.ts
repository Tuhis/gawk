// The switch to a local relay (R74, docs/76 item 43), the web client's half
// of PP14. Home sends a LocalRelayOffer (0x19) on any session kind to a client
// whose public address matches a linked PoP's. This module decides, once per
// fresh offer, whether to follow it:
//
//   1. Only a fresh offer switches. Nothing about a PoP is remembered except
//      consent (the opt-out); the followed PoP lives in transportStore memory,
//      so a laptop back home never dials a dead LAN address.
//   2. The candidates are probed over /echo in parallel; the first that
//      answers wins.
//   3. Viewers and room participants switch at once: the store change is the
//      session's one teardown and redial (one freeze of about a second). A
//      broadcaster switches only before going live; a live one is told its
//      next broadcast will use the PoP.
//   4. The browser's local-network permission decides how: granted switches
//      silently, prompt probes with a toast explaining the prompt, denied
//      stays home with a note, and a browser that knows neither name probes.
//   5. Failure stays on home and marks the PoP unreachable for this tab, with
//      no retry loop.
//   6. "Use … instead" is remembered per PoP handle until picked again.
//   7. A client that will follow offers says lr=1 on every CONNECT to home.

import { log } from '../../lib/logger';
import { readStored, writeStored } from '../../lib/storage';
import { bytesToHex } from '../../transport/connection';
import type { LocalRelayEvent } from '../../transport/local-relay';
import type { LocalRelayOfferMessage } from '../../transport/wire';
import { useTransportStore } from '../../state/transportStore';
import { probeRelay, sanitizeIdentityName, type ProbeOptions, type ProbeResult, type ProbeTransportFactory } from '../servers/probe';
import { queryLocalNetworkPermission, type LocalNetworkPermission } from './permission';
import { INITIAL_POP_STATE, usePopStore } from './popStore';

export type SessionRole = 'viewer' | 'room' | 'broadcaster';

// The opted-out PoP handles (the Pop CR's uid: random, stable across pods,
// restarts and re-pairs). Bounded so a wandering laptop cannot grow it.
export const OPT_OUT_KEY = 'gawk.popOptOut';
const MAX_OPT_OUTS = 32;

// A candidate probe in a browser that will prompt waits for the person to
// answer Chrome's prompt, far past the picker's 4 s handshake budget.
export const POP_PROMPT_PROBE_TIMEOUT_MS = 60_000;

export interface SwitchDeps {
  probe: (
    url: string,
    certHashHex: string,
    now?: () => number,
    factory?: ProbeTransportFactory,
    opts?: ProbeOptions,
  ) => Promise<ProbeResult>;
  queryPermission: () => Promise<LocalNetworkPermission>;
  now: () => number;
  isMac: () => boolean;
}

const defaultDeps: SwitchDeps = {
  probe: probeRelay,
  queryPermission: () => queryLocalNetworkPermission(),
  now: () => Date.now(),
  isMac: () => typeof navigator !== 'undefined' && /Macintosh|Mac OS X/.test(navigator.userAgent) && !/iPhone|iPad/.test(navigator.userAgent),
};

let deps: SwitchDeps = defaultDeps;
// PoPs whose candidates all failed in this tab (item 43.5).
const unreachable = new Set<string>();
// The PoP this tab last heard of (an offer, or the one it followed).
let lastHandle: string | null = null;

export function setSwitchDepsForTest(d: Partial<SwitchDeps>): void {
  deps = { ...defaultDeps, ...d };
}

export function resetSwitchForTest(): void {
  deps = defaultDeps;
  unreachable.clear();
  lastHandle = null;
  usePopStore.setState({ ...INITIAL_POP_STATE });
}

// ---- Copy (docs/76 §7.3; provisional until PP1) ------------------------------

// The home app's host as the guest knows it: "gawk.ioio.fi".
export function homeAppHost(): string {
  const info = usePopStore.getState().info?.info;
  if (info?.linked && info.homeOrigin !== '') {
    try {
      return new URL(info.homeOrigin).host;
    } catch {
      // fall through
    }
  }
  return typeof window === 'undefined' ? 'gawk' : window.location.host;
}

export function promptToast(name: string): string {
  return `${name} has a local relay. Your browser will ask to let ${homeAppHost()} connect to devices on this network. Allow it to stream and watch over the LAN.`;
}

export function deniedNote(): string {
  return `Your browser blocked the local relay. Streams still work through ${homeAppHost()}, over the party’s internet. To use the local relay, allow local network access for this site in the browser’s site settings, then reload.`;
}

export function nextBroadcastNote(name: string): string {
  return `${name} has a local relay. Your next broadcast will use it.`;
}

// item 44: Chrome on macOS needs the OS permission too.
export function macFailedNote(name: string): string {
  return `Couldn’t reach ${name}’s local relay. On a Mac, allow your browser under System Settings › Privacy & Security › Local Network.`;
}

export function offerName(offer: { name: string }): string {
  return sanitizeIdentityName(offer.name) || 'This party';
}

// ---- Opt-outs ----------------------------------------------------------------

function readOptOuts(): string[] {
  try {
    const v: unknown = JSON.parse(readStored(OPT_OUT_KEY) ?? '[]');
    return Array.isArray(v) ? v.filter((h): h is string => typeof h === 'string') : [];
  } catch {
    return [];
  }
}

function writeOptOuts(handles: string[]): void {
  writeStored(OPT_OUT_KEY, JSON.stringify(handles.slice(-MAX_OPT_OUTS)));
}

// ---- The switch ----------------------------------------------------------------

// Every session reader hands its 0x18 / 0x19 here.
export function noteLocalRelay(ev: LocalRelayEvent, role: SessionRole): void {
  if (ev.kind === 'info') {
    usePopStore.setState({ info: { relayUrl: useTransportStore.getState().serverUrl, info: ev.info } });
    return;
  }
  void considerOffer(ev.offer, role);
}

// lr=1 (item 43.7): this tab is on home and would follow an offer. Home
// holds a matching lr=1 session's demand for a moment (item 21), so a tab
// that has already given up on the PoP it was last offered (unreachable
// here, or opted out of) says nothing: it would only wait for no reason.
export function willFollowOffers(): boolean {
  if (useTransportStore.getState().resolvedSource === 'pop') return false;
  const s = usePopStore.getState();
  if (s.permission === 'denied') return false;
  const last = s.declined?.handle ?? s.offer?.handle ?? lastHandle;
  if (last != null && (unreachable.has(last) || readOptOuts().includes(last))) return false;
  return true;
}

// Asks once at startup so the first CONNECT's lr=1 already knows a denial.
export async function primeLocalNetworkPermission(): Promise<void> {
  try {
    const permission = await deps.queryPermission();
    usePopStore.setState({ permission });
  } catch {
    // stays 'unknown'
  }
}

function candidateHashes(c: { hashes: Uint8Array[] }): string[] {
  return c.hashes.map((h) => bytesToHex(h));
}

function expired(offer: LocalRelayOfferMessage, at: number): boolean {
  return deps.now() - at > offer.validity * 1000;
}

export async function considerOffer(offer: LocalRelayOfferMessage, role: SessionRole, at: number = deps.now()): Promise<void> {
  const store = usePopStore.getState();
  if (useTransportStore.getState().resolvedSource === 'pop') return;
  lastHandle = offer.handle;
  if (store.phase === 'probing') return;
  if (unreachable.has(offer.handle)) return;
  if (expired(offer, at)) return;
  const name = offerName(offer);
  if (readOptOuts().includes(offer.handle)) {
    usePopStore.setState({ declined: { handle: offer.handle, name }, offer, offerAt: at });
    return;
  }
  if (role === 'broadcaster' && store.broadcasterLive) {
    // A live broadcast is never moved (it would need cross-fleet resume).
    usePopStore.setState({ phase: 'deferred', offer, offerAt: at, note: nextBroadcastNote(name) });
    return;
  }

  usePopStore.setState({ phase: 'probing', offer, offerAt: at, note: null, declined: null });
  let permission: LocalNetworkPermission = 'unknown';
  try {
    permission = await deps.queryPermission();
  } catch {
    // probe as for an unknown name
  }
  usePopStore.setState({ permission });
  if (permission === 'denied') {
    usePopStore.setState({ phase: 'denied', note: deniedNote(), toast: null });
    return;
  }
  if (permission === 'prompt') usePopStore.setState({ toast: promptToast(name) });

  const probeOpts: ProbeOptions = permission === 'prompt' ? { connectTimeoutMs: POP_PROMPT_PROBE_TIMEOUT_MS } : {};
  const winner = await Promise.any(
    offer.candidates.map(async (c) => {
      const r = await deps.probe(c.url, candidateHashes(c).join(','), undefined, undefined, probeOpts);
      if (r.state !== 'ok') throw new Error(`no answer from ${c.url}`);
      return c;
    }),
  ).catch(() => null);

  usePopStore.setState({ toast: null });
  if (winner === null) {
    unreachable.add(offer.handle);
    log.info(`local relay ${name}: no candidate answered; staying on this server`);
    // A prompt answered "block" reads as a failure here; say so if it was.
    let after: LocalNetworkPermission = permission;
    if (permission === 'prompt') {
      after = await deps.queryPermission().catch(() => permission);
    }
    usePopStore.setState({
      phase: after === 'denied' ? 'denied' : 'failed',
      permission: after,
      note: after === 'denied' ? deniedNote() : deps.isMac() ? macFailedNote(name) : null,
    });
    return;
  }
  if (expired(offer, at) || useTransportStore.getState().resolvedSource === 'pop') {
    usePopStore.setState({ phase: 'idle' });
    return;
  }
  log.info(`local relay ${name}: switching to ${winner.url}`);
  useTransportStore.getState().setOfferOverride(winner.url, candidateHashes(winner), offer.handle, offer.name);
  usePopStore.setState({ phase: 'switched', note: null });
}

// docs/76 item 43.3: the web broadcaster probes the resolved relay's /echo
// when its screen opens, so home's offer arrives in time to switch before
// going live. The same probe reads a PoP's LocalRelayInfo, so a remembered
// party-wall Hide is known before the first /publish (wall=hidden). An
// offer is followed only by a tab that would follow one (lr=1).
export async function probeHomeForOffer(): Promise<void> {
  const follow = willFollowOffers();
  const t = useTransportStore.getState();
  const r = await deps.probe(t.serverUrl, t.certHashes.join(','), undefined, undefined, { followOffers: follow });
  if (r.state !== 'ok') return;
  if (r.localRelay) {
    usePopStore.setState({ info: { relayUrl: t.serverUrl, info: r.localRelay } });
  }
  if (follow && r.offer) await considerOffer(r.offer, 'broadcaster');
}

// The broadcaster screen reports going live and back. An offer kept while
// live is acted on once the broadcast ends, if it is still valid.
export function setBroadcasterLive(live: boolean): void {
  const s = usePopStore.getState();
  if (s.broadcasterLive === live) return;
  usePopStore.setState({ broadcasterLive: live });
  if (!live && s.phase === 'deferred' && s.offer) {
    const { offer, offerAt } = s;
    usePopStore.setState({ phase: 'idle', note: null });
    void considerOffer(offer, 'broadcaster', offerAt);
  }
}

// A followed PoP that answered its probe but then failed a session: back to
// home, and the PoP counts as unreachable for this tab like a failed probe
// (item 43.5), so home's next offer of it does not loop the tab back.
export function leaveUnreachableLocalRelay(): void {
  const t = useTransportStore.getState();
  const handle = t.offerOverride?.handle;
  if (!handle) return;
  unreachable.add(handle);
  lastHandle = handle;
  log.info(`local relay ${offerName(t.offerOverride!)}: a session failed there; back to this tab's server`);
  t.clearOfferOverride();
  usePopStore.setState({ phase: 'failed', note: null, toast: null });
}

// The chip's "Use gawk.ioio.fi instead" (item 43.6).
export function optOut(): void {
  const t = useTransportStore.getState();
  const handle = t.offerOverride?.handle;
  if (!handle) return;
  const name = offerName(t.offerOverride!);
  writeOptOuts([...readOptOuts().filter((h) => h !== handle), handle]);
  t.clearOfferOverride();
  usePopStore.setState({ phase: 'idle', offer: null, declined: { handle, name }, note: null, toast: null });
}

// Picking an opted-out PoP again: forget the opt-out and follow its last
// offer, if it is still valid.
export async function pickLocalRelayAgain(): Promise<void> {
  const s = usePopStore.getState();
  const handle = s.declined?.handle ?? s.offer?.handle;
  if (!handle) return;
  writeOptOuts(readOptOuts().filter((h) => h !== handle));
  usePopStore.setState({ declined: null });
  if (s.offer && s.offer.handle === handle) {
    await considerOffer(s.offer, s.broadcasterLive ? 'broadcaster' : 'viewer', s.offerAt);
  }
}

export function dismissPopToast(): void {
  usePopStore.setState({ toast: null });
}
