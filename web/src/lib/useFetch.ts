import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError } from '../api/client';

/**
 * Shared "Live" cadence for auto-refreshing dashboard views. The Overview page
 * advances its time window on this same interval (see useLiveWindow) so the
 * calls table, KPIs, and chart all reflect "now" on each tick.
 */
export const LIVE_REFRESH_MS = 10_000;

interface FetchState<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  /** Manually re-run the fetch. */
  refetch: () => void;
  /** True only on the very first load (used to show skeletons). */
  initialLoading: boolean;
}

interface Options {
  /** Auto-refresh interval in milliseconds. 0 disables polling. */
  intervalMs?: number;
  /** Skip fetching entirely (e.g. while a dependency is unresolved). */
  enabled?: boolean;
}

/**
 * useFetch runs an async loader, exposing loading/error/data and an optional
 * polling interval. The loader is keyed by `deps`; changing them refetches.
 * A 401 (ApiError status 401) is swallowed here because the API client already
 * routes the app back to the gate.
 */
export function useFetch<T>(
  loader: () => Promise<T>,
  deps: ReadonlyArray<unknown>,
  options: Options = {},
): FetchState<T> {
  const { intervalMs = 0, enabled = true } = options;
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState<boolean>(enabled);
  const [initialLoading, setInitialLoading] = useState<boolean>(enabled);
  const loaderRef = useRef(loader);
  loaderRef.current = loader;
  const cancelled = useRef(false);

  const run = useCallback(async () => {
    if (!enabled) return;
    setLoading(true);
    try {
      const result = await loaderRef.current();
      if (cancelled.current) return;
      setData(result);
      setError(null);
    } catch (e) {
      if (cancelled.current) return;
      if (e instanceof ApiError && e.status === 401) {
        // Handled globally by the gate; don't surface a banner.
        return;
      }
      setError(e instanceof Error ? e.message : 'Request failed');
    } finally {
      if (!cancelled.current) {
        setLoading(false);
        setInitialLoading(false);
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled]);

  useEffect(() => {
    cancelled.current = false;
    if (!enabled) {
      setLoading(false);
      setInitialLoading(false);
      return;
    }
    run();
    if (intervalMs > 0) {
      const id = window.setInterval(run, intervalMs);
      return () => {
        cancelled.current = true;
        window.clearInterval(id);
      };
    }
    return () => {
      cancelled.current = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, enabled, intervalMs]);

  return { data, loading, error, refetch: run, initialLoading };
}

/**
 * useLiveTick returns a unix-seconds timestamp that advances on `intervalMs`.
 * It updates immediately when the interval fires (not on every render), so it
 * can be used as a stable dependency that changes exactly once per tick. The
 * interval is cleared on unmount.
 */
export function useLiveTick(intervalMs: number = LIVE_REFRESH_MS): number {
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000));
  useEffect(() => {
    const id = window.setInterval(() => {
      setNow(Math.floor(Date.now() / 1000));
    }, intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs]);
  return now;
}
