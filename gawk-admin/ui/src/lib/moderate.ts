// The kill-and-ban sequence (docs/42 §4.9), shared by the Broadcasts view's
// Kill + ban dialog and the moderation timeline's Violation step (R70,
// docs/72 D10). One copy, because its ORDER is the point: a second copy is the
// copy that forgets it.

import { AuthRedirect } from '@gawk/oidc-session';

import { ApiError, enforcementNotice } from '../api/client.ts';
import type { ApiClient } from '../api/client.ts';
import type { Ban, CreateBanRequest } from '../api/types.ts';

/** What a kill-and-ban asks for: the dialog's and the Violation form's draft. */
export interface BanRequestDraft {
  reason: string;
  /** RFC3339, or null for a permanent ban. */
  expiresAt: string | null;
  /** null = do not ban the publisher IP. */
  ip: { prefixLength: number } | null;
}

/** How a kill-and-ban went. */
export interface KillAndBanOutcome {
  /** The error line when either write failed, null when both are in force. */
  failure: string | null;
  /** The amber 202 notice when a ban is recorded but not yet enforced. */
  pending: string | null;
  /** The confirmation line; meaningful when `failure` is null. */
  summary: string;
}

/**
 * Ban the broadcast — and, when the draft says so, its publisher IP — the way
 * docs/42 §4.9 requires. Never throws, except `AuthRedirect`: the session is
 * mid-redirect to the IdP and nothing more will run.
 */
export async function killAndBan(
  api: ApiClient,
  broadcastId: string,
  draft: BanRequestDraft,
): Promise<KillAndBanOutcome> {
  // The IP ban goes FIRST, and the order is the whole point.
  //
  // The literal "publisher" is §4.7's contract: the server resolves the
  // live publisher's address through relayscan rather than trusting an
  // address this page read seconds ago. That resolve therefore has to run
  // before anything has started terminating the session — and the ID ban
  // is exactly that, twice over: it triggers the AP3 kill, and its handler
  // calls `afterMutation()`, which invalidates the relayscan fleet cache,
  // so an IP ban placed after it does a *fresh* scan racing that kill and
  // loses. Going first costs nothing, because the IP ban's own AP3
  // actuation ends the session just the same.
  const ip = draft.ip
    ? await attemptBan(api, {
        target: { type: 'ip', value: 'publisher', prefixLength: draft.ip.prefixLength },
        expiresAt: draft.expiresAt,
        reason: draft.reason,
        sourceBroadcastId: broadcastId,
      })
    : null;
  // And the ID ban is attempted WHATEVER happened to it. It is the action
  // that must land — the relay kills every live publisher matching a new
  // Ban (AP3), so it both stops the broadcast now and keeps it stopped —
  // and letting a 503 on the IP half abort it would leave the operator
  // with a live broadcast and nothing enforced at all.
  const id = await attemptBan(api, {
    target: { type: 'broadcastId', value: broadcastId },
    expiresAt: draft.expiresAt,
    reason: draft.reason,
    sourceBroadcastId: broadcastId,
  });
  // Either write can come back 202 on its own; one unenforced ban is
  // enough to make this amber, so the first notice wins.
  const pending = pendingNotice(broadcastId, banOf(id)) ?? pendingNotice(broadcastId, banOf(ip));
  return {
    failure: partialFailure(broadcastId, id, ip),
    pending,
    summary: banSummary(broadcastId, id.ok && id.existed, ip === null ? null : ip.ok && ip.existed),
  };
}

/**
 * The result of one `POST /bans`, as an outcome rather than an exception.
 *
 * Kill + ban runs two of these and the second must not be skipped because the
 * first failed, so neither may throw.
 */
export type BanAttempt =
  | { ok: true; ban: Ban | null; existed: boolean }
  | { ok: false; error: string };

/**
 * Place a ban, treating `409 duplicate_active` as "already in force" and every
 * other failure as an outcome to report.
 *
 * A 409 is the one refusal that is not a refusal: it means the target already
 * carries an active ban — the state the operator is asking for — and it comes
 * back WITH that ban, so there is nothing to guess. That is what makes a retry
 * idempotent in either direction: whichever of the two writes already landed
 * is tolerated, and only the missing one is really re-sent.
 */
export async function attemptBan(api: ApiClient, req: CreateBanRequest): Promise<BanAttempt> {
  try {
    return { ok: true, ban: await api.createBan(req), existed: false };
  } catch (err) {
    // Not an outcome: the session is mid-redirect to the IdP and nothing more
    // will run. Rendering it as a failed half would tell the operator a lie
    // on the way out of the page.
    if (err instanceof AuthRedirect) throw err;
    if (err instanceof ApiError && err.code === 'duplicate_active') {
      return { ok: true, ban: err.ban, existed: true };
    }
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export function banOf(attempt: BanAttempt | null): Ban | null {
  return attempt !== null && attempt.ok ? attempt.ban : null;
}

/**
 * The error line when kill + ban did not land in full, or null when it did.
 *
 * It has to name which half is in force, because that is what decides what the
 * operator does next — and, since `BansView` has no create form and a killed
 * broadcast leaves the table, this dialog is the only place they can do it.
 */
export function partialFailure(
  id: string,
  idAttempt: BanAttempt,
  ipAttempt: BanAttempt | null,
): string | null {
  const idError = idAttempt.ok ? null : idAttempt.error;
  const ipError = ipAttempt === null || ipAttempt.ok ? null : ipAttempt.error;
  if (idError === null && ipError === null) return null;
  if (idError !== null && ipError !== null) {
    return `Nothing was banned. The broadcast: ${idError}. The publisher IP: ${ipError}.`;
  }
  if (idError !== null) {
    return ipAttempt === null
      ? idError
      : `The publisher IP is banned, but ${id} is not: ${idError}. Kill and ban again to retry it — the IP ban already in place will not be duplicated.`;
  }
  return `${id} is banned, but its publisher IP is not: ${ipError}. Kill and ban again to retry it — the broadcast ban already in place will not be duplicated.`;
}

/**
 * The confirmation line for a kill + ban.
 *
 * It has to stay true when a target was already banned. "Banned ABC123." would
 * claim this click did something it did not — in a portal whose whole job is
 * the moderation record — and a red error would claim nothing happened when
 * the state the operator asked for is the state that exists.
 */
export function banSummary(id: string, idExisted: boolean, ipExisted: boolean | null): string {
  if (ipExisted === null) return idExisted ? `${id} was already banned.` : `Banned ${id}.`;
  if (!idExisted && !ipExisted) return `Banned ${id} and its publisher IP.`;
  if (idExisted && ipExisted) return `${id} and its publisher IP were already banned.`;
  if (idExisted) return `Banned ${id}'s publisher IP; ${id} was already banned.`;
  return `Banned ${id}; its publisher IP was already banned.`;
}

/**
 * A ban that was RECORDED but is not yet ENFORCED — the server's `202
 * Accepted`, not an error.
 *
 * The Postgres row is committed; only the projection to a Ban CR failed, and
 * the reconciler will heal it. So this is neither a plain success nor a
 * failure, and calling it either would mislead: "failed" invites a retry that
 * will now 409 against the row that does exist, and "done" claims an
 * enforcement that has not started. The sentence is the server's — it is the
 * side that knows which way the two are out of step — prefixed with the
 * broadcast so the operator knows which row it is about.
 */
export function pendingNotice(id: string, ban: Ban | null | undefined): string | null {
  const detail = enforcementNotice(ban);
  return detail === null ? null : `${id} — ${detail} The broadcast stays live until it succeeds.`;
}
