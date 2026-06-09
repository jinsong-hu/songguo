import { createContext, useContext } from 'react';
import type { Settings } from '../api/types';

interface SettingsContextValue {
  settings: Settings;
  signOut: () => void;
}

export const SettingsContext = createContext<SettingsContextValue | null>(null);

export function useSettings(): SettingsContextValue {
  const ctx = useContext(SettingsContext);
  if (!ctx) throw new Error('useSettings must be used within SettingsContext');
  return ctx;
}
