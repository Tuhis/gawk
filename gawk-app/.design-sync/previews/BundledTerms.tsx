import { BundledTerms } from 'gawk-app';

const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  fontSize: 'var(--fs-md)',
  lineHeight: 1.6,
  minHeight: '100%',
  padding: '32px 28px',
};

/**
 * The shipped default Terms of Use — always in the bundle, so a dev build, an
 * un-configured install and every override-fetch failure still render real,
 * styled prose with zero network dependency. Operator name, contact and
 * version substitute from runtime config.
 */
export const Default = () => (
  <div style={canvas}>
    <div style={{ maxWidth: '680px', margin: '0 auto' }}>
      <BundledTerms />
    </div>
  </div>
);
