// What this tab knows about local relays (R74, docs/76 §6.12 and §7.3): the
// state of the switch, the one-line copy the session screens show, and the
// LocalRelayInfo of the PoP this tab is talking to. Memory only, except the
// opt-outs, which switch.ts persists itself.

import { create } from 'zustand';

import type { LocalRelayInfoMessage, LocalRelayOfferMessage } from '../../transport/wire';
import type { LocalNetworkPermission } from './permission';

// idle      no offer acted on
// probing   dialing the candidates
// switched  this tab is on the PoP (transportStore source 'pop')
// denied    the browser's permission is denied; staying on home
// failed    no candidate answered; staying on home for this tab
// deferred  a live broadcaster's offer, kept for its next broadcast
export type PopPhase = 'idle' | 'probing' | 'switched' | 'denied' | 'failed' | 'deferred';

export interface PopState {
  phase: PopPhase;
  // The offer being probed, deferred, or followed.
  offer: LocalRelayOfferMessage | null;
  offerAt: number;
  // The toast beside the browser's prompt (item 43.4, §7.3).
  toast: string | null;
  // The quiet note: denied, failed on a Mac, or "your next broadcast".
  note: string | null;
  // An offer from a PoP the user opted out of, so the chip can offer it back.
  declined: { handle: string; name: string } | null;
  // The LocalRelayInfo (0x18) last received, and the relay that sent it.
  info: { relayUrl: string; info: LocalRelayInfoMessage } | null;
  // The last local-network permission answer ('unknown' until asked).
  permission: LocalNetworkPermission;
  // Whether this tab is broadcasting: a live broadcast is never moved.
  broadcasterLive: boolean;
}

export const INITIAL_POP_STATE: PopState = {
  phase: 'idle',
  offer: null,
  offerAt: 0,
  toast: null,
  note: null,
  declined: null,
  info: null,
  permission: 'unknown',
  broadcasterLive: false,
};

export const usePopStore = create<PopState>(() => ({ ...INITIAL_POP_STATE }));
