import { useCallback, useEffect, useState } from 'react';

/** The user's theme preference. 'auto' follows the OS colour scheme. */
export type Theme = 'light' | 'dark' | 'auto';
/** The concrete theme actually applied to the page. */
export type ResolvedTheme = 'light' | 'dark';

const STORAGE = 'songguo_theme';

function readStored(): Theme {
  try {
    const t = localStorage.getItem(STORAGE);
    if (t === 'dark' || t === 'light' || t === 'auto') return t;
  } catch {
    /* ignore */
  }
  // Default to auto: follow the OS colour scheme until the user picks explicitly.
  return 'auto';
}

function systemTheme(): ResolvedTheme {
  try {
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  } catch {
    return 'light';
  }
}

function resolve(theme: Theme): ResolvedTheme {
  return theme === 'auto' ? systemTheme() : theme;
}

function apply(theme: ResolvedTheme): void {
  document.documentElement.setAttribute('data-theme', theme);
}

/** useTheme manages the theme preference (light/dark/auto), persisted to
 * localStorage. The resolved light/dark value is applied via data-theme on
 * <html>; in auto mode it tracks the OS colour scheme live. */
export function useTheme(): {
  theme: Theme;
  resolvedTheme: ResolvedTheme;
  setTheme: (t: Theme) => void;
} {
  const [theme, setThemeState] = useState<Theme>(readStored);
  const [resolved, setResolved] = useState<ResolvedTheme>(() => resolve(readStored()));

  // Re-resolve when the preference changes, and — while in auto — whenever the
  // OS colour scheme flips underneath us.
  useEffect(() => {
    setResolved(resolve(theme));
    if (theme !== 'auto') return;
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const onChange = () => setResolved(systemTheme());
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, [theme]);

  useEffect(() => {
    apply(resolved);
  }, [resolved]);

  const setTheme = useCallback((t: Theme) => {
    try {
      localStorage.setItem(STORAGE, t);
    } catch {
      /* ignore */
    }
    setThemeState(t);
  }, []);

  return { theme, resolvedTheme: resolved, setTheme };
}
