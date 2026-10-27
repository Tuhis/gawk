// R81 VX0: the talk gates the spike compares (docs/82 §12), one decision per
// 10 ms block. `fixedGate` is docs/81 D9 as it ships (a fixed level floor);
// `trackedGate` is the candidate: the floor follows the speaker's own level,
// learned from blocks RNNoise is confident are speech. talkGate.ts ports the
// tracker and the gate from here; their unit tests pin the same numbers.
// The defaults below are the sweep's; what ships (docs/82 D7, D8) is
// `tracked-m12-q0.85-hl15`: margin 12, cap -30, hyst 0, q 0.85, 15 s.

export const BLOCK_MS = 10;
export const ABS_FLOOR_DB = -50;

const MIN_DB = -80;
const BINS = 81; // 1 dB bins, -80..0 dBFS

// The speaker's level: a high quantile of the denoised level of confident
// speech blocks, over a histogram whose weight decays with time (half-life
// halfLifeS). Learns from every confident block, sent or not, so the floor
// can't ratchet itself up; null until minMass blocks of evidence.
export class LevelTracker {
  constructor({ q = 0.85, halfLifeS = 300, minMass = 300, confident = 0.95, prior = null, priorMass = 300 } = {}) {
    this.q = q;
    this.decay = 2 ** (-(BLOCK_MS / 1000) / halfLifeS);
    this.minMass = minMass;
    this.confident = confident;
    this.bins = new Float64Array(BINS);
    this.mass = 0;
    if (prior !== null) this.add(prior, priorMass);
  }

  add(db, w) {
    const i = Math.max(0, Math.min(BINS - 1, Math.round(db) - MIN_DB));
    this.bins[i] += w;
    this.mass += w;
  }

  update(prob, levelDb, eligible = true) {
    for (let i = 0; i < BINS; i++) this.bins[i] *= this.decay;
    this.mass *= this.decay;
    if (eligible && prob >= this.confident && levelDb >= ABS_FLOOR_DB) this.add(levelDb, 1);
  }

  level() {
    if (this.mass < this.minMass) return null;
    const target = this.q * this.mass;
    let acc = 0;
    for (let i = 0; i < BINS; i++) {
      acc += this.bins[i];
      if (acc >= target) return i + MIN_DB;
    }
    return MIN_DB + BINS - 1;
  }
}

const dbOf = (rms) => (rms > 1e-5 ? 20 * Math.log10(rms) : -100);

// docs/81 D9 as shipped: prob >= on opens, >= off stays, the denoised block
// must be at least floorDb loud, hold after the last speech block.
export function fixedGate(prob, outRms, { on = 0.9, off = 0.7, holdMs = 300, floorDb = ABS_FLOOR_DB } = {}) {
  const g = new Uint8Array(prob.length);
  let open = false;
  let held = 1e9;
  for (let f = 0; f < prob.length; f++) {
    const p = dbOf(outRms[f]) < floorDb ? 0 : prob[f];
    const speech = p >= on || (open && p >= off);
    open = speech;
    held = speech ? 0 : held + 1;
    g[f] = speech || held < holdMs / BLOCK_MS ? 1 : 0;
  }
  return g;
}

// The candidate: the floor is the learned level minus `margin`, clamped to
// [ABS_FLOOR_DB, cap]; while open, `hyst` dB less is enough to stay open.
// Returns the decisions and the floor per block (for the meter's grey).
export function trackedGate(prob, outRms, { on = 0.9, off = 0.7, holdMs = 300, margin = 20, cap = -30, hyst = 6, tracker = {} } = {}) {
  const t = new LevelTracker(tracker);
  const g = new Uint8Array(prob.length);
  const floors = new Float32Array(prob.length);
  let open = false;
  let held = 1e9;
  for (let f = 0; f < prob.length; f++) {
    const lv = dbOf(outRms[f]);
    const learned = t.level();
    const floorOpen = learned === null ? ABS_FLOOR_DB : Math.min(cap, Math.max(ABS_FLOOR_DB, learned - margin));
    const floor = open ? Math.max(ABS_FLOOR_DB, floorOpen - hyst) : floorOpen;
    floors[f] = floorOpen;
    const p = lv < floor ? 0 : prob[f];
    const speech = p >= on || (open && p >= off);
    open = speech;
    held = speech ? 0 : held + 1;
    g[f] = speech || held < holdMs / BLOCK_MS ? 1 : 0;
    t.update(prob[f], lv);
  }
  return { gate: g, floors, finalLevel: t.level() };
}
