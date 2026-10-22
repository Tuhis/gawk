// The party wall's Hide and Show for a broadcaster on a PoP (docs/76 item 34).
//
// Every party broadcast joins the PoP's wall at publish time. Hide sends an R42
// Detach for the broadcaster's own broadcast through the PoP, Show an Attach.
// The broadcaster did not attach in a control session of its own (the PoP
// vouched for it), so the session to the wall carries the broadcast's resume
// token as proof, the way a room mint does (`broadcast` + `resume`); the PoP
// checks it locally and forwards the command vouched. A remembered Hide
// travels as `wall=hidden` on the next /publish, so a hidden broadcaster never
// flashes onto the wall.

import { log } from '../../lib/logger';
import { readStored, writeStored } from '../../lib/storage';
import {
  RoomSession,
  type RoomSessionCallbacks,
  type RoomSessionOptions,
} from '../../transport/room-session';
import { ROOM_CLIENT_WEB_BROADCASTER, ROOM_EVENT_COMMAND_REJECTED } from '../../transport/wire';

export const WALL_HIDDEN_KEY = 'gawk.wallHidden';
const MAX_REMEMBERED = 32;
// How long the session waits after sending for the PoP to refuse it.
const WALL_COMMAND_SETTLE_MS = 750;
const WALL_COMMAND_TIMEOUT_MS = 10_000;

function readHidden(): string[] {
  try {
    const v: unknown = JSON.parse(readStored(WALL_HIDDEN_KEY) ?? '[]');
    return Array.isArray(v) ? v.filter((c): c is string => typeof c === 'string') : [];
  } catch {
    return [];
  }
}

function norm(code: string): string {
  return code.trim().toLowerCase();
}

export function isWallHidden(wallCode: string): boolean {
  return wallCode !== '' && readHidden().includes(norm(wallCode));
}

export function setWallHidden(wallCode: string, hidden: boolean): void {
  const code = norm(wallCode);
  if (code === '') return;
  const rest = readHidden().filter((c) => c !== code);
  writeStored(WALL_HIDDEN_KEY, JSON.stringify((hidden ? [...rest, code] : rest).slice(-MAX_REMEMBERED)));
}

export interface WallSessionLike {
  start(): Promise<void>;
  stop(): void;
  attach(broadcastId: string, resumeTokenHex: string, label: string): void;
  detach(broadcastId: string): void;
}

export type WallSessionFactory = (opts: RoomSessionOptions, cb: RoomSessionCallbacks) => WallSessionLike;

const defaultFactory: WallSessionFactory = (opts, cb) => new RoomSession(opts, cb);

export interface WallCommand {
  serverUrl: string;
  certHashHex: string;
  wallCode: string;
  broadcastId: string;
  resumeTokenHex: string;
  label: string;
  hide: boolean;
}

// Resolves true once the command went out and the PoP did not refuse it.
export function sendWallCommand(
  cmd: WallCommand,
  factory: WallSessionFactory = defaultFactory,
  settleMs: number = WALL_COMMAND_SETTLE_MS,
): Promise<boolean> {
  return new Promise<boolean>((resolve) => {
    let done = false;
    let sent = false;
    let session: WallSessionLike | null = null;
    const timers: ReturnType<typeof setTimeout>[] = [];
    const finish = (ok: boolean) => {
      if (done) return;
      done = true;
      for (const t of timers) clearTimeout(t);
      session?.stop();
      resolve(ok);
    };
    timers.push(setTimeout(() => finish(false), WALL_COMMAND_TIMEOUT_MS));
    session = factory(
      {
        serverUrl: cmd.serverUrl,
        certHashHex: cmd.certHashHex,
        target: { kind: 'join', code: cmd.wallCode },
        nickname: '',
        clientKind: ROOM_CLIENT_WEB_BROADCASTER,
        proof: { broadcastId: cmd.broadcastId, resumeTokenHex: cmd.resumeTokenHex },
      },
      {
        onConnected: () => {},
        onState: () => {
          if (sent || done) return;
          sent = true;
          if (cmd.hide) session?.detach(cmd.broadcastId);
          else session?.attach(cmd.broadcastId, cmd.resumeTokenHex, cmd.label);
          if (settleMs <= 0) finish(true);
          else timers.push(setTimeout(() => finish(true), settleMs));
        },
        onEvent: (ev) => {
          if (ev.kind === ROOM_EVENT_COMMAND_REJECTED) {
            log.info('party wall refused the command:', ev);
            finish(false);
          }
        },
        onReconnecting: () => {},
        onEnded: () => finish(sent),
        onError: () => finish(false),
      },
    );
    session.start().catch(() => finish(false));
  });
}
