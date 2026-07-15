// R13 (docs/18): the encoder-capability probe matrix. Probes
// (resolution rung × framerate × codec preference × acceleration hint) via
// VideoEncoder.isConfigSupported() into a support map the picker annotates
// from and the auto ceiling / auto-fps default resolve against.
//
// Carried assumption (from encoder.ts, verified on Chromium during L1): a
// 'prefer-hardware' probe answering supported=true is a commitment to
// hardware — Chromium returns false when it can't do HW. There is no
// spec-level "require hardware"; this probe is as close as it gets. The
// probe is *advisory*: the live configure() result wins (docs/18 Decision
// 13). On Firefox every prefer-hardware probe is rejected (its
// VideoEncoder is software-only) and the matrix degrades to all-software.

import { computeBitrate, computeTargetSize, RESOLUTION_RUNGS, type ResolutionRung } from './ladder';

export type HwPreference = 'auto' | 'hardware' | 'software';
export type ProbeAcceleration = 'hardware' | 'software' | 'unsupported';

export interface SupportEntry {
  acceleration: ProbeAcceleration;
  // The first codec preference that resolved at this combo; null when
  // unsupported.
  codec: string | null;
}

export interface SourceDims {
  width: number;
  height: number;
}

// Pre-capture upper bound for the native rung: probe as if the source were
// 16:9 4K. Refined from the first real frame's dimensions once capture
// starts — frames are truth (docs/01), and a matrix probed at the wrong
// native size would mis-annotate the picker.
export const DEFAULT_PROBE_SOURCE: SourceDims = { width: 3840, height: 2160 };

// Concrete framerates the matrix probes. 'native' fps is annotated against
// its measured value once known; pre-capture it is treated as 60 (the same
// upper-bound stance as DEFAULT_PROBE_SOURCE).
export const PROBE_FPS_VALUES: readonly number[] = [60, 30, 5];

// The dimensions a rung actually encodes at for a given source: the rung's
// scaled size, or the source itself when the rung wouldn't shrink it
// (native, or a source already within the cap — computeTargetSize's
// passthrough rule).
export function probeDims(rung: ResolutionRung, source: SourceDims): SourceDims {
  return (
    computeTargetSize(source.width, source.height, rung) ?? {
      width: source.width,
      height: source.height,
    }
  );
}

export type IsConfigSupportedFn = (
  config: VideoEncoderConfig,
) => Promise<{ supported?: boolean; config?: VideoEncoderConfig }>;

// Whether this scope can probe at all. When it can't (no WebCodecs — jsdom,
// exotic browsers) the pipeline skips the matrix entirely and keeps the
// pre-R13 optimistic defaults: an unavailable probe must not clamp behavior
// (docs/18 Decision 13 — runtime truth over probe truth).
export function probeSupported(): boolean {
  return typeof VideoEncoder !== 'undefined' && typeof VideoEncoder.isConfigSupported === 'function';
}

const defaultIsConfigSupported: IsConfigSupportedFn = (config) => {
  if (typeof VideoEncoder === 'undefined' || typeof VideoEncoder.isConfigSupported !== 'function') {
    return Promise.resolve({ supported: false });
  }
  return VideoEncoder.isConfigSupported(config);
};

export function matrixKey(rung: ResolutionRung, framerate: number): string {
  return `${rung}@${framerate}`;
}

export interface SupportMatrix {
  source: SourceDims;
  hwPreference: HwPreference;
  entries: ReadonlyMap<string, SupportEntry>;
  get(rung: ResolutionRung, framerate: number): SupportEntry;
}

const UNSUPPORTED: SupportEntry = { acceleration: 'unsupported', codec: null };

// Probes one (dims, fps) combo per acceleration policy and memoizes by the
// full probe key, so re-running the matrix (settings change, native-dims
// refinement) only pays for combos it hasn't seen — refined native dims
// produce a new key and genuinely re-probe. Probe exceptions classify that
// codec attempt as unsupported and the walk continues; the prober never
// throws.
export class EncoderSupportProber {
  private cache = new Map<string, Promise<SupportEntry>>();
  private isSupported: IsConfigSupportedFn;

