import { useCallback, useEffect, useRef, useState } from 'react';
import { BrowserRouter, Route, Routes } from 'react-router-dom';
import { api, clearAdminKey, onUnauthorized } from './api/client';
import type { Settings } from './api/types';
import { ApiError } from './api/client';
import { Gate } from './components/Gate';
import { Layout } from './components/Layout';
import { ToastProvider } from './components/Toast';
import { SettingsContext } from './lib/settingsContext';
import { OverviewPage } from './pages/Overview';
import { VendorsPage } from './pages/Vendors';
import { TokensPage } from './pages/Tokens';
import { SettingsPage } from './pages/SettingsPage';

type Phase =
  | { kind: 'loading' }
  | { kind: 'gate' }
  | { kind: 'ready'; settings: Settings };

export function App() {
  const [phase, setPhase] = useState<Phase>({ kind: 'loading' });
  const mounted = useRef(true);

  const bootstrap = useCallback(async () => {
    try {
      const settings = await api.settings();
      if (mounted.current) setPhase({ kind: 'ready', settings });
    } catch (e) {
      if (!mounted.current) return;
      if (e instanceof ApiError && e.status === 401) {
        setPhase({ kind: 'gate' });
      } else {
        // Network/other error: if there's no key requirement we still can't
        // proceed, so show the gate as a recovery surface.
        setPhase({ kind: 'gate' });
      }
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    // If the API is unprotected, settings returns 200 even without a key.
    void bootstrap();
    return () => {
      mounted.current = false;
    };
  }, [bootstrap]);

  // Any 401 from the API client routes us back to the gate.
  useEffect(() => {
    return onUnauthorized(() => {
      if (mounted.current) setPhase({ kind: 'gate' });
    });
  }, []);

  const signOut = useCallback(() => {
    clearAdminKey();
    setPhase({ kind: 'gate' });
  }, []);

  if (phase.kind === 'loading') {
    return (
      <div
        style={{
          height: '100%',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
        }}
      >
        <span className="spinner" />
      </div>
    );
  }

  if (phase.kind === 'gate') {
    return (
      <Gate
        verify={async () => {
          await api.settings();
        }}
        onAuthenticated={() => {
          // The Gate stored & verified the key before calling back.
          void bootstrap();
        }}
      />
    );
  }

  const { settings } = phase;

  return (
    <SettingsContext.Provider value={{ settings, signOut }}>
      <ToastProvider>
        <BrowserRouter>
          <Routes>
            <Route
              element={
                <Layout
                  adminProtected={settings.admin_protected}
                  version={settings.version}
                />
              }
            >
              <Route index element={<OverviewPage />} />
              <Route path="vendors" element={<VendorsPage />} />
              <Route path="tokens" element={<TokensPage />} />
              <Route path="settings" element={<SettingsPage />} />
              <Route path="*" element={<OverviewPage />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </ToastProvider>
    </SettingsContext.Provider>
  );
}
