import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App } from './App.tsx';
import { bootstrapAuth } from './auth/auth.ts';
import { AuthGate } from './auth/AuthGate.tsx';
import './styles/global.css';

// The mode probe runs before React renders, so in OIDC mode the redirect flow
// starts at once — against a live IdP session that is an invisible bounce —
// and in none/basic mode the page mounts after one 404 (docs/55 D5).
void bootstrapAuth();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AuthGate>
      <App />
    </AuthGate>
  </StrictMode>,
);
