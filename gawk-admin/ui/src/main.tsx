import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App } from './App.tsx';
import { AuthProvider } from './auth/AuthContext.tsx';
import { AuthSession } from './auth/session.ts';
import './styles/global.css';

// One session per page load, created before React renders so the redirect flow
// starts immediately: on a cold load with a live IdP session this is an
// invisible bounce, and the operator who followed a webhook link should land on
// the broadcast, not on a login screen they have to click through (docs/42
// §4.8, §4.10).
const session = new AuthSession();
void session.start();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AuthProvider session={session}>
      <App />
    </AuthProvider>
  </StrictMode>,
);
