import { useCallback, useEffect, useState } from 'react';

export type Theme = 'light' | 'dark';

const STORAGE = 'songguo_theme';

function readStored(): Theme {
  try {
    const t = localStorage.getItem(STORAGE);
    if (t === 'dark' || t === 'light') return t;
  } catch {
    /* ignore */
  }
  // Default to light per the design (light is the default theme).
  return 'light';
}

function apply(theme: Theme): void {
  document.documentElement.setAttribute('data-theme', theme);
}

/** useTheme manages the light/dark theme, persisted to localStorage and applied
 * via data-theme on <html>. */
export function useTheme(): { theme: Theme; setTheme: (t: Theme) => void; toggle: () => void } {
  const [theme, setThemeState] = useState<Theme>(readStored);

  useEffect(() => {
    apply(theme);
  }, [theme]);

  const setTheme = useCallback((t: Theme) => {
    try {
      localStorage.setItem(STORAGE, t);
    } catch {
      /* ignore */
    }
    setThemeState(t);
  }, []);

  const toggle = useCallback(() => {
    setThemeState((prev) => {
      const next = prev === 'dark' ? 'light' : 'dark';
      try {
        localStorage.setItem(STORAGE, next);
      } catch {
        /* ignore */
      }
      return next;
    });
  }, []);

  return { theme, setTheme, toggle };
}
