// Speaking indicators (R79, docs/80 D16, OD14): worked out locally from the
// VoiceFrames this browser receives, never sent to the relay. A speaker is
// speaking from their first frame, held SPEAKING_HOLD_MS after their last one
// (frames come every 10–20 ms, so the hold rides over a lost datagram), and
// cleared at once on the end-of-transmission flag. Pure: the owner calls
// expire() when nextExpiryMs() comes due.

export const SPEAKING_HOLD_MS = 250;

export class SpeakingTracker {
  private readonly lastFrameAt = new Map<number, number>();
  private readonly holdMs: number;

  constructor(holdMs = SPEAKING_HOLD_MS) {
    this.holdMs = holdMs;
  }

  // One received frame. Returns whether the speaking set changed.
  frame(session: number, end: boolean, nowMs: number): boolean {
    if (end) return this.lastFrameAt.delete(session);
    const had = this.lastFrameAt.has(session);
    this.lastFrameAt.set(session, nowMs);
    return !had;
  }

  // Drops everyone whose hold has run out. Returns whether the set changed.
  expire(nowMs: number): boolean {
    let changed = false;
    for (const [session, at] of this.lastFrameAt) {
      if (nowMs - at >= this.holdMs) {
        this.lastFrameAt.delete(session);
        changed = true;
      }
    }
    return changed;
  }

  // When the next hold runs out, or null when nobody is speaking.
  nextExpiryMs(): number | null {
    let next: number | null = null;
    for (const at of this.lastFrameAt.values()) {
      const due = at + this.holdMs;
      if (next === null || due < next) next = due;
    }
    return next;
  }

  remove(session: number): boolean {
    return this.lastFrameAt.delete(session);
  }

  clear(): void {
    this.lastFrameAt.clear();
  }

  speaking(): number[] {
    return [...this.lastFrameAt.keys()].sort((a, b) => a - b);
  }
}
