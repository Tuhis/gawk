import styles from './stream.module.css';
import { useTransportStore } from '../../state/transportStore';

interface Props {
  disabled: boolean;
}

export function ServerSettings({ disabled }: Props) {
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const certHashHex = useTransportStore((s) => s.certHashHex);
  const setServerUrl = useTransportStore((s) => s.setServerUrl);
  const setCertHashHex = useTransportStore((s) => s.setCertHashHex);

  return (
    <div className={styles.settings}>
      <div className={styles.field}>
        <label htmlFor="server-url">Server URL</label>
        <input
          id="server-url"
          value={serverUrl}
          onChange={(e) => setServerUrl(e.target.value)}
          disabled={disabled}
          placeholder="https://localhost:4433"
          spellCheck={false}
        />
      </div>
      <div className={styles.field}>
        <label htmlFor="cert-hash">Dev cert hash (hex; from server log, empty for a real cert)</label>
        <input
          id="cert-hash"
          value={certHashHex}
          onChange={(e) => setCertHashHex(e.target.value)}
          disabled={disabled}
          placeholder="cert_hash_hex from gawk-server startup log"
          spellCheck={false}
        />
      </div>
    </div>
  );
}
