// R4 automatic-fallback decision core (docs/09-automatic-fallback.md).
// Pure and timer-free: time is injected per call, never read (same
// testability discipline as FpsGate and KeyframeCadence). The controller
// decides *direction* only; the pipeline resolves it against autoLadder and
// reports back steps it could not apply (stepRejected). Ladder knowledge,
// auto-vs-explicit mode, and all actuation stay in the pipeline.

// Detection thresholds (Decision 2). Tuned in I4 on the real gaming PC —
// all constants live here and are exported for the tests.
//
// Outcomes older than this fall out of the sliding window.
export const WINDOW_MS = 4000;
// Never decide on fewer windowed outcomes (guards low-fps rungs: a full
// window at the 5 fps rung holds ~20).
export const MIN_SAMPLES = 15;
// Step down when ≥ this fraction of windowed frames were rejected. A
// struggling encoder converges on rejecting 1 − encodeRate/inputRate of
// frames, so 0.25 ≈ "running 25% behind".
export const TRIGGER_RATIO = 0.25;
// After any encoder recreate, outcomes are discarded for this long:
// renegotiation churn must not count, and the new rung gets a fair chance.
export const COOLDOWN_MS = 8000;

// Step-up probing (Decision 5).
//
// The healthy streak needs the rejection ratio below this, continuously.
export const RECOVERY_RATIO = 0.02;
// Base healthy period before an up-probe fires.
export const UP_PROBE_MS = 30_000;
// A step-down this soon after a step-up marks the probe failed (doubles the
// next probe interval); a probe surviving this window resets the interval.
export const UP_FAIL_WINDOW_MS = 60_000;
// Probe-interval backoff cap: steady overload pumps quality at most once
// per ~8 minutes instead of oscillating.
export const UP_PROBE_MAX_MS = 480_000;

// Encoder-error bounding (Decision 7): a second error this soon after an
// error-triggered reset means the problem is not resolution — fail.
export const ERROR_FAIL_WINDOW_MS = 10_000;

export type FallbackDecision = 'none' | 'stepDown' | 'stepUp';
export type StepDirection = 'down' | 'up';

interface Outcome {
  atMs: number;
  accepted: boolean;
}

export class FallbackController {
  private window: Outcome[] = [];
  private rejectedInWindow = 0;
  // Time of the first outcome in the current uninterrupted observation run;
  // step-down decisions additionally require a full window observed since
  // then, so a short 100% burst can't trigger before dilution is possible.
  private observedSinceMs = 0;
  private cooldownUntilMs = -Infinity;
  // Start of the current streak of ratio < RECOVERY_RATIO evaluations.
  private healthySinceMs: number | null = null;

  private probeMs = UP_PROBE_MS;
  // Set when a step-up was actually applied; used to classify a following
  // step-down as a failed probe. Cleared once the probe survives the fail
  // window (which also resets the interval) or on stepRejected('up').
  private lastStepUpAtMs: number | null = null;
  private lastErrorResetAtMs: number | null = null;

  // stepRejected latches: a step the pipeline could not apply must not
  // re-fire every frame. 'down' (floor / explicit-mode pressure) is cleared
  // by an emitted stepUp; 'up' (ceiling) by an emitted stepDown.
  private downLatched = false;
  private upLatched = false;

  get probeIntervalMs(): number {
    return this.probeMs;
  }

  // Feed one encode outcome; returns the decision for the pipeline to act
  // on (or ignore in explicit mode). Enters cooldown internally whenever it
  // emits a step.
  record(accepted: boolean, nowMs: number): FallbackDecision {
    this.resolveProbeSurvival(nowMs);
    if (nowMs < this.cooldownUntilMs) return 'none';

    while (this.window.length > 0 && this.window[0].atMs < nowMs - WINDOW_MS) {
      const expired = this.window.shift()!;
      if (!expired.accepted) this.rejectedInWindow--;
    }
    if (this.window.length === 0) {
      // Fresh observation run (startup, post-cooldown, or a frame-flow
      // stall). A stall also breaks the healthy streak: the probe needs
      // observed healthy frames, not mere elapsed time.
      this.observedSinceMs = nowMs;
      this.healthySinceMs = null;
    }
    this.window.push({ atMs: nowMs, accepted });
    if (!accepted) this.rejectedInWindow++;

    const ratio = this.rejectedInWindow / this.window.length;
    const decidable = this.window.length >= MIN_SAMPLES;

    if (
      ratio >= TRIGGER_RATIO &&
      decidable &&
      nowMs - this.observedSinceMs >= WINDOW_MS &&
      !this.downLatched
    ) {
      this.emitStep('down', nowMs);
      return 'stepDown';
    }

    if (ratio < RECOVERY_RATIO) {
      if (this.healthySinceMs === null) {
        this.healthySinceMs = nowMs;
      } else if (decidable && nowMs - this.healthySinceMs >= this.probeMs && !this.upLatched) {
        this.emitStep('up', nowMs);
        return 'stepUp';
      }
    } else {
      this.healthySinceMs = null;
    }
    return 'none';
  }

  // Decision 7 bounding. The auto/explicit split lives in the pipeline: it
  // ignores 'stepDown' outside auto mode and fails instead.
  onEncoderError(nowMs: number): 'stepDown' | 'fail' {
    this.resolveProbeSurvival(nowMs);
    if (this.downLatched) return 'fail'; // already at the floor
    if (this.lastErrorResetAtMs !== null && nowMs - this.lastErrorResetAtMs <= ERROR_FAIL_WINDOW_MS) {
      return 'fail';
    }
    this.lastErrorResetAtMs = nowMs;
    // The strongest possible backpressure evidence — same bookkeeping as a
    // ratio-triggered step-down, including failed-probe backoff.
    this.emitStep('down', nowMs);
    return 'stepDown';
  }

  // Called by the pipeline on any encoder recreate it initiated itself
  // (selection change, source-dimension change) so the cooldown and window
  // clear regardless of who caused the reset.
  noteReset(nowMs: number): void {
    this.downLatched = false;
    this.upLatched = false;
    this.enterCooldown(nowMs);
  }

  // The pipeline could not apply an emitted step (floor/ceiling, or an
  // explicit selection): latch so the decision doesn't re-fire every frame.
  stepRejected(direction: StepDirection): void {
    if (direction === 'down') {
      this.downLatched = true;
    } else {
      this.upLatched = true;
      // The step-up was never applied, so it is not a probe — a later
      // step-down must not count it as a probe failure.
      this.lastStepUpAtMs = null;
    }
  }

  private emitStep(direction: StepDirection, nowMs: number): void {
    if (direction === 'down') {
      if (this.lastStepUpAtMs !== null && nowMs - this.lastStepUpAtMs <= UP_FAIL_WINDOW_MS) {
        this.probeMs = Math.min(this.probeMs * 2, UP_PROBE_MAX_MS);
      }
      this.lastStepUpAtMs = null;
      this.upLatched = false;
    } else {
      this.lastStepUpAtMs = nowMs;
      this.downLatched = false;
    }
    this.enterCooldown(nowMs);
  }

  private enterCooldown(nowMs: number): void {
    this.cooldownUntilMs = nowMs + COOLDOWN_MS;
    this.window = [];
    this.rejectedInWindow = 0;
    this.healthySinceMs = null;
    this.observedSinceMs = nowMs;
  }

  private resolveProbeSurvival(nowMs: number): void {
    if (this.lastStepUpAtMs !== null && nowMs - this.lastStepUpAtMs > UP_FAIL_WINDOW_MS) {
      this.probeMs = UP_PROBE_MS;
      this.lastStepUpAtMs = null;
    }
  }
}
