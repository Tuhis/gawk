import { create } from 'zustand';

// Server connection settings shared by the broadcast and view pages,
// persisted so a dev-cert hash pasted once survives reloads (the hash
// changes on every server restart, the URL rarely).
const LS_SERVER_URL = 'gawk.serverUrl';
const LS_CERT_HASH = 'gawk.certHashHex';

const DEFAULT_SERVER_URL =
  typeof window !== 'undefined' && window.location.hostname === 'gawk.ioio.fi'
    ? 'https://api.gawk.ioio.fi:4433'
    : 'https://localhost:4433';

interface TransportSettingsState {
  serverUrl: string;
  // hex(SHA-256(cert DER)) from the gawk-server startup log; empty when the
  // server has a real certificate (or Chrome runs with the SPKI flag).
  certHashHex: string;

  setServerUrl: (url: string) => void;
  setCertHashHex: (hash: string) => void;
}

export const useTransportStore = create<TransportSettingsState>((set) => ({
  serverUrl: localStorage.getItem(LS_SERVER_URL) ?? DEFAULT_SERVER_URL,
  certHashHex: localStorage.getItem(LS_CERT_HASH) ?? '',

  setServerUrl: (serverUrl) => {
    localStorage.setItem(LS_SERVER_URL, serverUrl);
    set({ serverUrl });
  },
  setCertHashHex: (certHashHex) => {
    localStorage.setItem(LS_CERT_HASH, certHashHex);
    set({ certHashHex });
  },
}));
