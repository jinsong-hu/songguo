import { createContext, useContext } from 'react';
import type { Me } from '../api/types';

// Session identity available in both shells (admin dashboard and the scoped
// user playground). Layout reads `me.role` to decide which nav to render.
interface SessionContextValue {
  me: Me;
  signOut: () => void;
}

export const SessionContext = createContext<SessionContextValue | null>(null);

export function useSession(): SessionContextValue {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error('useSession must be used within SessionContext');
  return ctx;
}
