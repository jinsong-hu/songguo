import { lazy, useCallback, useEffect, useRef, useState } from 'react';
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { api, clearAdminKey, getAdminKey, onUnauthorized } from './api/client';
import type { Settings } from './api/types';
import { ApiError } from './api/client';
import { Gate } from './components/Gate';
import { Layout } from './components/Layout';
import { ToastProvider } from './components/Toast';
import { SettingsContext } from './lib/settingsContext';

// Routes are split into their own chunks so heavy page-only deps (charts on
// Overview, the Radix Select on a service detail, etc.) don't weigh down the
// initial bundle. The pages use named exports, so map them onto `default`.
const OverviewPage = lazy(() => import('./pages/Overview').then((m) => ({ default: m.OverviewPage })));
const ServicesPage = lazy(() => import('./pages/Services').then((m) => ({ default: m.ServicesPage })));
const ServiceDetailPage = lazy(() =>
  import('./pages/ServiceDetail').then((m) => ({ default: m.ServiceDetailPage })),
);
const ProvidersPage = lazy(() => import('./pages/Providers').then((m) => ({ default: m.ProvidersPage })));
const VendorAddPage = lazy(() => import('./pages/VendorAdd').then((m) => ({ default: m.VendorAddPage })));
const ProviderEditPage = lazy(() =>
  import('./pages/ProviderEdit').then((m) => ({ default: m.ProviderEditPage })),
);
const UsersPage = lazy(() => import('./pages/Users').then((m) => ({ default: m.UsersPage })));
const UserNewPage = lazy(() => import('./pages/UserNew').then((m) => ({ default: m.UserNewPage })));
const UserEditPage = lazy(() => import('./pages/UserEdit').then((m) => ({ default: m.UserEditPage })));
const SettingsPage = lazy(() => import('./pages/SettingsPage').then((m) => ({ default: m.SettingsPage })));

type Phase =
  | { kind: 'loading' }
  | { kind: 'gate' }
  | { kind: 'ready'; settings: Settings };

// A restarting backend is briefly unreachable. Retry the initial probe with
// backoff (capped) before giving up, so a backend restart doesn't bounce an
// already-authenticated user back to the gate.
const MAX_BOOT_RETRIES = 6;

export function App() {
  const [phase, setPhase] = useState<Phase>({ kind: 'loading' });
  const mounted = useRef(true);

  const bootstrap = useCallback(async (attempt = 0) => {
    try {
      const settings = await api.settings();
      if (mounted.current) setPhase({ kind: 'ready', settings });
    } catch (e) {
      if (!mounted.current) return;
      // A real 401 means the stored key is missing or wrong — clearAdminKey
      // already ran in the client, so the gate is the right destination.
      if (e instanceof ApiError && e.status === 401) {
        setPhase({ kind: 'gate' });
        return;
      }
      // Transient failure (backend restarting, network blip). If we still hold
      // a key, the key is fine — keep waiting and retry instead of forcing a
      // re-login. The 401 branch above is the only thing that drops the key.
      if (getAdminKey() && attempt < MAX_BOOT_RETRIES) {
        const delay = Math.min(500 * 2 ** attempt, 5000);
        setPhase({ kind: 'loading' });
        window.setTimeout(() => {
          if (mounted.current) void bootstrap(attempt + 1);
        }, delay);
        return;
      }
      // No key, or retries exhausted: show the gate as a recovery surface.
      setPhase({ kind: 'gate' });
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
              element={<Layout />}
            >
              <Route index element={<OverviewPage />} />
              <Route path="services" element={<ServicesPage />} />
              <Route path="services/add" element={<Navigate to="/providers" replace />} />
              <Route path="services/:model" element={<ServiceDetailPage />} />
              <Route path="providers" element={<ProvidersPage />} />
              <Route path="providers/add" element={<Navigate to="/providers" replace />} />
              <Route path="providers/add/:vendorId" element={<VendorAddPage />} />
              {/* The old custom-provider routes are now the "custom" catalog vendor. */}
              <Route path="providers/new" element={<Navigate to="/providers/add/custom" replace />} />
              <Route path="providers/new/:kind" element={<Navigate to="/providers/add/custom" replace />} />
              <Route path="providers/:id/edit" element={<ProviderEditPage />} />
              <Route path="users" element={<UsersPage />} />
              <Route path="users/new" element={<UserNewPage />} />
              <Route path="users/:id/edit" element={<UserEditPage />} />
              <Route path="settings" element={<SettingsPage />} />
              <Route path="*" element={<OverviewPage />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </ToastProvider>
    </SettingsContext.Provider>
  );
}
