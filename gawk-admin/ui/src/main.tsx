import { AuthSession } from '@gawk/oidc-session';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App } from './App.tsx';
import { AuthProvider } from './auth/AuthContext.tsx';
import './styles/global.css';

// One session per page load, created before React renders so the redirect flow
// starts immediately: on a cold load with a live IdP session this is an
// invisible bounce, and the operator who followed a webhook link should land on
// the broadcast, not on a login screen they have to click through (docs/42
// §4.8, §4.10).
//
// The two options are the portal's own values for the shared session
// (docs/55 D3), unchanged from before it was shared: `auth/config` is the
// bootstrap route internal/portal serves, and the storage key prefix names the
// one sessionStorage entry a login in flight across a redeploy reads back.
const session = new AuthSession({ configPath: 'auth/config', storageKeyPrefix: 'gawk-admin' });
void session.start();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AuthProvider session={session}>
      <App />
    </AuthProvider>
  </StrictMode>,
);
