import { create } from 'zustand';
import { DEFAULT_CAPTURE_CONFIG, EMPTY_STATS } from '../media/types';
import type { CaptureConfig, PipelineStats, PipelineStatus } from '../media/types';
import type { EncoderConfigured } from '../media/encoder';

interface PipelineState {
  status: PipelineStatus;
  config: CaptureConfig;
  stats: PipelineStats;
  encoderInfo: EncoderConfigured | null;
  capturePath: string | null;
  lastError: string | null;

  setStatus: (status: PipelineStatus) => void;
  setConfig: (config: Partial<CaptureConfig>) => void;
  setStats: (stats: PipelineStats) => void;
  setEncoderInfo: (info: EncoderConfigured | null) => void;
  setCapturePath: (path: string | null) => void;
  setError: (message: string | null) => void;
  reset: () => void;
}

export const usePipelineStore = create<PipelineState>((set) => ({
  status: 'idle',
  config: { ...DEFAULT_CAPTURE_CONFIG },
  stats: { ...EMPTY_STATS },
  encoderInfo: null,
  capturePath: null,
  lastError: null,

  setStatus: (status) => set({ status }),
  setConfig: (config) => set((s) => ({ config: { ...s.config, ...config } })),
  setStats: (stats) => set({ stats }),
  setEncoderInfo: (encoderInfo) => set({ encoderInfo }),
  setCapturePath: (capturePath) => set({ capturePath }),
  setError: (lastError) => set({ lastError }),
  reset: () =>
    set({
      status: 'idle',
      stats: { ...EMPTY_STATS },
      encoderInfo: null,
      capturePath: null,
      lastError: null,
    }),
}));
