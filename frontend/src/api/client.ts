import type {
  Breakdown,
  BreakdownDimension,
  Bucket,
  CallsFilters,
  CallsPage,
  CallEntry,
  CallTrace,
  Catalog,
  ContextComposition,
  CreateProviderBody,
  CreateProxyBody,
  CreateUserBody,
  ErrorBreakdown,
  ErrorCodesBreakdown,
  FeedPage,
  Me,
  Overview,
  PatchProviderBody,
  PatchProxyBody,
  PatchServiceProviderRoutingBody,
  PatchUserBody,
  PricingRow,
  Proxy,
  ProxyTestResult,
  Provider,
  Service,
  SessionContext,
  SessionDetail,
  SessionMessages,
  SessionStats,
  Settings,
  User,
  UsageFacets,
  UsageFilter,
  UsageSeries,
  UsageDimension,
  TokensByModelSeries,
  SuccessByModelSeries,
  CacheByModelSeries,
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

/**
 * Build a query string. An array value becomes one repeated param per entry
 * (`?models=a&models=b`) — the form the backend reads for the multi-value
 * filters, chosen because model ids and vendor names are free-form operator
 * input and a comma-joined list could not be split back apart safely. An empty
 * array emits nothing, so "all" costs nothing on the wire.
 */
function qs(
  params: Record<string, string | number | string[] | undefined | null>,
): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (Array.isArray(v)) {
      for (const item of v) if (item !== '') sp.append(k, item);
    } else if (v !== undefined && v !== null && v !== '') {
      sp.set(k, String(v));
    }
  }
  const s = sp.toString();
  return s ? `?${s}` : '';
}

/** Spread a UsageFilter into qs() params; an absent filter contributes nothing. */
function filterParams(
  f?: UsageFilter,
): { models?: string[]; vendors?: string[]; clients?: string[] } {
  return { models: f?.models, vendors: f?.vendors, clients: f?.clients };
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
    models: f.models,
    vendors: f.vendors,
    clients: f.clients,
    status: f.status && f.status !== 'all' ? f.status : undefined,
    sort: f.sort && f.sort !== 'recent' ? f.sort : undefined,
    limit: f.limit,
    offset: f.offset,
  });
}

export const api = {
  /** Whoami — succeeds for any valid key; `role` decides which shell to show. */
  me: () => request<Me>('/me'),

  settings: () => request<Settings>('/settings'),

  overview: (since: number, until: number, filter?: UsageFilter) =>
    request<Overview>(`/overview${qs({ since, until, ...filterParams(filter) })}`),

  /** Aggregate stats over coding-agent sessions in the window. Under a filter
   *  these are the sessions that *touched* a selected model, provider or client,
   *  with their whole-session figures — see the backend's SessionStats. */
  sessionsOverview: (since: number, until: number, filter?: UsageFilter) =>
    request<SessionStats>(`/sessions/overview${qs({ since, until, ...filterParams(filter) })}`),

  series: (since: number, until: number, bucket: Bucket) =>
    request<UsageSeries>(`/usage/series${qs({ since, until, bucket })}`),

  /** The models, providers and clients with traffic in the window, ranked by
   *  requests — the option lists for the three top-bar filters. Deliberately
   *  unfiltered by the current selection, so choosing a model never removes
   *  providers. The clients list holds only recognized clients, so it can be
   *  empty on a gateway that sees no coding-agent traffic. */
  facets: (since: number, until: number) =>
    request<UsageFacets>(`/usage/facets${qs({ since, until })}`),

  tokensByModel: (
    since: number,
    until: number,
    bucket: Bucket,
    dimension: UsageDimension = 'model',
    filter?: UsageFilter,
  ) =>
    request<TokensByModelSeries>(
      `/usage/tokens-by-model${qs({ since, until, bucket, dimension, ...filterParams(filter) })}`,
    ),

  successByModel: (
    since: number,
    until: number,
    bucket: Bucket,
    dimension: UsageDimension = 'model',
    filter?: UsageFilter,
  ) =>
    request<SuccessByModelSeries>(
      `/usage/success-by-model${qs({ since, until, bucket, dimension, ...filterParams(filter) })}`,
    ),

  cacheByModel: (
    since: number,
    until: number,
    bucket: Bucket,
    dimension: UsageDimension = 'model',
    filter?: UsageFilter,
  ) =>
    request<CacheByModelSeries>(
      `/usage/cache-by-model${qs({ since, until, bucket, dimension, ...filterParams(filter) })}`,
    ),

  breakdown: (dimension: BreakdownDimension, since: number, until: number) =>
    request<Breakdown>(`/usage/breakdown${qs({ dimension, since, until })}`),

  errors: (since: number, until: number) =>
    request<ErrorBreakdown>(`/usage/errors${qs({ since, until })}`),

  /**
   * Top upstream error codes (status → count, ranked). Optionally scoped to one
   * series via dimension+key (e.g. dimension='model', key=<model>) so the
   * Overview error-codes panel can filter to the clicked row.
   */
  errorCodes: (
    since: number,
    until: number,
    dimension?: UsageDimension,
    key?: string,
    filter?: UsageFilter,
  ) =>
    request<ErrorCodesBreakdown>(
      `/usage/error-codes${qs({
        since,
        until,
        dimension: key ? dimension : undefined,
        key,
        ...filterParams(filter),
      })}`,
    ),

  calls: (f: CallsFilters) => request<CallsPage>(`/calls${callsQuery(f)}`),

  /** Activity feed: one row per session (aggregated) or standalone request. */
  feed: (f: CallsFilters) => request<FeedPage>(`/feed${callsQuery(f)}`),

  /** Fetch a single call entry by id (UUID). 404 if absent. */
  call: (id: string) => request<CallEntry>(`/calls/${encodeURIComponent(id)}`),

  /** Fetch one session's rollups, agent tree, and calls. 404 if absent. */
  session: (id: string) => request<SessionDetail>(`/sessions/${encodeURIComponent(id)}`),

  /** Fetch compact prompt material reconstructed from captured session requests. */
  sessionMessages: (id: string) =>
    request<SessionMessages>(`/sessions/${encodeURIComponent(id)}/messages`),

  /** Aggregated context-window composition over a range (Overview sunburst). */
  contextComposition: (since: number, until: number, filter?: UsageFilter) =>
    request<ContextComposition>(
      `/context/composition${qs({ since, until, ...filterParams(filter) })}`,
    ),

  /** Per-turn context growth, snapshot, and dwell for one session, scoped to one
   *  agent (agent="" or omitted → the main loop). */
  sessionContext: (id: string, agent?: string) =>
    request<SessionContext>(`/sessions/${encodeURIComponent(id)}/context${qs({ agent: agent || undefined })}`),

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

  patchServiceProviderRouting: (providerId: string, body: PatchServiceProviderRoutingBody) =>
    request<void>(`/services/routing/${encodeURIComponent(providerId)}`, {
      method: 'PATCH',
      body: JSON.stringify(body),
    }),

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

  // --- Outbound proxies ---

  proxies: () => request<Proxy[]>('/proxies'),

  createProxy: (body: CreateProxyBody) =>
    request<Proxy>('/proxies', { method: 'POST', body: JSON.stringify(body) }),

  patchProxy: (id: string, body: PatchProxyBody) =>
    request<Proxy>(`/proxies/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(body),
    }),

  deleteProxy: (id: string) =>
    request<void>(`/proxies/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  testProxy: (id: string) =>
    request<ProxyTestResult>(`/proxies/${encodeURIComponent(id)}/test`, {
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