  constructor(isSupported: IsConfigSupportedFn = defaultIsConfigSupported) {
    this.isSupported = isSupported;
  }

  probeCombo(
    codecs: readonly string[],
    hwPreference: HwPreference,
    dims: SourceDims,
    framerate: number,
  ): Promise<SupportEntry> {
    const key = `${codecs.join(',')}|${hwPreference}|${dims.width}x${dims.height}@${framerate}`;
    let entry = this.cache.get(key);
    if (!entry) {
      entry = this.runCombo(codecs, hwPreference, dims, framerate);
      this.cache.set(key, entry);
    }
    return entry;
  }

  private async runCombo(
    codecs: readonly string[],
    hwPreference: HwPreference,
    dims: SourceDims,
    framerate: number,
  ): Promise<SupportEntry> {
    const base = {
      width: dims.width,
      height: dims.height,
      bitrate: computeBitrate(dims.width, dims.height, framerate),
      framerate,
    };

    if (hwPreference !== 'software') {
      for (const codec of codecs) {
        if (await this.hardwareSupported({ ...base, codec })) {
          return { acceleration: 'hardware', codec };
        }
      }
      // 'hardware' mode refuses to run software: not-HW ⇒ unsupported.
      if (hwPreference === 'hardware') return UNSUPPORTED;
    }

    // Software check. 'software' mode probes prefer-software (matching the
    // encoder's software-only variants); 'auto' probes hint-free (matching
    // the cascade's fallback variants — what the encoder would actually run).
    const swHint: Partial<VideoEncoderConfig> =
      hwPreference === 'software' ? { hardwareAcceleration: 'prefer-software' } : {};
    for (const codec of codecs) {
      if (await this.softwareSupported({ ...base, codec, ...swHint })) {
        return { acceleration: 'software', codec };
      }
    }
    return UNSUPPORTED;
  }

  private async hardwareSupported(config: VideoEncoderConfig): Promise<boolean> {
    try {
      const support = await this.isSupported({
        ...config,
        hardwareAcceleration: 'prefer-hardware',
      });
      // supported=true with prefer-hardware is the HW commitment — unless
      // the resolved config explicitly says the browser went software.
      return support.supported === true && support.config?.hardwareAcceleration !== 'prefer-software';
    } catch {
      return false;
    }
  }

  private async softwareSupported(config: VideoEncoderConfig): Promise<boolean> {
    try {
      const support = await this.isSupported(config);
      return support.supported === true;
    } catch {
      return false;
    }
  }
}

export interface ProbeMatrixOptions {
  codecs: readonly string[];
  hwPreference: HwPreference;
  // Actual source dimensions once capture is running; the 4K upper bound
  // before that.
  source?: SourceDims;
  fpsValues?: readonly number[];
  rungs?: readonly ResolutionRung[];
}

// Probes the full (rung × fps) matrix — a dozen or so isConfigSupported
// calls, milliseconds each, run concurrently. Rungs that don't shrink the
// source share the prober's memoized combo with the native rung.
export async function probeSupportMatrix(
  prober: EncoderSupportProber,
  opts: ProbeMatrixOptions,
): Promise<SupportMatrix> {
  const source = opts.source ?? DEFAULT_PROBE_SOURCE;
  const fpsValues = opts.fpsValues ?? PROBE_FPS_VALUES;
  const rungs = opts.rungs ?? RESOLUTION_RUNGS;

  const entries = new Map<string, SupportEntry>();
  await Promise.all(
    rungs.flatMap((rung) =>
      fpsValues.map(async (fps) => {
        const entry = await prober.probeCombo(opts.codecs, opts.hwPreference, probeDims(rung, source), fps);
        entries.set(matrixKey(rung, fps), entry);
      }),
    ),
  );

  return {
    source,
    hwPreference: opts.hwPreference,
    entries,
    get: (rung, framerate) => entries.get(matrixKey(rung, framerate)) ?? UNSUPPORTED,
  };
}
