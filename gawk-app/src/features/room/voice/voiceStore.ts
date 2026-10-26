// The voice client's view state (voiceController.ts publishes into it) and
// the push-to-talk key, for the room screen to render from. Not persisted;
// the preferences live in voicePrefs.ts.

import { create } from 'zustand';
import { INITIAL_VOICE_STATE, type VoiceViewState } from './voiceController';
import { loadPttKey, loadTalkMode } from './voicePrefs';

export interface VoiceStoreState extends VoiceViewState {
  pttKey: string;
  set: (patch: Partial<VoiceStoreState>) => void;
  reset: () => void;
}

export const useVoiceStore = create<VoiceStoreState>((set) => ({
  ...INITIAL_VOICE_STATE,
  talkMode: loadTalkMode(),
  pttKey: loadPttKey(),
  set: (patch) => set(patch),
  reset: () => set({ ...INITIAL_VOICE_STATE, talkMode: loadTalkMode(), pttKey: loadPttKey() }),
}));
