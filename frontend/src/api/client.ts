import type {
  Breakdown,
  BreakdownDimension,
  CallsFilters,
  CallsPage,
  CallEntry,
  CallTrace,
  Catalog,
  ContextComposition,
  CreateProviderBody,
  CreateUserBody,
  ErrorBreakdown,
  FeedPage,
  Me,
  Overview,
  PatchProviderBody,
  PatchUserBody,
  PricingRow,
  Provider,
  Service,
  SessionContext,
  SessionDetail,
  SessionStats,
  Settings,
  User,
  UsageSeries,
  TokensByModelSeries,
  Vendor,
  VendorTestResult,
} from './types';

const KEY_STORAGE = 'songguo_admin_key';

/** ApiError carries the HTTP status so callers can branch on 401, etc. */
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = 'ApiError';
  }
}

type UnauthorizedListener = () => void;
const unauthorizedListeners = new Set<UnauthorizedListener>();

/** Subscribe to forced sign-outs triggered by a 401 from any request. */
export function onUnauthorized(fn: UnauthorizedListener): () => void {
  unauthorizedListeners.add(fn);
  return () => unauthorizedListeners.delete(fn);
}

export function getAdminKey(): string {
  try {
    return localStorage.getItem(KEY_STORAGE) ?? '';
  } catch {
    return '';
  }
}

export function setAdminKey(key: string): void {
  try {
    localStorage.setItem(KEY_STORAGE, key);
  } catch {
    /* ignore storage failures */
  }
}

export function clearAdminKey(): void {
  try {
    localStorage.removeItem(KEY_STORAGE);
  } catch {
    /* ignore */
  }
}

function authHeaders(): HeadersInit {
  const key = getAdminKey();
  return key ? { Authorization: `Bearer ${key}` } : {};
}

function handleUnauthorized(): void {
  clearAdminKey();
  for (const fn of unauthorizedListeners) fn();
}

function qs(params: Record<string, string | number | undefined | null>): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') sp.set(k, String(v));
  }
  const s = sp.toString();
  return s ? `?${s}` : '';
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(`/api${path}`, {
      ...init,
      headers: {
        ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
        ...authHeaders(),
        ...init?.headers,
      },
    });
  } catch (e) {
    throw new ApiError(0, e instanceof Error ? e.message : 'Network error');
  }

  if (res.status === 401) {
    handleUnauthorized();
    throw new ApiError(401, 'Unauthorized');
  }

  if (!res.ok) {
    let message = `Request failed (${res.status})`;
    try {
      const body = (await res.json()) as { error?: { message?: string } };
      if (body?.error?.message) message = body.error.message;
    } catch {
      /* keep default */
    }
    throw new ApiError(res.status, message);
  }

  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

function callsQuery(f: CallsFilters): string {
  return qs({
    since: f.since,
    until: f.until,
    user_id: f.user_id,
    model: f.model,
    vendor: f.vendor,
    status: f.status && f.status !== 'all' ? f.status : undefined,
    limit: f.limit,
    offset: f.offset,
  });
}

export const api = {
  /** Whoami — succeeds for any valid key; `role` decides which shell to show. */
  me: () => request<Me>('/me'),

  settings: () => request<Settings>('/settings'),

  overview: (since: number, until: number) =>
    request<Overview>(`/overview${qs({ since, until })}`),

  /** Aggregate stats over coding-agent sessions in the window. */
  sessionsOverview: (since: number, until: number) =>
    request<SessionStats>(`/sessions/overview${qs({ since, until })}`),

  series: (since: number, until: number, bucket: 'hour' | 'day') =>
    request<UsageSeries>(`/usage/series${qs({ since, until, bucket })}`),

  tokensByModel: (since: number, until: number, bucket: 'hour' | 'day') =>
    request<TokensByModelSeries>(`/usage/tokens-by-model${qs({ since, until, bucket })}`),

  breakdown: (dimension: BreakdownDimension, since: number, until: number) =>
    request<Breakdown>(`/usage/breakdown${qs({ dimension, since, until })}`),

  errors: (since: number, until: number) =>
    request<ErrorBreakdown>(`/usage/errors${qs({ since, until })}`),

  calls: (f: CallsFilters) => request<CallsPage>(`/calls${callsQuery(f)}`),

  /** Activity feed: one row per session (aggregated) or standalone request. */
  feed: (f: CallsFilters) => request<FeedPage>(`/feed${callsQuery(f)}`),

  /** Fetch a single call entry by id (UUID). 404 if absent. */
  call: (id: string) => request<CallEntry>(`/calls/${encodeURIComponent(id)}`),

  /** Fetch one session's rollups, agent tree, and calls. 404 if absent. */
  session: (id: string) => request<SessionDetail>(`/sessions/${encodeURIComponent(id)}`),

  /** Aggregated context-window composition over a range (Overview sunburst). */
  contextComposition: (since: number, until: number) =>
    request<ContextComposition>(`/context/composition${qs({ since, until })}`),

  /** Per-turn context growth, snapshot, and dwell for one session. */
  sessionContext: (id: string) =>
    request<SessionContext>(`/sessions/${encodeURIComponent(id)}/context`),

  /** Fetch the captured request/response trace for a call (UUID). 404 if none. */
  trace: (id: string) => request<CallTrace>(`/calls/${encodeURIComponent(id)}/trace`),

  users: () => request<User[]>('/users'),

  createUser: (body: CreateUserBody) =>
    request<User>('/users', { method: 'POST', body: JSON.stringify(body) }),

  patchUser: (id: string, body: PatchUserBody) =>
    request<User>(`/users/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(body),
    }),

  deleteUser: (id: string) =>
    request<void>(`/users/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  vendors: () => request<Vendor[]>('/vendors'),

  testVendor: (name: string) =>
    request<VendorTestResult>(`/vendors/${encodeURIComponent(name)}/test`, {
      method: 'POST',
    }),

  // --- Services (auto-derived, model-centric) ---

  services: () => request<Service[]>('/services'),

  // --- Providers (SQLite-backed upstream config) ---

  providers: () => request<Provider[]>('/providers'),

  createProvider: (body: CreateProviderBody) =>
    request<Provider>('/providers', { method: 'POST', body: JSON.stringify(body) }),

  patchProvider: (id: string, body: PatchProviderBody) =>
    request<Provider>(`/providers/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(body),
    }),

  deleteProvider: (id: string) =>
    request<void>(`/providers/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  testProvider: (id: string) =>
    request<VendorTestResult>(`/providers/${encodeURIComponent(id)}/test`, {
      method: 'POST',
    }),

  catalog: () => request<Catalog>('/catalog'),

  /** All registered wire names (for the provider form's allowlist picker). */
  wires: () => request<string[]>('/wires'),

  pricing: () => request<PricingRow[]>('/pricing'),

  /**
   * Download the calls export as a Blob using the auth header, then trigger a
   * browser save. A plain anchor href cannot carry the Authorization header.
   */
  async exportCalls(format: 'csv' | 'json', f: CallsFilters): Promise<void> {
    const query = callsQuery({ ...f, limit: undefined, offset: undefined });
    const sep = query ? '&' : '?';
    const res = await fetch(`/api/calls/export${query}${sep}format=${format}`, {
      headers: authHeaders(),
    });
    if (res.status === 401) {
      handleUnauthorized();
      throw new ApiError(401, 'Unauthorized');
    }
    if (!res.ok) throw new ApiError(res.status, `Export failed (${res.status})`);
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `calls.${format}`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  },
};
