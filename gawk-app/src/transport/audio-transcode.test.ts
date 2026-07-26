import { describe, expect, it, vi } from 'vitest';
import {
  AAC_CODEC,
  AacTranscoder,
  extractAudioSpecificConfig,
  type AudioDataLike,
  type EncodedChunkLike,
  type TranscodeInput,
  type TranscoderDeps,
} from './audio-transcode';

// docs/27 finding 6, measured on iPhone (iOS 18.7 / Safari 26.5.2) by the R22
// device probe: Safari's AudioEncoder hands back the WHOLE `esds` payload — a
// complete ES_Descriptor — as `decoderConfig.description`, where the WebCodecs
// spec (and Chrome) hand back the bare AudioSpecificConfig. These are the real
// 39 bytes from the device. Note the 4-byte 0x80-continuation descriptor sizes:
// Apple writes the long form even for tiny payloads.
const SAFARI_DESCRIPTION = Uint8Array.from(
  (
    '03808080220000000480808014401400180000000000000000' +
    '0005808080021190068080800102'
  ).match(/../g)!.map((h) => parseInt(h, 16)),
);
// The AudioSpecificConfig buried inside it: AAC-LC (AOT 2), 48 kHz, stereo —
// byte-identical to what Chrome returns directly.
const ASC = Uint8Array.from([0x11, 0x90]);

describe('extractAudioSpecificConfig', () => {
  it('passes a bare AudioSpecificConfig through unchanged (the Chrome/spec shape)', () => {
    expect(extractAudioSpecificConfig(ASC)).toEqual(ASC);
  });

  it('unwraps the AudioSpecificConfig from a full ES_Descriptor (the Safari shape)', () => {
    // Without this, the muxer nests an entire ES_Descriptor inside its own
    // DecoderSpecificInfo and WebKit rejects the init segment with
    // MEDIA_ERR_SRC_NOT_SUPPORTED, closing the MediaSource.
    expect(extractAudioSpecificConfig(SAFARI_DESCRIPTION)).toEqual(ASC);
  });

  it('unwraps a bare DecoderConfigDescriptor too', () => {
    // tag 0x04, 1-byte size, OTI 0x40, streamType byte, bufferSizeDB, two
    // bitrates, then the DecoderSpecificInfo.
    const dcd = Uint8Array.from([
      // size 0x11 = 17: OTI(1) + streamType(1) + bufferSizeDB(3) + bitrates(8)
      // + the 4-byte DecoderSpecificInfo.
      0x04, 0x11, 0x40, 0x15, 0x00, 0x18, 0x00,
      0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
      0x05, 0x02, 0x11, 0x90,
    ]);
    expect(extractAudioSpecificConfig(dcd)).toEqual(ASC);
  });

  it('returns the input unchanged when a descriptor carries no DecoderSpecificInfo', () => {
    // Refusing to guess: a shape we do not understand is passed through, and the
    // muxer's own guard is what refuses to build an unusable init segment.
    const noDsi = Uint8Array.from([0x03, 0x05, 0x00, 0x01, 0x00, 0x06, 0x01, 0x02]);
    expect(extractAudioSpecificConfig(noDsi)).toEqual(noDsi);
  });

  it('does not walk off the end of a truncated descriptor', () => {
    const truncated = Uint8Array.from([0x03, 0x40, 0x00, 0x01, 0x00]);
    expect(() => extractAudioSpecificConfig(truncated)).not.toThrow();
  });

  it('handles an empty description', () => {
    expect(extractAudioSpecificConfig(new Uint8Array(0))).toEqual(new Uint8Array(0));
  });
});

// The transcoder is the boundary where the encoder's answer enters our world, so
// the normalization has to happen there — the muxer's contract stays "description
// IS the AudioSpecificConfig" and its golden vectors do not move.
describe('AacTranscoder description normalization', () => {
  function depsEmitting(description: Uint8Array): {
    deps: TranscoderDeps;
    emit: () => void;
  } {
    let output: ((chunk: EncodedChunkLike, metadata?: { decoderConfig?: { description?: unknown } }) => void) | null =
      null;
    const deps: TranscoderDeps = {
      createEncoder: (cb) => {
        output = cb.output;
        return {
          configure: vi.fn(),
          encode: vi.fn(),
          close: vi.fn(),
        };
      },
      createAudioData: (): AudioDataLike => ({ close: vi.fn() }),
    };
    const emit = () => {
      output?.(
        {
          timestamp: 0,
          byteLength: 4,
          copyTo: (dest: Uint8Array) => dest.set([1, 2, 3, 4]),
        },
        { decoderConfig: { description } },
      );
    };
    return { deps, emit };
  }

  const pcm = (): TranscodeInput => ({
    timestampUs: 0,
    sampleRate: 48_000,
    channels: [new Float32Array(960), new Float32Array(960)],
    frameCount: 960,
  });

  it('hands the muxer the bare ASC when the encoder returns an ES_Descriptor', () => {
    const outputs: (Uint8Array | null)[] = [];
    const { deps, emit } = depsEmitting(SAFARI_DESCRIPTION);
    const t = new AacTranscoder((o) => outputs.push(o.description), deps);
    t.push(pcm());
    emit();
    expect(t.getStats().codec).toBe(AAC_CODEC);
    expect(outputs[0]).toEqual(ASC);
  });

  it('is a no-op on the Chrome shape', () => {
    const outputs: (Uint8Array | null)[] = [];
    const { deps, emit } = depsEmitting(ASC);
    const t = new AacTranscoder((o) => outputs.push(o.description), deps);
    t.push(pcm());
    emit();
    expect(outputs[0]).toEqual(ASC);
  });
});
