// The R3 broadcast ladder: what the broadcaster can choose to send, and the
// pure math turning a rung + an actual source frame into encode parameters.
// See docs/08-resolution-framerate-picker.md for the locked decisions.

export type ResolutionRung = 'native' | 1080 | 720 | 480;
export type FramerateRung = 'native' | 60 | 30 | 5;

// Native first — it is the default everywhere.
export const RESOLUTION_RUNGS: readonly ResolutionRung[] = ['native', 1080, 720, 480];
export const FRAMERATE_RUNGS: readonly FramerateRung[] = ['native', 60, 30, 5];

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

export function computeBitrate(width: number, height: number, fps: number): number {
  const base = BITS_PER_PIXEL_AT_60 * width * height * 60 * Math.sqrt(fps / 60);
  return Math.min(MAX_BITRATE, Math.max(MIN_BITRATE, Math.round(base)));
}
