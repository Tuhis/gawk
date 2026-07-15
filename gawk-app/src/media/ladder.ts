// The R3 broadcast ladder: what the broadcaster can choose to send, and the
// pure math turning a rung + an actual source frame into encode parameters.
// See docs/08-resolution-framerate-picker.md for the locked decisions.

export type ResolutionRung = 'native' | 1080 | 720 | 480;
export type FramerateRung = 'native' | 60 | 30 | 5;

// Native first — it is the default everywhere.
export const RESOLUTION_RUNGS: readonly ResolutionRung[] = ['native', 1080, 720, 480];
export const FRAMERATE_RUNGS: readonly FramerateRung[] = ['native', 60, 30, 5];

// R4 (docs/09): "auto" is a selection, not a rung — the picker's resolution
// axis. Everything downstream (computeTargetSize, the preprocessor) stays
// concrete; the pipeline resolves 'auto' to the current auto-ladder rung.
// Auto first — it is the new default.
export type ResolutionSelection = 'auto' | ResolutionRung;
export const RESOLUTION_SELECTIONS: readonly ResolutionSelection[] = ['auto', ...RESOLUTION_RUNGS];

// R12 (docs/17 Decision 4): the framerate axis gets the same shape. 'auto'
// resolves at probe time (framerate-first, resolveAutoFps below) and never
// runtime-steps — R4 stepping stays resolution-only.
export type FramerateSelection = 'auto' | FramerateRung;
export const FRAMERATE_SELECTIONS: readonly FramerateSelection[] = ['auto', ...FRAMERATE_RUNGS];

export interface TargetSize {
  width: number;
  height: number;
}

function roundToEven(n: number): number {
  const r = Math.round(n);
  return r - (r % 2);
}

// A rung caps the LONGER dimension at its 16:9 width equivalent (1080p →
// 1920), preserving aspect ratio. Capping the shorter dimension instead
// would leave total pixel count unbounded — a hostile source shaped
// 25000x1080 would count as "already 1080p" and pass through at 27
// megapixels; with the longer-dimension cap the worst case is cap² (a
// square source). Returns null when no scaling applies: the native rung,
// or a source already within the cap (never upscale).
const LONGER_DIM_CAP: Record<Exclude<ResolutionRung, 'native'>, number> = {
  1080: 1920,
  720: 1280,
  480: 854,
};

export function computeTargetSize(
  srcWidth: number,
  srcHeight: number,
  rung: ResolutionRung,
): TargetSize | null {
  if (rung === 'native') return null;
  const cap = LONGER_DIM_CAP[rung];
  const longer = Math.max(srcWidth, srcHeight);
  if (longer <= cap) return null;
  const scale = cap / longer;
  return {
    width: Math.max(2, roundToEven(srcWidth * scale)),
    height: Math.max(2, roundToEven(srcHeight * scale)),
  };
}

// Bitrate follows the ladder: 0.05 bits per pixel at a 60 fps reference
// (1920x1080@60 ≈ 6.2 Mbps, matching the historical fixed 6 Mbps), scaled by
// sqrt(fps/60) so low-framerate streams keep enough headroom for crisp
// keyframes, clamped to [0.5, 10] Mbps.
const BITS_PER_PIXEL_AT_60 = 0.05;
const MIN_BITRATE = 500_000;
const MAX_BITRATE = 10_000_000;

// The per-source effective ladder auto mode walks (docs/09 Decision 3):
// 'native' (the ceiling) followed by every rung whose longer-dimension cap
// is strictly below the source's longer dimension — the rungs that actually
// shrink the picture. Stepping to a rung whose cap wouldn't change anything
// would recreate the encoder for an identical config.
export function autoLadder(srcLongerDim: number): ResolutionRung[] {
  const rungs: ResolutionRung[] = ['native'];
  for (const rung of RESOLUTION_RUNGS) {
    if (rung !== 'native' && LONGER_DIM_CAP[rung] < srcLongerDim) rungs.push(rung);
  }
  return rungs;
}

// R12 (docs/17 Decisions 3+4): structural mirror of the probe matrix's
// lookup — declared here (not imported) because probe.ts imports this
// module; the extra SupportEntry fields are structurally compatible.
export type SupportLookup = (
  rung: ResolutionRung,
  framerate: number,
) => { acceleration: 'hardware' | 'software' | 'unsupported' };

// Decision 4: 'auto' fps resolves framerate-first — 60 when any rung probes
// hardware at 60, else the conservative 30 (the software path keeps the old
// fan-out default). Never 'native': a 144 Hz monitor would multiply the
// fan-out for no viewer-visible benefit.
export function resolveAutoFps(lookup: SupportLookup): 60 | 30 {
  for (const rung of RESOLUTION_RUNGS) {
    if (lookup(rung, 60).acceleration === 'hardware') return 60;
  }
  return 30;
}

// Decision 3: when nothing probes hardware (Firefox; software-only mode)
// the auto ceiling is 1080p — a sane software starting point; R4 stepping
// handles the rest.
export const SOFTWARE_CEILING: ResolutionRung = 1080;

// The auto ceiling: the highest rung that probes hardware at the effective
// fps. RESOLUTION_RUNGS is ordered native-first, so the first hit wins.
export function hardwareCeiling(lookup: SupportLookup, framerate: number): ResolutionRung {
  for (const rung of RESOLUTION_RUNGS) {
    if (lookup(rung, framerate).acceleration === 'hardware') return rung;
  }
  return SOFTWARE_CEILING;
}

// Slices an autoLadder() result at the ceiling. 'native' survives a
// non-native ceiling only when the source itself fits under the ceiling's
// cap (a 720p source at a 1080p ceiling encodes native — the ceiling bounds
// pixel count, not the rung label). Never returns empty: the ladder's floor
// (480) fits under every ceiling, but guard anyway.
export function applyCeiling(
  rungs: ResolutionRung[],
  ceiling: ResolutionRung,
  srcLongerDim: number,
): ResolutionRung[] {
  if (ceiling === 'native') return rungs;
  const cap = LONGER_DIM_CAP[ceiling];
  const out = rungs.filter((rung) =>
    rung === 'native' ? srcLongerDim <= cap : LONGER_DIM_CAP[rung] <= cap,
  );
  return out.length > 0 ? out : [rungs[rungs.length - 1]];
}

// Capture-constraint width cap for a sticky rung (docs/17 Decision 6): null
// for native — the broad grant, no cap beyond the original request.
export function rungCapWidth(rung: ResolutionRung): number | null {
  return rung === 'native' ? null : LONGER_DIM_CAP[rung];
}

export function computeBitrate(width: number, height: number, fps: number): number {
  const base = BITS_PER_PIXEL_AT_60 * width * height * 60 * Math.sqrt(fps / 60);
  return Math.min(MAX_BITRATE, Math.max(MIN_BITRATE, Math.round(base)));
}

// R12 (docs/17 Decision 11): the advanced bitrate override is absolute —
// it replaces the ladder math until reset — but clamped to a wider band
// than the ladder's: the 1 Gbps homelab uplink allows experiments past the
// 10 Mbps ladder cap (15 viewers × 50 Mbps stays within egress), while the
// floor matches the ladder's.
export const BITRATE_OVERRIDE_MIN = 500_000;
export const BITRATE_OVERRIDE_MAX = 50_000_000;

export function clampBitrateOverride(bps: number): number {
  return Math.min(BITRATE_OVERRIDE_MAX, Math.max(BITRATE_OVERRIDE_MIN, Math.round(bps)));
}
